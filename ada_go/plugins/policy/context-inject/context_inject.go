// Package contextinject 是 `policy/context-inject`:把工作区里的说明文件
// (AGENTS.md / README.md / 自定义文件)拼成一段「工作区上下文」,在每次模型调用前
// 追加到 system prompt。
//
// 它是改写型策略,不是门禁:注入失败(文件不存在、被 fs 策略拒绝)只是跳过,
// 不能让整轮对话失败;真正的安全边界仍由 fs-sandbox / caps 等装饰器负责。
//
// 注入走 llm/request waterfall(seam-api §3.2 允许策略改写请求),不往 session
// 里写东西:上下文是派生数据,随文件变化而变化,不该成为会话事实。
package contextinject

import (
	"context"
	"fmt"
	"strings"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamllm "github.com/issueye/ada_go_v2/plugins/seam/llm"
)

// PolicyName 是插件 id。
const PolicyName = "context-inject"

// Priority 是请求改写优先级:先注入上下文,再交给其它请求策略。
const Priority = 200

const (
	defaultTitle    = "工作区上下文"
	titlePrefix     = "## "
	fileHeader      = "### "
	defaultMaxBytes = 32 << 10 // 32 KiB:够放 AGENTS/README,又不至于把 prompt 预算吃光
	minMaxBytes     = 256
	truncatedNote   = "\n…（上下文已截断）"
)

type config struct {
	Title    string   `json:"title,omitempty"`
	Files    []string `json:"files,omitempty"`
	Text     string   `json:"text,omitempty"`
	MaxBytes int      `json:"maxBytes,omitempty"`
}

type injector struct {
	chain    *seamfs.Chain
	title    string
	files    []string
	text     string
	maxBytes int
}

// Plugin 返回 context-inject 插件:可选注入 fs,监听 llm/request。
//
// fs 用依赖注入拿到的是 *seamfs.Chain,因此读取会经过 fs-sandbox / caps 等策略;
// 没有 fs 服务时只注入静态 Text。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "context-inject: 解析配置失败: %v", err)
		}
		inj := &injector{
			title:    strings.TrimSpace(conf.Title),
			text:     strings.TrimSpace(conf.Text),
			files:    normalizeFiles(conf.Files),
			maxBytes: conf.MaxBytes,
		}
		if inj.title == "" {
			inj.title = defaultTitle
		}
		if inj.maxBytes <= 0 {
			inj.maxBytes = defaultMaxBytes
		}
		if inj.maxBytes < minMaxBytes {
			inj.maxBytes = minMaxBytes
		}
		if v, ok := ctx.TryService("fs"); ok {
			inj.chain, _ = v.(*seamfs.Chain)
		}
		return ctx.OnWaterfall(seamllm.EventRequest, inj.handle, kernel.WithPriority(Priority)), nil
	}).WithOptional("fs")
}

// normalizeFiles 去空白、去重;路径保持调用方写法,/workspace 前缀在读取时补。
func normalizeFiles(files []string) []string {
	out := make([]string, 0, len(files))
	seen := map[string]bool{}
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// handle 在模型调用前把上下文追加进 system prompt。
func (i *injector) handle(ctx context.Context, _ *kernel.Context, input any, next kernel.Next) (any, error) {
	req, ok := input.(*seamllm.Request)
	if !ok || req == nil {
		return next(input)
	}
	body := i.build(ctx)
	if body == "" {
		return next(input)
	}
	req.Messages = mergeSystem(req.Messages, body)
	return next(input)
}

// build 汇总静态文本与文件内容;文件读不到就跳过(注入不应让整轮失败)。
func (i *injector) build(ctx context.Context) string {
	var body strings.Builder
	if i.text != "" {
		body.WriteString(i.text)
		body.WriteString("\n")
	}
	if i.chain != nil && len(i.files) > 0 {
		fsys, err := i.chain.Get()
		if err == nil {
			for _, f := range i.files {
				res, rerr := fsys.Read(ctx, seamfs.ReadRequest{Path: seamfs.Path(workspacePath(f))})
				if rerr != nil {
					continue // 不存在/被策略拒绝:跳过,不要往对话里塞错误
				}
				text := strings.TrimSpace(string(res.Content))
				if text == "" {
					continue
				}
				fmt.Fprintf(&body, "%s%s\n%s\n\n", fileHeader, displayPath(f), text)
			}
		}
	}
	content := strings.TrimSpace(body.String())
	if content == "" {
		return ""
	}
	text := titlePrefix + i.title + "\n" + content
	return truncate(text, i.maxBytes)
}

// workspacePath 把相对路径补成 fs seam 的 /workspace/...
func workspacePath(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/workspace/" + strings.TrimPrefix(p, "./")
}

// displayPath 是文件头里显示的路径(去掉 /workspace 前缀,保持简短)。
func displayPath(p string) string {
	p = strings.TrimPrefix(p, "/workspace/")
	return strings.TrimPrefix(p, "./")
}

// mergeSystem 把上下文并进第一条 system 消息;没有 system 消息时新建一条。
//
// 并进同一条而不是再插一条:有的 OpenAI 兼容端点对多条 system 的拼接方式不一致,
// 单条 system 是各家都认的形态。
func mergeSystem(msgs []seamllm.Message, text string) []seamllm.Message {
	for idx := range msgs {
		if msgs[idx].Role != seamllm.RoleSystem {
			continue
		}
		msgs[idx].Content = append(msgs[idx].Content, seamllm.Block{Kind: seamllm.BlockText, Text: "\n\n" + text})
		return msgs
	}
	head := seamllm.Message{
		Role:    seamllm.RoleSystem,
		Content: []seamllm.Block{{Kind: seamllm.BlockText, Text: text}},
	}
	return append([]seamllm.Message{head}, msgs...)
}

// truncate 按字节截断,保留可读的结尾说明。
func truncate(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	cut := max - len(truncatedNote)
	if cut < 0 {
		cut = 0
	}
	return text[:cut] + truncatedNote
}
