// Package shell 是 `seam/shell` 的契约层:命令/结果类型、受管环境、装饰器链与错误码。
//
// 契约来源:docs/seam-api-security.md §4。实现属于 plugins/provider/*,策略属于 plugins/policy/*。
package shell

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// 错误码(seam-api-security §1.5 与 §4.2)。
const (
	CodeDenied   seamerr.Code = "EDENIED"
	CodeTimeout  seamerr.Code = "ETIMEOUT"
	CodeInvalid  seamerr.Code = "EINVALID"
	CodeNotFound seamerr.Code = "ENOENT"
	CodeTooLarge seamerr.Code = "ETOOLARGE"
)

// 默认值与上限。
const (
	DefaultTimeout = 120 * time.Second
	MaxTimeout     = 30 * time.Minute
	MaxOutputBytes = 4 << 20
	MaxChunkBytes  = 64 << 10
)

// Path 是虚拟路径(与 seam/fs 同一路径空间):/workspace/...、/tmp/...。
type Path string

// Facts 是本次执行实际受到的约束。沙箱 seam 落地后这里会改为 sandbox.Facts。
type Facts struct {
	Enforced bool
	Backend  string
	Mode     string
}

// Command 是一条命令。Program 必须是程序名/路径,不含空白;参数不经 shell 解析。
type Command struct {
	Program   string
	Args      []string
	Dir       Path
	Stdin     []byte
	Env       map[string]string
	Timeout   time.Duration // 0 = DefaultTimeout
	Shell     bool          // 默认 false;true 需要 policy 显式允许
	ShellArgs []string
}

// Result 是一次执行的结果;输出截断到 MaxOutputBytes。
type Result struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool
	Duration  time.Duration
	Facts     Facts
}

// Chunk 是流式输出的一段。
type Chunk struct {
	Stream string // stdout | stderr
	Text   string
	Seq    int
}

// Env 是受管环境变量注册表:只暴露清单声明 ∩ 宿主存在的变量 + ADA_*。
type Env interface {
	Get(name string) (string, bool)
	Snapshot() map[string]string
}

// Shell 是命令执行契约。
type Shell interface {
	Run(ctx context.Context, cmd Command) (Result, error)
	Stream(ctx context.Context, cmd Command, onChunk func(Chunk) error) (Result, error)
	Env() Env
}

// Denied 构造安全闸拒绝(source=sandbox|cap|approval)。
func Denied(source, message string) error {
	return &seamerr.Error{Code: CodeDenied, Provider: "shell", Message: fmt.Sprintf("[source=%s] %s", source, message)}
}

// Invalid 构造 EINVALID。
func Invalid(message string) error {
	return &seamerr.Error{Code: CodeInvalid, Provider: "shell", Message: message}
}

// Timeout 构造 ETIMEOUT。
func Timeout(message string, err error) error {
	return &seamerr.Error{Code: CodeTimeout, Provider: "shell", Message: message, Err: err}
}

// Chain 是 seam 插件提供的服务值:provider 安装底座,策略注册装饰器。
type Chain struct {
	mu    sync.Mutex
	base  Shell
	items []decoration
	seq   uint64
}

type decoration struct {
	id       uint64
	fn       func(Shell) Shell
	priority int
}

// NewChain 创建空链。
func NewChain() *Chain { return &Chain{} }

// Install 安装底座实现。
func (c *Chain) Install(base Shell) (kernel.Dispose, error) {
	if base == nil {
		return nil, Invalid("shell: 底座实现不能为 nil")
	}
	c.mu.Lock()
	if c.base != nil {
		c.mu.Unlock()
		return nil, Invalid("shell: 已安装底座实现")
	}
	c.base = base
	c.mu.Unlock()

	var once bool
	return func(context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !once && c.base == base {
			c.base = nil
			once = true
		}
		return nil
	}, nil
}

// Decorate 注册装饰器;priority 高者在外层,同级后注册者在外层。
func (c *Chain) Decorate(fn func(Shell) Shell, priority int) (kernel.Dispose, error) {
	if fn == nil {
		return nil, Invalid("shell: 装饰器不能为 nil")
	}
	c.mu.Lock()
	c.seq++
	d := decoration{id: c.seq, fn: fn, priority: priority}
	c.items = append(c.items, d)
	c.mu.Unlock()

	var once bool
	return func(context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if once {
			return nil
		}
		once = true
		for i := range c.items {
			if c.items[i].id == d.id {
				c.items = append(c.items[:i], c.items[i+1:]...)
				break
			}
		}
		return nil
	}, nil
}

// Get 组装当前链;未安装底座返回 EUNIMPLEMENTED。
func (c *Chain) Get() (Shell, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == nil {
		return nil, seamerr.New(seamerr.ErrUnimplemented, "shell", "没有安装 shell 实现", false)
	}
	items := make([]decoration, len(c.items))
	copy(items, c.items)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority < items[j].priority
		}
		return items[i].id < items[j].id
	})
	cur := c.base
	for _, d := range items {
		cur = d.fn(cur)
	}
	return cur, nil
}

// Applied 返回装饰器优先级(外层到内层,诊断用)。
func (c *Chain) Applied() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]decoration, len(c.items))
	copy(items, c.items)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority > items[j].priority
		}
		return items[i].id > items[j].id
	})
	out := make([]int, 0, len(items))
	for _, d := range items {
		out = append(out, d.priority)
	}
	return out
}

const pluginID = "seam-shell"

// EventObserve 是 shell 操作观察事件(观察不是门禁;payload 见 docs/seam-shell.md)。
const EventObserve = "shell/observe"

// Plugin 返回 seam/shell 插件:只提供 shell 服务(Chain),不提供实现。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("shell", NewChain(), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("shell")
}
