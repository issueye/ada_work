// Package acphelper 是 `acp-helper`:把 ACP client 的文件/终端能力暴露成模型工具。
//
// 它不是第二条 fs/shell 通道:路径与命令由 ACP client 自己的策略裁决;
// 客户端未声明能力或没有连接时工具直接拒绝,不回退到本地 fs/shell。
package acphelper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
	acpseam "github.com/issueye/ada_go_v2/plugins/seam/acp"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// PluginName 是插件 id 与工具来源。
const PluginName = "acp-helper"

const (
	readTool  = "acp_read_file"
	writeTool = "acp_write_file"
	runTool   = "acp_run"
)

const (
	defaultTimeout = 120 * time.Second
	maxTimeout     = 600 * time.Second
	// maxOutputRunes 是返回给模型的终端输出上限;客户端自己的 outputByteLimit 仍然生效。
	maxOutputRunes = 32 << 10
)

type helper struct {
	reg *acpseam.Registry
}

// Plugin 返回 acp-helper:inject tools + acp,注册三个工具。
func Plugin() kernel.Plugin {
	return kernel.Func(PluginName, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*acpseam.Registry](ctx, acpseam.ServiceName)
		if err != nil {
			return nil, err
		}
		tl, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		h := &helper{reg: reg}
		defs := []tools.ToolDef{readDef(h), writeDef(h), runDef(h)}
		var disposers []kernel.Dispose
		for _, def := range defs {
			d, rerr := tl.Register(def)
			if rerr != nil {
				for _, prior := range disposers {
					_ = prior(context.Background())
				}
				return nil, rerr
			}
			disposers = append(disposers, d)
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
	}).WithInject("tools", acpseam.ServiceName)
}

func readDef(h *helper) tools.ToolDef {
	return tools.ToolDef{
		Name: readTool, Summary: "通过 ACP client 读取文本文件(客户端视角的绝对路径)",
		ReadOnly: true, Source: PluginName,
		Input: schema.FromMap(map[string]any{
			"type": "object", "required": []any{"path"},
			"properties": map[string]any{
				"path":  map[string]any{"type": "string", "description": "客户端视角的绝对路径"},
				"line":  map[string]any{"type": "integer", "minimum": 1, "description": "起始行(可选)"},
				"limit": map[string]any{"type": "integer", "minimum": 1, "description": "最多行数(可选)"},
			},
		}),
		Handler: h.read,
	}
}

func writeDef(h *helper) tools.ToolDef {
	return tools.ToolDef{
		Name: writeTool, Summary: "通过 ACP client 写入文本文件(客户端视角的绝对路径)",
		Source: PluginName,
		Input: schema.FromMap(map[string]any{
			"type": "object", "required": []any{"path", "content"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "客户端视角的绝对路径"},
				"content": map[string]any{"type": "string", "description": "完整文件内容"},
			},
		}),
		Handler: h.write,
	}
}

func runDef(h *helper) tools.ToolDef {
	return tools.ToolDef{
		Name: runTool, Summary: "在 ACP client 的终端里执行命令,等待退出并返回输出",
		Source: PluginName,
		Input: schema.FromMap(map[string]any{
			"type": "object", "required": []any{"command"},
			"properties": map[string]any{
				"command":         map[string]any{"type": "string", "description": "可执行文件名"},
				"args":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"cwd":             map[string]any{"type": "string", "description": "工作目录(可选)"},
				"timeoutSeconds":  map[string]any{"type": "integer", "minimum": 1, "maximum": 600},
				"outputByteLimit": map[string]any{"type": "integer", "minimum": 1},
			},
		}),
		Handler: h.run,
	}
}

func (h *helper) read(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
	client, res := h.client()
	if client == nil {
		return res, nil
	}
	if call.SessionID == "" {
		return denied("缺少会话上下文(sessionId)"), nil
	}
	if !client.Capabilities().ReadTextFile {
		return denied("ACP client 未声明 fs.readTextFile"), nil
	}
	var args struct {
		Path  string `json:"path"`
		Line  int    `json:"line,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return failed("参数解析失败: " + err.Error()), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return failed("path 不能为空"), nil
	}
	content, err := client.ReadTextFile(ctx, call.SessionID, args.Path, args.Line, args.Limit)
	if err != nil {
		return failed("读取失败: " + err.Error()), nil
	}
	return ok(clip(content)), nil
}

func (h *helper) write(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
	client, res := h.client()
	if client == nil {
		return res, nil
	}
	if call.SessionID == "" {
		return denied("缺少会话上下文(sessionId)"), nil
	}
	if !client.Capabilities().WriteTextFile {
		return denied("ACP client 未声明 fs.writeTextFile"), nil
	}
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return failed("参数解析失败: " + err.Error()), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return failed("path 不能为空"), nil
	}
	if err := client.WriteTextFile(ctx, call.SessionID, args.Path, args.Content); err != nil {
		return failed("写入失败: " + err.Error()), nil
	}
	return ok(fmt.Sprintf("已写入 %s(%d 字节)", args.Path, len(args.Content))), nil
}

func (h *helper) run(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
	client, res := h.client()
	if client == nil {
		return res, nil
	}
	if call.SessionID == "" {
		return denied("缺少会话上下文(sessionId)"), nil
	}
	if !client.Capabilities().Terminal {
		return denied("ACP client 未声明 terminal"), nil
	}
	var args struct {
		Command         string   `json:"command"`
		Args            []string `json:"args,omitempty"`
		Cwd             string   `json:"cwd,omitempty"`
		TimeoutSeconds  int      `json:"timeoutSeconds,omitempty"`
		OutputByteLimit int      `json:"outputByteLimit,omitempty"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return failed("参数解析失败: " + err.Error()), nil
	}
	if strings.TrimSpace(args.Command) == "" {
		return failed("command 不能为空"), nil
	}
	timeout := time.Duration(args.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	term, err := client.CreateTerminal(ctx, call.SessionID, acpseam.TerminalRequest{
		Command: args.Command, Args: args.Args, Cwd: args.Cwd,
		OutputByteLimit: args.OutputByteLimit,
	})
	if err != nil {
		return failed("创建终端失败: " + err.Error()), nil
	}
	// 调用方取消后仍要释放终端,所以清理用脱离取消的 ctx。
	cleanup := context.WithoutCancel(ctx)
	defer func() { _ = client.ReleaseTerminal(cleanup, call.SessionID, term) }()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status, err := client.WaitForExit(runCtx, call.SessionID, term)
	if err != nil {
		_ = client.KillTerminal(cleanup, call.SessionID, term)
		out, _ := client.TerminalOutput(cleanup, call.SessionID, term)
		if runCtx.Err() == context.DeadlineExceeded {
			return statusResult(tools.StatusTimeout,
				fmt.Sprintf("等待终端超时(%s)\n%s", timeout, clip(out.Output))), nil
		}
		return failed("等待终端失败: " + err.Error() + "\n" + clip(out.Output)), nil
	}
	out, oerr := client.TerminalOutput(ctx, call.SessionID, term)
	if oerr != nil {
		return failed("读取终端输出失败: " + oerr.Error()), nil
	}
	text := clip(out.Output)
	if out.Truncated {
		text += "\n(客户端已截断输出)"
	}
	if status.ExitCode != nil && *status.ExitCode != 0 {
		return statusResult(tools.StatusError, fmt.Sprintf("退出码 %d\n%s", *status.ExitCode, text)), nil
	}
	if status.Signal != nil && *status.Signal != "" {
		return statusResult(tools.StatusError, "被信号终止: "+*status.Signal+"\n"+text), nil
	}
	return ok(text), nil
}

// client 返回当前 ACP client;没有连接时用 StatusDenied 表达「能力不可用」。
func (h *helper) client() (acpseam.Client, tools.Result) {
	c, err := h.reg.Get()
	if err != nil {
		return nil, denied("ACP client 不可用: " + err.Error())
	}
	return c, tools.Result{}
}

func ok(text string) tools.Result     { return statusResult(tools.StatusOK, text) }
func denied(text string) tools.Result { return statusResult(tools.StatusDenied, text) }
func failed(text string) tools.Result { return statusResult(tools.StatusError, text) }

func statusResult(status tools.Status, text string) tools.Result {
	return tools.Result{Status: status, Content: []llm.Block{{Kind: llm.BlockText, Text: text}}}
}

// clip 按 rune 截断,避免切坏 UTF-8;超限时明确告诉模型输出不完整。
func clip(s string) string {
	r := []rune(s)
	if len(r) <= maxOutputRunes {
		return s
	}
	return string(r[:maxOutputRunes]) + "\n(输出已截断)"
}
