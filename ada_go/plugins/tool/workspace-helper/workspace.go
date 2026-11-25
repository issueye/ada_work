// Package toolworkspace 是 `workspace-helper`:把工作区查询暴露成只读模型工具。
//
// 它不提供切换工具:工作区根属于宿主选择,模型只能查询自己所在的工作区。
// 工具结果只包含 Info,绝不包含 Target.Host。
package toolworkspace

import (
	"context"
	"encoding/json"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// PluginName 是插件 id 与工具来源。
const PluginName = "workspace-helper"

const (
	infoTool = "workspace_info"
	listTool = "workspace_list"
)

// infoOutput 是 workspace_info 的稳定输出;binding 只在会话有绑定的时候出现。
type infoOutput struct {
	Binding   string         `json:"binding,omitempty"`
	Current   workspace.Info `json:"current"`
	Workspace workspace.Info `json:"workspace"`
}

// Plugin 返回 workspace-helper 插件:inject tools 与 workspace,注册两个只读工具。
func Plugin() kernel.Plugin {
	return kernel.Func(PluginName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		chain, err := kernel.Service[*workspace.Chain](ctx, workspace.ServiceName)
		if err != nil {
			return nil, err
		}

		defs := []tools.ToolDef{{
			Name:     infoTool,
			Summary:  "查询当前工作区与会话绑定的工作区(只读)",
			ReadOnly: true,
			Timeout:  30 * time.Second,
			Source:   PluginName,
			Input:    schema.FromMap(map[string]any{"type": "object"}),
			Handler: func(callCtx context.Context, call tools.CallInfo) (tools.Result, error) {
				svc, err := chain.Get()
				if err != nil {
					return toolErr(err.Error()), nil
				}
				out, err := buildInfo(callCtx, svc)
				if err != nil {
					return toolErr(err.Error()), nil
				}
				return jsonResult(out), nil
			},
		}, {
			Name:     listTool,
			Summary:  "列出已配置的工作区(只读)",
			ReadOnly: true,
			Timeout:  30 * time.Second,
			Source:   PluginName,
			Input:    schema.FromMap(map[string]any{"type": "object"}),
			Handler: func(callCtx context.Context, call tools.CallInfo) (tools.Result, error) {
				svc, err := chain.Get()
				if err != nil {
					return toolErr(err.Error()), nil
				}
				list, err := svc.List(callCtx)
				if err != nil {
					return toolErr(err.Error()), nil
				}
				return jsonResult(list), nil
			},
		}}

		var disposers []kernel.Dispose
		for _, d := range defs {
			disp, err := registry.Register(d)
			if err != nil {
				for _, prior := range disposers {
					_ = prior(context.Background())
				}
				return nil, err
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
	}).WithInject("tools", workspace.ServiceName)
}

// buildInfo 读取会话绑定与当前工作区;绑定未知时直接报错,不静默回退。
func buildInfo(ctx context.Context, svc workspace.Service) (infoOutput, error) {
	current, err := svc.Current(ctx)
	if err != nil {
		return infoOutput{}, err
	}
	out := infoOutput{Current: current, Workspace: current}
	if b, ok := workspace.BindingFrom(ctx); ok {
		bound, err := svc.Get(ctx, b.Workspace)
		if err != nil {
			return infoOutput{}, err
		}
		out.Binding = string(b.Workspace)
		out.Workspace = bound
	}
	return out, nil
}

func jsonResult(v any) tools.Result {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolErr("序列化失败: " + err.Error())
	}
	return tools.Result{Status: tools.StatusOK, Content: []llm.Block{{Kind: llm.BlockText, Text: string(b)}}}
}

func toolErr(msg string) tools.Result {
	return tools.Result{Status: tools.StatusError, Content: []llm.Block{{Kind: llm.BlockText, Text: msg}}}
}
