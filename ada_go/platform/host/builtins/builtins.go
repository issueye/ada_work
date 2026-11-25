// Package builtins 是宿主共用的内置 Go 插件注册表与 caps 接线。
//
// host/cli 与 host/server 共享它,避免两处各维护一份「内置安全栈」清单。
package builtins

import (
	"github.com/issueye/ada_go_v2/platform/assemble"
	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/plugin"
	agentloop "github.com/issueye/ada_go_v2/plugins/core/agent-loop"
	compaction "github.com/issueye/ada_go_v2/plugins/core/compaction-basic"
	sessionstats "github.com/issueye/ada_go_v2/plugins/core/session-stats"
	systemprompt "github.com/issueye/ada_go_v2/plugins/core/system-prompt"
	approvalgate "github.com/issueye/ada_go_v2/plugins/policy/approval-gate"
	auditlog "github.com/issueye/ada_go_v2/plugins/policy/audit-log"
	capsgate "github.com/issueye/ada_go_v2/plugins/policy/caps-gate"
	fsobserve "github.com/issueye/ada_go_v2/plugins/policy/fs-observe"
	fsredact "github.com/issueye/ada_go_v2/plugins/policy/fs-redact"
	fssandbox "github.com/issueye/ada_go_v2/plugins/policy/fs-sandbox"
	llmretry "github.com/issueye/ada_go_v2/plugins/policy/llm-retry"
	rpcguard "github.com/issueye/ada_go_v2/plugins/policy/rpc-guard"
	rpcobserve "github.com/issueye/ada_go_v2/plugins/policy/rpc-observe"
	secretsguard "github.com/issueye/ada_go_v2/plugins/policy/secrets-guard"
	shellguard "github.com/issueye/ada_go_v2/plugins/policy/shell-guard"
	shellobserve "github.com/issueye/ada_go_v2/plugins/policy/shell-observe"
	shellredact "github.com/issueye/ada_go_v2/plugins/policy/shell-redact"
	tooltimeout "github.com/issueye/ada_go_v2/plugins/policy/tool-timeout"
	workspaceguard "github.com/issueye/ada_go_v2/plugins/policy/workspace-guard"
	approvalcli "github.com/issueye/ada_go_v2/plugins/provider/approval-cli"
	approvaldeny "github.com/issueye/ada_go_v2/plugins/provider/approval-deny"
	capsstatic "github.com/issueye/ada_go_v2/plugins/provider/caps-static"
	fslocal "github.com/issueye/ada_go_v2/plugins/provider/fs-local"
	openai "github.com/issueye/ada_go_v2/plugins/provider/llm-openai"
	replay "github.com/issueye/ada_go_v2/plugins/provider/llm-replay"
	rpcjson "github.com/issueye/ada_go_v2/plugins/provider/rpc-json"
	rpcnet "github.com/issueye/ada_go_v2/plugins/provider/rpc-net"
	rpcpipe "github.com/issueye/ada_go_v2/plugins/provider/rpc-pipe"
	rpctls "github.com/issueye/ada_go_v2/plugins/provider/rpc-tls"
	sandboxnone "github.com/issueye/ada_go_v2/plugins/provider/sandbox-none"
	secretsenv "github.com/issueye/ada_go_v2/plugins/provider/secrets-env"
	sessionjsonl "github.com/issueye/ada_go_v2/plugins/provider/session-jsonl"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	shelllocal "github.com/issueye/ada_go_v2/plugins/provider/shell-local"
	workspacelocal "github.com/issueye/ada_go_v2/plugins/provider/workspace-local"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamllm "github.com/issueye/ada_go_v2/plugins/seam/llm"
	seamprompts "github.com/issueye/ada_go_v2/plugins/seam/prompts"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	seamsandbox "github.com/issueye/ada_go_v2/plugins/seam/sandbox"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
	seamsession "github.com/issueye/ada_go_v2/plugins/seam/session"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
	seamworkspace "github.com/issueye/ada_go_v2/plugins/seam/workspace"
	toolhelper "github.com/issueye/ada_go_v2/plugins/tool/tool-helper"
	toolworkspace "github.com/issueye/ada_go_v2/plugins/tool/workspace-helper"
)

// Plugins 返回编译进宿主的内置 Go 插件注册表。
//
// 第三方 Go 插件仍需宿主编译时注册;这里只放仓库内置的参考实现。
func Plugins() map[string]func() kernel.Plugin {
	return map[string]func() kernel.Plugin{
		"seam-llm":         seamllm.Plugin,
		"llm-replay":       replay.Plugin,
		"llm-openai":       openai.Plugin,
		"seam-prompts":     seamprompts.Plugin,
		"system-prompt":    systemprompt.Plugin,
		"seam-tools":       seamtools.Plugin,
		"agent-loop":       agentloop.Plugin,
		"session-stats":    sessionstats.Plugin,
		"compaction-basic": compaction.Plugin,
		"approval-gate":    approvalgate.Plugin,
		"audit-log":        auditlog.Plugin,
		"llm-retry":        llmretry.Plugin,
		"tool-timeout":     tooltimeout.Plugin,
		"seam-fs":          seamfs.Plugin,
		"fs-local":         fslocal.Plugin,
		"fs-sandbox":       fssandbox.Plugin,
		"seam-workspace":   seamworkspace.Plugin,
		"workspace-local":  workspacelocal.Plugin,
		"workspace-guard":  workspaceguard.Plugin,
		"workspace-helper": toolworkspace.Plugin,
		"tool-helper":      toolhelper.Plugin,
		"seam-shell":       seamshell.Plugin,
		"shell-local":      shelllocal.Plugin,
		"shell-guard":      shellguard.Plugin,
		"seam-sandbox":     seamsandbox.Plugin,
		"sandbox-none":     sandboxnone.Plugin,
		"seam-approval":    seamapproval.Plugin,
		"approval-deny":    approvaldeny.Plugin,
		"approval-cli":     approvalcli.Plugin,
		"seam-session":     seamsession.Plugin,
		"session-memory":   memory.Plugin,
		"session-jsonl":    sessionjsonl.Plugin,
		"caps-static":      capsstatic.Plugin,
		"seam-secrets":     seamsecrets.Plugin,
		"secrets-env":      secretsenv.Plugin,
		"secrets-guard":    secretsguard.Plugin,
		"caps-gate":        capsgate.Plugin,
		"fs-redact":        fsredact.Plugin,
		"fs-observe":       fsobserve.Plugin,
		"shell-redact":     shellredact.Plugin,
		"shell-observe":    shellobserve.Plugin,
		"seam-rpc":         seamrpc.Plugin,
		"rpc-json":         rpcjson.Plugin,
		"rpc-net":          rpcnet.Plugin,
		"rpc-pipe":         rpcpipe.Plugin,
		"rpc-tls":          rpctls.Plugin,
		"rpc-guard":        rpcguard.Plugin,
		"rpc-observe":      rpcobserve.Plugin,
	}
}

// ApplyAutoGrants 把清单 caps/trust 灌进提供 "caps" 服务的插件配置。
//
// 这样 caps-gate 会优先用注入的 Broker;preset 里手写的 grants 仍会保留(并集)。
func ApplyAutoGrants(manifests []*plugin.Manifest) error {
	grants, err := AutoGrants(manifests)
	if err != nil {
		return err
	}
	for _, m := range manifests {
		provides := false
		for _, name := range m.Provide {
			if name == "caps" {
				provides = true
				break
			}
		}
		if !provides {
			continue
		}
		if m.Config == nil {
			m.Config = map[string]any{}
		}
		merged := map[string][]string{}
		if existing, ok := m.Config["grants"].(map[string][]string); ok {
			for k, v := range existing {
				merged[k] = append(merged[k], v...)
			}
		}
		for plugin, list := range grants {
			merged[plugin] = append(merged[plugin], list...)
		}
		m.Config["grants"] = merged
	}
	return nil
}

// AutoGrants 从清单 caps/trust 生成授权表(失败即返回错误)。
func AutoGrants(manifests []*plugin.Manifest) (map[string][]string, error) {
	broker := caps.NewBroker(nil)
	if err := assemble.GrantManifests(broker, manifests); err != nil {
		return nil, err
	}
	return broker.Snapshot(), nil
}
