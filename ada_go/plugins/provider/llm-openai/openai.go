// Package openai 是 `provider/llm-openai`:OpenAI 兼容的 Chat Completions 实现。
//
// 它只做一次调用:重试/路由/裁剪属于 policy;密钥优先从 seam/secrets 取,
// 没有 secrets 服务时才退回环境变量(本地开发/真实端点测试用)。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
)

// ProviderName 是 provider 与插件 id。
const ProviderName = "llm-openai"

// DefaultEndpoint 是 OpenAI 官方端点;本地兼容端点通过配置覆盖。
const DefaultEndpoint = "https://api.openai.com/v1"

// DefaultModel 是未配置模型时的兜底。
const DefaultModel = "gpt-4o-mini"

// DefaultTimeout 是单次请求默认超时。
const DefaultTimeout = 120 * time.Second

// Options 构造 provider。
type Options struct {
	Endpoint  string
	Model     string
	Models    []llm.ModelInfo
	APIKey    string
	Timeout   time.Duration
	MaxTokens int
}

// Provider 是 OpenAI 兼容实现。
type Provider struct {
	endpoint  string
	model     string
	models    []llm.ModelInfo
	apiKey    string
	maxTokens int
	http      *http.Client
}

// New 构造 provider。
func New(opts Options) (*Provider, error) {
	endpoint := strings.TrimRight(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	p := &Provider{
		endpoint: endpoint, model: model,
		models: opts.Models, apiKey: opts.APIKey,
		maxTokens: opts.MaxTokens,
		http:      &http.Client{Timeout: timeout},
	}
	if len(p.models) == 0 {
		p.models = []llm.ModelInfo{{ID: model, ContextWindow: 8192}}
	}
	return p, nil
}

// Name 返回 provider 名。
func (p *Provider) Name() string { return ProviderName }

// Version 返回契约主版本。
func (p *Provider) Version() int { return 1 }

// Models 返回配置的模型。
func (p *Provider) Models(ctx context.Context) ([]llm.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "列模型前已取消", err)
	}
	out := make([]llm.ModelInfo, len(p.models))
	copy(out, p.models)
	return out, nil
}

// Complete 非流式补全。
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	return p.do(ctx, req, false, nil)
}

// Stream 流式补全;SSE 增量经 onDelta 回调。
func (p *Provider) Stream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (llm.Response, error) {
	if onDelta == nil {
		return llm.Response{}, seamerr.New(seamerr.ErrInvalid, ProviderName, "Stream 需要 onDelta", false)
	}
	return p.do(ctx, req, true, onDelta)
}

func (p *Provider) do(ctx context.Context, req llm.Request, stream bool, onDelta func(llm.Delta) error) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "请求前已取消", err)
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	wire := wireRequest{
		Model:       model,
		Messages:    toWireMessages(req.Messages),
		Tools:       toWireTools(req.Tools),
		ToolChoice:  toWireChoice(req.ToolChoice),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		Stream:      stream,
	}
	if wire.MaxTokens <= 0 {
		wire.MaxTokens = p.maxTokens
	}
	if stream {
		wire.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return llm.Response{}, seamerr.Wrap(seamerr.ErrInvalid, ProviderName, "序列化请求失败", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, seamerr.Wrap(seamerr.ErrInvalid, ProviderName, "构造请求失败", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return llm.Response{}, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "连接模型端点失败", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return llm.Response{}, statusError(resp.StatusCode, string(data))
	}
	if stream {
		return p.readStream(ctx, resp.Body, onDelta)
	}
	return p.readComplete(resp.Body)
}

func (p *Provider) readComplete(body io.Reader) (llm.Response, error) {
	var wr wireResponse
	if err := json.NewDecoder(body).Decode(&wr); err != nil {
		return llm.Response{}, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "解析响应失败", err)
	}
	if len(wr.Choices) == 0 {
		return llm.Response{}, seamerr.New(seamerr.ErrUnavailable, ProviderName, "响应没有 choices", false)
	}
	return fromWireChoice(wr.Choices[0], wr.Usage)
}

func (p *Provider) readStream(ctx context.Context, body io.Reader, onDelta func(llm.Delta) error) (llm.Response, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 2<<20)
	var content, reasoning strings.Builder
	calls := map[int]*wireToolCall{}
	var usage *llm.Usage
	finish := ""
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return llm.Response{}, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "流式读取被取消", err)
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk wireChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return llm.Response{}, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "解析流式块失败", err)
		}
		if chunk.Usage != nil {
			u := usageFromWire(*chunk.Usage)
			usage = &u
			if err := onDelta(llm.Delta{Kind: llm.DeltaUsage, Usage: usage}); err != nil {
				return llm.Response{}, err
			}
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			if ch.Delta.ReasoningContent != "" {
				reasoning.WriteString(ch.Delta.ReasoningContent)
				if err := onDelta(llm.Delta{Kind: llm.DeltaReasoning, Text: ch.Delta.ReasoningContent}); err != nil {
					return llm.Response{}, err
				}
			}
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
				if err := onDelta(llm.Delta{Kind: llm.DeltaText, Text: ch.Delta.Content}); err != nil {
					return llm.Response{}, err
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				cur := calls[tc.Index]
				if cur == nil {
					cur = &wireToolCall{Type: "function"}
					calls[tc.Index] = cur
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Function.Name += tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					cur.Function.Arguments += tc.Function.Arguments
				}
				if err := onDelta(llm.Delta{
					Kind: llm.DeltaToolArgs, ToolIndex: tc.Index, ToolID: cur.ID,
					Text: tc.Function.Arguments,
				}); err != nil {
					return llm.Response{}, err
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return llm.Response{}, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "读取流式响应失败", err)
	}
	if err := onDelta(llm.Delta{Kind: llm.DeltaDone}); err != nil {
		return llm.Response{}, err
	}

	msg := llm.Message{Role: llm.RoleAssistant}
	if content.Len() > 0 {
		msg.Content = append(msg.Content, llm.Block{Kind: llm.BlockText, Text: content.String()})
	}
	if reasoning.Len() > 0 {
		msg.Content = append(msg.Content, llm.Block{Kind: llm.BlockReasoning, Text: reasoning.String()})
	}
	toolCalls, err := fromWireCalls(calls)
	if err != nil {
		return llm.Response{}, err
	}
	msg.ToolCalls = toolCalls
	u := llm.Usage{}
	if usage != nil {
		u = *usage
	}
	return llm.Response{Message: msg, ToolCalls: toolCalls, Usage: u, Finish: finishReason(finish)}, nil
}

// 下面是 OpenAI 兼容的 wire 类型(只覆盖用到的子集)。
type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type wireToolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

type wireResponse struct {
	Choices []wireChoice `json:"choices"`
	Usage   wireUsage    `json:"usage"`
}

type wireChoice struct {
	Message      wireMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type wireChunk struct {
	Choices []wireChunkChoice `json:"choices"`
	Usage   *wireUsage        `json:"usage"`
}

type wireChunkChoice struct {
	Delta        wireDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

type wireDelta struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

func toWireMessages(msgs []llm.Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		wm := wireMessage{Role: string(m.Role)}
		var parts []string
		for _, b := range m.Content {
			if b.Kind == llm.BlockText {
				parts = append(parts, b.Text)
			}
		}
		wm.Content = strings.Join(parts, "\n")
		if len(m.ToolCalls) > 0 {
			wm.ToolCalls = make([]wireToolCall, 0, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				args := string(tc.Args)
				if args == "" {
					args = "{}"
				}
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					Index: i, ID: tc.ID, Type: "function",
					Function: wireFunction{Name: tc.Name, Arguments: args},
				})
			}
		}
		if m.Role == llm.RoleTool {
			for _, b := range m.Content {
				if b.Meta != nil && b.Meta["toolCallId"] != "" {
					wm.ToolCallID = b.Meta["toolCallId"]
					break
				}
			}
		}
		out = append(out, wm)
	}
	return out
}

func toWireTools(in []llm.ToolSchema) []wireTool {
	if len(in) == 0 {
		return nil
	}
	out := make([]wireTool, 0, len(in))
	for _, t := range in {
		params := t.Input
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, wireTool{Type: "function", Function: wireFunction{
			Name: t.Name, Description: t.Description, Parameters: params,
		}})
	}
	return out
}

func toWireChoice(c llm.ToolChoice) any {
	switch c.Mode {
	case "", "auto":
		return nil
	case "none":
		return "none"
	case "required":
		return "required"
	case "name", "function":
		if c.Name == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]string{"name": c.Name}}
	default:
		return nil
	}
}

func fromWireChoice(ch wireChoice, usage wireUsage) (llm.Response, error) {
	msg := llm.Message{Role: llm.RoleAssistant}
	if ch.Message.Content != "" {
		msg.Content = append(msg.Content, llm.Block{Kind: llm.BlockText, Text: ch.Message.Content})
	}
	if ch.Message.ReasoningContent != "" {
		msg.Content = append(msg.Content, llm.Block{Kind: llm.BlockReasoning, Text: ch.Message.ReasoningContent})
	}
	calls := make([]llm.ToolCall, 0, len(ch.Message.ToolCalls))
	for _, tc := range ch.Message.ToolCalls {
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		if !json.Valid(args) {
			return llm.Response{}, seamerr.New(seamerr.ErrSchema, ProviderName, "工具参数不是合法 JSON: "+tc.Function.Name, false)
		}
		calls = append(calls, llm.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	msg.ToolCalls = calls
	return llm.Response{
		Message: msg, ToolCalls: calls, Usage: usageFromWire(usage),
		Finish: finishReason(ch.FinishReason),
	}, nil
}

func fromWireCalls(calls map[int]*wireToolCall) ([]llm.ToolCall, error) {
	out := make([]llm.ToolCall, 0, len(calls))
	for i := 0; i < len(calls); i++ {
		tc := calls[i]
		if tc == nil {
			continue
		}
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		if !json.Valid(args) {
			return nil, seamerr.New(seamerr.ErrSchema, ProviderName, "工具参数不是合法 JSON: "+tc.Function.Name, false)
		}
		out = append(out, llm.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	return out, nil
}

func usageFromWire(u wireUsage) llm.Usage {
	return llm.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, CacheRead: u.CachedTokens}
}

func finishReason(s string) llm.FinishReason {
	switch s {
	case "stop":
		return llm.FinishStop
	case "length":
		return llm.FinishLength
	case "tool_calls", "function_call":
		return llm.FinishToolCalls
	case "content_filter":
		return llm.FinishContentFilter
	case "":
		return ""
	default:
		return llm.FinishError
	}
}

func statusError(status int, body string) error {
	body = strings.TrimSpace(body)
	if len(body) > 512 {
		body = body[:512]
	}
	msg := fmt.Sprintf("模型端点返回 HTTP %d: %s", status, body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return seamerr.New(seamerr.ErrAuth, ProviderName, msg, false)
	case status == http.StatusTooManyRequests:
		return seamerr.New(seamerr.ErrRateLimit, ProviderName, msg, true)
	case status >= 500:
		return seamerr.New(seamerr.ErrUnavailable, ProviderName, msg, true)
	case status == http.StatusBadRequest && strings.Contains(strings.ToLower(body), "context"):
		return seamerr.New(seamerr.ErrContext, ProviderName, msg, false)
	default:
		return seamerr.New(seamerr.ErrInvalid, ProviderName, msg, false)
	}
}

type config struct {
	Endpoint   string          `json:"endpoint,omitempty"`
	Model      string          `json:"model,omitempty"`
	Models     []llm.ModelInfo `json:"models,omitempty"`
	APIKeyName string          `json:"apiKeyName,omitempty"`
	APIKeyEnv  string          `json:"apiKeyEnv,omitempty"`
	TimeoutMs  int             `json:"timeoutMs,omitempty"`
	MaxTokens  int             `json:"maxTokens,omitempty"`
}

// Plugin 返回 llm-openai 插件:inject llm seam,可选 secrets,注册 provider。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*llm.Registry](ctx, "llm")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "llm-openai: 解析配置失败: %v", err)
		}
		key, err := resolveKey(ctx, conf)
		if err != nil {
			return nil, err
		}
		p, err := New(Options{
			Endpoint: conf.Endpoint, Model: conf.Model, Models: conf.Models,
			APIKey: key, Timeout: time.Duration(conf.TimeoutMs) * time.Millisecond,
			MaxTokens: conf.MaxTokens,
		})
		if err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "llm-openai: %v", err)
		}
		return registry.Register(p)
	}).WithInject("llm").WithOptional("secrets")
}

// resolveKey 优先用 secrets 服务,再退回环境变量;两者都没有时返回空 key(本地端点可无鉴权)。
func resolveKey(ctx *kernel.Context, conf config) (string, error) {
	name := conf.APIKeyName
	if name != "" {
		if err := seamsecrets.ValidateName(name); err != nil {
			return "", kernel.Fail(kernel.CodeInvalid, "llm-openai: apiKeyName 非法: %v", err)
		}
		if v, ok := ctx.TryService("secrets"); ok {
			if chain, ok := v.(*seamsecrets.Chain); ok {
				getter, err := chain.Get()
				if err != nil {
					return "", err
				}
				key, err := getter.Get(ctx.Context(), name)
				if err != nil {
					return "", err
				}
				return key, nil
			}
		}
	}
	env := conf.APIKeyEnv
	if env == "" {
		env = "ADA_LLM_API_KEY"
	}
	return os.Getenv(env), nil
}
