// Package replay 是 `provider/llm-replay`:用录制的步骤回放模型输出。
//
// 它让契约测试与 golden 不依赖网络/密钥;每次 Complete/Stream 消耗一步,
// 一步可以带显式 Deltas,也可以由 Response 自动拆成文本增量。
package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ProviderName 是 provider 与插件 id。
const ProviderName = "llm-replay"

// Step 是一步回放。
type Step struct {
	Response llm.Response `json:"response"`
	Deltas   []llm.Delta  `json:"deltas,omitempty"`
}

// Provider 是回放实现。
type Provider struct {
	mu       sync.Mutex
	steps    []Step
	cursor   int
	models   []llm.ModelInfo
	defModel string
}

// New 构造回放 provider;steps 为空时调用会返回 EUNAVAILABLE。
func New(steps []Step, models []llm.ModelInfo, defaultModel string) *Provider {
	if defaultModel == "" && len(models) > 0 {
		defaultModel = models[0].ID
	}
	return &Provider{steps: steps, models: models, defModel: defaultModel}
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

// Complete 消耗一步并返回完整响应。
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	step, err := p.next(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	return step.Response, nil
}

// Stream 消耗一步并逐块回调;显式 Deltas 优先,否则把文本拆成三段。
func (p *Provider) Stream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (llm.Response, error) {
	step, err := p.next(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	deltas := step.Deltas
	if len(deltas) == 0 {
		deltas = synthesize(step.Response)
	}
	for _, d := range deltas {
		if err := ctx.Err(); err != nil {
			return llm.Response{}, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "流式回放被取消", err)
		}
		if err := onDelta(d); err != nil {
			return llm.Response{}, err
		}
	}
	return step.Response, nil
}

func (p *Provider) next(ctx context.Context) (Step, error) {
	if err := ctx.Err(); err != nil {
		return Step{}, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "回放前已取消", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cursor >= len(p.steps) {
		return Step{}, seamerr.New(seamerr.ErrUnavailable, ProviderName,
			fmt.Sprintf("回放脚本已用完(%d 步)", len(p.steps)), false)
	}
	step := p.steps[p.cursor]
	p.cursor++
	return step, nil
}

func synthesize(resp llm.Response) []llm.Delta {
	text := resp.Message.Text()
	if text == "" {
		if len(resp.ToolCalls) > 0 {
			return []llm.Delta{{Kind: llm.DeltaToolArgs, ToolIndex: 0, ToolID: resp.ToolCalls[0].ID}, {Kind: llm.DeltaDone}}
		}
		return []llm.Delta{{Kind: llm.DeltaDone}}
	}
	n := utf8.RuneCountInString(text)
	part := (n + 2) / 3
	if part < 1 {
		part = 1
	}
	runes := []rune(text)
	var out []llm.Delta
	for i := 0; i < len(runes); i += part {
		end := i + part
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, llm.Delta{Kind: llm.DeltaText, Text: string(runes[i:end])})
	}
	out = append(out, llm.Delta{Kind: llm.DeltaDone})
	return out
}

// LoadJSONL 从一行一步的 JSONL 读取脚本。
func LoadJSONL(path string) ([]Step, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("llm-replay: 打开脚本失败: %w", err)
	}
	defer f.Close()
	var steps []Step
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var s Step
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("llm-replay: 解析第 %d 行失败: %w", len(steps)+1, err)
		}
		steps = append(steps, s)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("llm-replay: 读取脚本失败: %w", err)
	}
	return steps, nil
}

type config struct {
	Path         string          `json:"path,omitempty"`
	Steps        []Step          `json:"steps,omitempty"`
	Models       []llm.ModelInfo `json:"models,omitempty"`
	DefaultModel string          `json:"defaultModel,omitempty"`
}

// Plugin 返回 llm-replay 插件:inject llm seam,注册回放 provider。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*llm.Registry](ctx, "llm")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "llm-replay: 解析配置失败: %v", err)
		}
		steps := conf.Steps
		if conf.Path != "" {
			steps, err = LoadJSONL(conf.Path)
			if err != nil {
				return nil, err
			}
		}
		return registry.Register(New(steps, conf.Models, conf.DefaultModel))
	}).WithInject("llm")
}
