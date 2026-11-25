// Package toolhelper 是 `tool-helper`:把「发现工具」和「按名字调用工具」
// 暴露成模型工具。
//
// 它解决的是工具很多时的上下文问题:模型先 tool_find 找到候选,再用 tool_exec
// 只调用需要的那一个。两者都不是绕过工具策略的第二条路径——tool_exec 走的是
// 同一个 tools.Registry.Invoke,目标工具的 guard/approval/超时/caps 全部照常执行。
package toolhelper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// PluginName 是插件 id 与工具来源。
const PluginName = "tool-helper"

// 工具名。
const (
	findTool = "tool_find"
	execTool = "tool_exec"
)

// 配置与限制。
type config struct {
	// Deny 是禁止被 tool_exec 调用的工具名;缺省拒绝 tool_exec 自身。
	Deny []string `json:"deny,omitempty"`
	// MaxDepth 是 tool_exec 的嵌套深度(1 表示不允许再嵌套 tool_exec)。
	MaxDepth int `json:"maxDepth,omitempty"`
	// DefaultLimit 是 tool_find 不传 limit 时返回的条数。
	DefaultLimit int `json:"defaultLimit,omitempty"`
}

const (
	defaultMaxDepth    = 1
	defaultFindLimit   = 20
	maxFindLimit       = 100
	maxExecTimeoutMs   = 600000
	maxNestedExecDepth = 5
	defaultCallTimeout = 30 * time.Second
	execOriginMarker   = "tool_exec"
)

// helper 是插件实例;它只持有 tools 注册表与归一化后的限制。
type helper struct {
	reg          *tools.Registry
	deny         map[string]bool
	maxDepth     int
	defaultLimit int
}

// depthKey 用私有 context key 记录嵌套深度,不污染 CallInfo 的公开字段。
type depthKey struct{}

// Plugin 返回 tool-helper 插件:inject tools,注册 tool_find / tool_exec。
func Plugin() kernel.Plugin {
	return kernel.Func(PluginName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "tool-helper: 解析配置失败: %v", err)
		}
		h := newHelper(registry, conf)
		h.deny[execTool] = true // 防止直接递归:即使配置漏写,也不能自己调自己。
		for _, name := range conf.Deny {
			if name = strings.TrimSpace(name); name != "" {
				h.deny[name] = true
			}
		}

		defs := []tools.ToolDef{h.findDef(), h.execDef()}
		var disposers []kernel.Dispose
		for _, def := range defs {
			disp, rerr := registry.Register(def)
			if rerr != nil {
				for _, prior := range disposers {
					_ = prior(context.Background())
				}
				return nil, rerr
			}
			disposers = append(disposers, disp)
		}
		return func(c context.Context) error {
			var first error
			for i := len(disposers) - 1; i >= 0; i-- {
				if err := disposers[i](c); err != nil && first == nil {
					first = err
				}
			}
			return first
		}, nil
	}).WithInject("tools")
}

func newHelper(reg *tools.Registry, conf config) *helper {
	h := &helper{reg: reg, deny: map[string]bool{}}
	h.maxDepth = conf.MaxDepth
	if h.maxDepth <= 0 {
		h.maxDepth = defaultMaxDepth
	}
	if h.maxDepth > maxNestedExecDepth {
		h.maxDepth = maxNestedExecDepth
	}
	h.defaultLimit = conf.DefaultLimit
	if h.defaultLimit <= 0 {
		h.defaultLimit = defaultFindLimit
	}
	if h.defaultLimit > maxFindLimit {
		h.defaultLimit = maxFindLimit
	}
	return h
}

func (h *helper) findDef() tools.ToolDef {
	return tools.ToolDef{
		Name:     findTool,
		Summary:  "按名字/摘要查找可用工具;工具很多时先用它缩小范围",
		ReadOnly: true,
		Timeout:  defaultCallTimeout,
		Source:   PluginName,
		Input: schema.FromMap(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "按工具名/摘要做不区分大小写的子串匹配;留空列出全部",
				},
				"limit": map[string]any{
					"type": "integer", "minimum": 1, "maximum": maxFindLimit,
					"description": "返回条数上限,缺省 " + fmt.Sprint(defaultFindLimit),
				},
				"verbose": map[string]any{
					"type":        "boolean",
					"description": "是否附带输入 schema",
				},
			},
		}),
		Handler: h.find,
	}
}

func (h *helper) execDef() tools.ToolDef {
	return tools.ToolDef{
		Name:     execTool,
		Summary:  "按名字调用另一个工具;目标工具的参数放在 args 对象里",
		ReadOnly: false,
		Timeout:  defaultCallTimeout,
		Source:   PluginName,
		Input: schema.FromMap(map[string]any{
			"type":     "object",
			"required": []any{"name"},
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "目标工具名;可先用 tool_find 查询",
				},
				"args": map[string]any{
					"type":        "object",
					"description": "目标工具的参数对象,默认 {}",
				},
				"timeoutMs": map[string]any{
					"type": "integer", "minimum": 1, "maximum": maxExecTimeoutMs,
					"description": "本次嵌套调用的附加超时上限(毫秒)",
				},
			},
		}),
		Handler: h.exec,
	}
}

// find 列出未受限的工具;query 同时匹配名字、摘要与标签。
func (h *helper) find(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
	var args struct {
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
		Verbose bool   `json:"verbose"`
	}
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return toolErr("tool_find 参数解析失败: " + err.Error()), nil
		}
	}
	if args.Limit <= 0 {
		args.Limit = h.defaultLimit
	}
	if args.Limit > maxFindLimit {
		args.Limit = maxFindLimit
	}
	list, err := h.reg.List(ctx)
	if err != nil {
		return toolErr("列出工具失败: " + err.Error()), nil
	}
	query := strings.ToLower(strings.TrimSpace(args.Query))
	matched := make([]tools.ToolInfo, 0, len(list))
	for _, info := range list {
		if query == "" || toolMatches(info, query) {
			matched = append(matched, info)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Name < matched[j].Name })
	if len(matched) == 0 {
		return textResult("没有匹配的工具(查询: " + args.Query + ")"), nil
	}
	shown := matched
	if len(shown) > args.Limit {
		shown = shown[:args.Limit]
	}

	var b strings.Builder
	if query == "" {
		b.WriteString(fmt.Sprintf("共 %d 个可用工具", len(matched)))
	} else {
		b.WriteString(fmt.Sprintf("查询 %q 命中 %d 个工具", args.Query, len(matched)))
	}
	if len(shown) < len(matched) {
		b.WriteString(fmt.Sprintf(",显示前 %d 个", len(shown)))
	}
	b.WriteString(":\n")
	for _, info := range shown {
		readOnly := "写"
		if info.ReadOnly {
			readOnly = "只读"
		}
		b.WriteString("- " + info.Name + "(" + readOnly + ")")
		if info.Summary != "" {
			b.WriteString(": " + info.Summary)
		}
		b.WriteString("\n")
		if args.Verbose && info.Input != nil {
			raw, merr := json.Marshal(info.Input.Raw())
			if merr == nil {
				b.WriteString("  input: " + string(raw) + "\n")
			}
		}
	}
	return textResult(strings.TrimRight(b.String(), "\n")), nil
}

func toolMatches(info tools.ToolInfo, query string) bool {
	if strings.Contains(strings.ToLower(info.Name), query) ||
		strings.Contains(strings.ToLower(info.Summary), query) {
		return true
	}
	for _, tag := range info.Tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	return false
}

// exec 通过同一个注册表调用目标工具。目标工具不是「被执行」到模型面前就结束:
// 它自己的守卫、审批、超时、cap 与再套一层 tool_exec 的深度限制都仍然生效。
func (h *helper) exec(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
	var args struct {
		Name      string          `json:"name"`
		Args      json.RawMessage `json:"args"`
		TimeoutMs int64           `json:"timeoutMs"`
	}
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return toolErr("tool_exec 参数解析失败: " + err.Error()), nil
		}
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return toolErr("tool_exec 需要非空的 name"), nil
	}
	if h.deny[name] {
		return toolErr("工具 " + name + " 不允许被 tool_exec 调用"), nil
	}
	depth, _ := ctx.Value(depthKey{}).(int)
	if depth >= h.maxDepth {
		return toolErr(fmt.Sprintf("tool_exec 嵌套深度超过上限(%d),拒绝继续", h.maxDepth)), nil
	}
	innerArgs := args.Args
	if len(innerArgs) == 0 || string(innerArgs) == "null" {
		innerArgs = json.RawMessage(`{}`)
	}
	innerCtx := context.WithValue(ctx, depthKey{}, depth+1)
	var cancel context.CancelFunc
	if args.TimeoutMs > 0 {
		if args.TimeoutMs > maxExecTimeoutMs {
			args.TimeoutMs = maxExecTimeoutMs
		}
		innerCtx, cancel = context.WithTimeout(innerCtx, time.Duration(args.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	res, err := h.reg.Invoke(innerCtx, tools.CallInfo{
		Name:      name,
		Args:      innerArgs,
		SessionID: call.SessionID,
		TurnID:    call.TurnID,
		StepID:    call.StepID,
		Origin:    execOriginMarker,
	})
	if err != nil {
		return toolErr("调用工具 " + name + " 失败: " + err.Error()), nil
	}
	if len(res.Content) == 0 {
		res.Content = []llm.Block{{Kind: llm.BlockText, Text: "(目标工具没有输出)"}}
	}
	return res, nil
}

func textResult(text string) tools.Result {
	return tools.Result{Status: tools.StatusOK, Content: []llm.Block{{Kind: llm.BlockText, Text: text}}}
}

func toolErr(msg string) tools.Result {
	return tools.Result{Status: tools.StatusError, Content: []llm.Block{{Kind: llm.BlockText, Text: msg}}}
}
