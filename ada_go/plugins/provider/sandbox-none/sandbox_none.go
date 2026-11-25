// Package sandboxnone 是 `provider/sandbox-none`:只在策略层限制,不做 OS 级隔离。
//
// 它必须如实报告 Enforced=false,让 UI/工具结果明确提示「未隔离」;
// 假装隔离比不隔离更危险。
package sandboxnone

import (
	"context"
	"fmt"
	"strings"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamsandbox "github.com/issueye/ada_go_v2/plugins/seam/sandbox"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ProviderName 是后端名与插件 id。
const ProviderName = "sandbox-none"

// None 是策略层实现。
type None struct {
	mode seamsandbox.Mode
}

// New 创建实现;mode 非法时报错。
func New(mode seamsandbox.Mode) (*None, error) {
	if mode == "" {
		mode = seamsandbox.ModeReadOnly
	}
	if !seamsandbox.ValidMode(mode) {
		return nil, seamerr.New(seamerr.ErrInvalid, ProviderName, fmt.Sprintf("未知模式 %q", mode), false)
	}
	return &None{mode: mode}, nil
}

// Mode 返回当前模式(本实现是单模式,不区分会话)。
func (n *None) Mode(context.Context, string) seamsandbox.Mode { return n.mode }

// Resolve 是判定入口:只做策略层规则,不声称 OS 级隔离。
func (n *None) Resolve(_ context.Context, req seamsandbox.Request) (seamsandbox.Decision, error) {
	switch n.mode {
	case seamsandbox.ModeReadOnly:
		switch req.Op {
		case seamsandbox.OpFSRead, seamsandbox.OpFSList:
			return allow(n.mode, "read-only: 读取放行"), nil
		case seamsandbox.OpFSWrite, seamsandbox.OpFSEdit:
			return deny(n.mode, true, fmt.Sprintf("read-only: 拒绝写 %s", req.Path)), nil
		case seamsandbox.OpShellExec:
			// 只读模式是否放行命令由 shell 策略决定(白名单),这里不重复判定。
			return allow(n.mode, "read-only: 命令交由 shell 策略判定"), nil
		}
	case seamsandbox.ModeWorkspaceWrite:
		switch req.Op {
		case seamsandbox.OpFSRead, seamsandbox.OpFSList, seamsandbox.OpShellExec:
			return allow(n.mode, fmt.Sprintf("%s: 放行 %s", n.mode, req.Op)), nil
		case seamsandbox.OpFSWrite, seamsandbox.OpFSEdit:
			if underWorkspaceOrTmp(req.Path) {
				return allow(n.mode, fmt.Sprintf("workspace-write: 允许写 %s", req.Path)), nil
			}
			return deny(n.mode, true, fmt.Sprintf("workspace-write: 只允许写 /workspace 与 /tmp,拒绝 %s", req.Path)), nil
		}
	case seamsandbox.ModeDanger:
		// 即使危险模式,插件目录仍受保护(安全契约 §1.3)。
		if req.Op == seamsandbox.OpFSWrite || req.Op == seamsandbox.OpFSEdit {
			p := string(req.Path)
			if p == "/plugin" || strings.HasPrefix(p, "/plugin/") {
				return deny(n.mode, false, fmt.Sprintf("danger-full-access: 插件目录受保护,拒绝写 %s", p)), nil
			}
		}
		return allow(n.mode, fmt.Sprintf("danger-full-access: 放行 %s", req.Op)), nil
	}
	return deny(n.mode, false, fmt.Sprintf("未知操作 %q,按拒绝处理", req.Op)), nil
}

func allow(mode seamsandbox.Mode, reason string) seamsandbox.Decision {
	return seamsandbox.Decision{Allow: true, Mode: mode, Reasons: []string{reason}}
}

func deny(mode seamsandbox.Mode, escalatable bool, reason string) seamsandbox.Decision {
	return seamsandbox.Decision{Allow: false, Mode: mode, Escalatable: escalatable, Reasons: []string{reason}}
}

func underWorkspaceOrTmp(p seamsandbox.Path) bool {
	s := string(p)
	return s == "/workspace" || strings.HasPrefix(s, "/workspace/") ||
		s == "/tmp" || strings.HasPrefix(s, "/tmp/")
}

// Backends 报告本后端能力:probe 通过,但只是策略层。
func (n *None) Backends() []seamsandbox.BackendInfo {
	return []seamsandbox.BackendInfo{{Name: ProviderName, Kind: "policy-only"}}
}

// Facts 如实报告「未隔离」。
func (n *None) Facts(context.Context, seamsandbox.Request) seamsandbox.Facts {
	return seamsandbox.Facts{
		Backend:  ProviderName,
		Mode:     n.mode,
		Roots:    []string{"/workspace", "/tmp"},
		Enforced: false,
	}
}

// Plugin 返回 sandbox-none 插件:inject sandbox seam,把后端注册进去。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*seamsandbox.Registry](ctx, "sandbox")
		if err != nil {
			return nil, err
		}
		mode := seamsandbox.Mode(cfg.String("mode"))
		impl, err := New(mode)
		if err != nil {
			return nil, err
		}
		return registry.Register(ProviderName, impl)
	}).WithInject("sandbox")
}
