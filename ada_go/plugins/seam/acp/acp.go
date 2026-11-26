// Package acp 是「agent 调用 ACP client」的反向能力契约。
//
// 与其它 seam 不同,这里描述的不是宿主资源,而是客户端在 initialize 时声明的
// 可选能力:fs/read_text_file、fs/write_text_file 与 terminal/*。它们可能完全
// 不存在,调用方必须先看 Capabilities 并处理 EUNIMPLEMENTED。
//
// 路径是客户端视角的绝对路径,不是 seam/fs 的虚拟路径;因此本契约刻意不接入
// seam/fs 链,而由需要编辑器文件/终端能力的工具或插件显式消费。
package acp

import (
	"context"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ServiceName 是 ACP client 能力在 kernel 中注册的服务名。
const ServiceName = "acp"

const pluginID = "seam-acp"

// Capabilities 是 client 声明的能力;连接级,不是会话级。
type Capabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
	Terminal      bool `json:"terminal"`
}

// TerminalRequest 是 terminal/create 的参数。
type TerminalRequest struct {
	Command         string            `json:"command"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Cwd             string            `json:"cwd,omitempty"`
	OutputByteLimit int               `json:"outputByteLimit,omitempty"`
}

// ExitStatus 是终端退出状态;仍在运行时两个字段都为空。
type ExitStatus struct {
	ExitCode *int    `json:"exitCode,omitempty"`
	Signal   *string `json:"signal,omitempty"`
}

// TerminalOutput 是 terminal/output 的结果。
type TerminalOutput struct {
	Output     string      `json:"output"`
	Truncated  bool        `json:"truncated,omitempty"`
	ExitStatus *ExitStatus `json:"exitStatus,omitempty"`
}

// Client 是 ACP client 的反向能力;由 provider/control-acp 的 Endpoint 实现。
//
// 所有方法都要求 sessionID:ACP 用会话限定文件与终端的归属。
type Client interface {
	Capabilities() Capabilities

	ReadTextFile(ctx context.Context, sessionID, path string, line, limit int) (string, error)
	WriteTextFile(ctx context.Context, sessionID, path, content string) error

	CreateTerminal(ctx context.Context, sessionID string, req TerminalRequest) (string, error)
	TerminalOutput(ctx context.Context, sessionID, terminalID string) (TerminalOutput, error)
	WaitForExit(ctx context.Context, sessionID, terminalID string) (ExitStatus, error)
	KillTerminal(ctx context.Context, sessionID, terminalID string) error
	ReleaseTerminal(ctx context.Context, sessionID, terminalID string) error
}

// Registry 持有当前连接的 client。
//
// 一个宿主进程同一时刻只有一个 ACP 连接,所以用 Set/Dispose 绑定而不是多实现
// 注册表;连接结束时清空,没有连接时 Get 返回 EUNIMPLEMENTED。
type Registry struct {
	mu     sync.RWMutex
	client Client
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry { return &Registry{} }

// Set 绑定当前连接的 client;已有的绑定必须的显式 Dispose 后才能替换。
func (r *Registry) Set(c Client) (kernel.Dispose, error) {
	if c == nil {
		return nil, Invalid("ACP client 不能为 nil")
	}
	r.mu.Lock()
	if r.client != nil {
		r.mu.Unlock()
		return nil, Invalid("ACP client 已绑定")
	}
	r.client = c
	r.mu.Unlock()
	return func(context.Context) error {
		r.mu.Lock()
		if r.client == c {
			r.client = nil
		}
		r.mu.Unlock()
		return nil
	}, nil
}

// Get 返回当前 client;没有连接时返回 EUNIMPLEMENTED。
func (r *Registry) Get() (Client, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.client == nil {
		return nil, Unimplemented("没有 ACP 连接")
	}
	return r.client, nil
}

// Available 判断当前是否有连接(诊断用)。
func (r *Registry) Available() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.client != nil
}

// Unimplemented 表示 client 未声明该能力。
func Unimplemented(msg string) error { return seamerr.Unimplemented("acp", msg) }

// Invalid 构造 EINVALID。
func Invalid(msg string) error {
	return seamerr.New(seamerr.ErrInvalid, "acp", msg, false)
}

// Unavailable 构造 EUNAVAILABLE。
func Unavailable(msg string) error {
	return seamerr.New(seamerr.ErrUnavailable, "acp", msg, true)
}

// Plugin 返回 seam/acp 插件:只提供 acp 服务(Registry),不连接任何客户端。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide(ServiceName, NewRegistry(), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide(ServiceName)
}
