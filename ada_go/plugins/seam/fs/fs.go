// Package fs 是 `seam/fs` 的契约层:虚拟路径、读/写/编辑/列目录的类型与接口、
// 装饰器链与错误码;没有实现(实现属于 plugins/provider/*,策略属于 plugins/policy/*)。
//
// 契约来源:docs/seam-api-security.md §1、§3。
package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// 错误码(seam-api-security §1.5)。
const (
	CodeDenied   seamerr.Code = "EDENIED"
	CodeNotFound seamerr.Code = "ENOENT"
	CodeIsDir    seamerr.Code = "EISDIR"
	CodeNotDir   seamerr.Code = "ENOTDIR"
	CodeConflict seamerr.Code = "ECONFLICT"
	CodeBinary   seamerr.Code = "EBINARY"
	CodeTooLarge seamerr.Code = "ETOOLARGE"
	CodeInvalid  seamerr.Code = "EINVALID"
	CodeProbe    seamerr.Code = "EPROBE"
)

// Path 是虚拟路径:/workspace/...、/tmp/...、/plugin/<id>/...。
//
// 宿主绝对路径不进插件代码:插件只能拿到 Path,真实路径由 provider 内部解析,
// 因此「构造一个原始 os.File」在类型层面就不可能。
type Path string

// Format 是读结果的格式。
type Format string

const (
	FormatAuto  Format = "auto"
	FormatText  Format = "text"
	FormatBytes Format = "bytes"
)

// FileKind 是文件类型(读工具据此决定是否解码)。
type FileKind string

const (
	KindText   FileKind = "text"
	KindBinary FileKind = "binary"
	KindImage  FileKind = "image"
	KindDir    FileKind = "dir"
	KindOther  FileKind = "other"
)

// Version 是内容哈希 + mtime + size 组成的版本凭证。
type Version string

// 默认上限(安全契约 §3.2)。
const (
	DefaultReadLimit  int64 = 1 << 20
	DefaultWriteLimit int64 = 4 << 20
	DefaultListLimit        = 2000
)

// VersionOf 计算版本凭证;provider 与测试共用同一算法。
func VersionOf(content []byte, modTime time.Time, size int64) Version {
	sum := sha256.Sum256(content)
	return Version(fmt.Sprintf("%s-%d-%d", hex.EncodeToString(sum[:8]), modTime.UnixNano(), size))
}

// FileInfo 是 stat 的结果。
type FileInfo struct {
	Path    Path
	Kind    FileKind
	Size    int64
	ModTime time.Time
	Version Version
}

// DirEntry 是列目录的一条。
type DirEntry struct {
	Path Path
	Name string
	Kind FileKind
	Size int64
}

// ReadRequest 是有界读。
type ReadRequest struct {
	Path   Path
	Offset int64
	Limit  int64  // 0 = DefaultReadLimit
	Format Format // 空 = auto
}

// ReadResult 是读结果;二进制在 text 格式下返回 EBINARY。
type ReadResult struct {
	Content   string
	Truncated bool
	Version   Version
	Kind      FileKind
	MimeType  string
	Size      int64
}

// WriteRequest 是原子写。IfVersion 为空表示「新建或危险模式覆盖」。
type WriteRequest struct {
	Path       Path
	Content    []byte
	IfVersion  Version
	CreateDirs bool
}

// WriteResult 返回新版本凭证。
type WriteResult struct {
	Version Version
	Size    int64
}

// EditRequest 是精确替换;必须提供读到的 Version(read-before-edit)。
type EditRequest struct {
	Path      Path
	IfVersion Version
	Old, New  string
	All       bool
}

// EditResult 返回替换次数与新版本。
type EditResult struct {
	Replacements int
	Version      Version
}

// ListRequest 列目录。
type ListRequest struct {
	Path      Path
	Recursive bool
	MaxDepth  int
	Limit     int
}

// SearchRequest 搜索(可选能力)。
type SearchRequest struct {
	Path  Path
	Query string
	Limit int
}

// SearchResult 是搜索结果。
type SearchResult struct {
	Matches []SearchMatch
}

// SearchMatch 是一条命中。
type SearchMatch struct {
	Path Path
	Line int
	Text string
}

// FS 是文件系统契约。
type FS interface {
	Stat(ctx context.Context, p Path) (FileInfo, error)
	List(ctx context.Context, req ListRequest) ([]DirEntry, error)
	Read(ctx context.Context, req ReadRequest) (ReadResult, error)
	Write(ctx context.Context, req WriteRequest) (WriteResult, error)
	Edit(ctx context.Context, req EditRequest) (EditResult, error)
	Search(ctx context.Context, req SearchRequest) (SearchResult, error)
}

// Denied 构造一个「安全闸拒绝」错误:source 区分 cap / sandbox / approval。
func Denied(source, message string) error {
	return &seamerr.Error{Code: CodeDenied, Provider: "fs", Message: fmt.Sprintf("[source=%s] %s", source, message)}
}

// NotFound 构造 ENOENT。
func NotFound(p Path) error {
	return &seamerr.Error{Code: CodeNotFound, Provider: "fs", Message: fmt.Sprintf("路径不存在: %s", p)}
}

// Conflict 构造 ECONFLICT。
func Conflict(message string) error {
	return &seamerr.Error{Code: CodeConflict, Provider: "fs", Message: message}
}

// TooLarge 构造 ETOOLARGE。
func TooLarge(message string) error {
	return &seamerr.Error{Code: CodeTooLarge, Provider: "fs", Message: message}
}

// Invalid 构造 EINVALID。
func Invalid(message string) error {
	return &seamerr.Error{Code: CodeInvalid, Provider: "fs", Message: message}
}

// Chain 是 seam 插件提供的服务值:provider 安装底座,策略注册装饰器。
//
// 为什么用装饰器而不是事件:安全闸必须确定性、可排序、同步可拒;
// 事件监听器可能 panic、可能不调 next,不适合做安全边界(§1.4)。
type Chain struct {
	mu    sync.Mutex
	base  FS
	items []decoration
	seq   uint64
}

type decoration struct {
	id       uint64
	fn       func(FS) FS
	priority int
}

// NewChain 创建空链。
func NewChain() *Chain { return &Chain{} }

// Install 安装底座实现;重复安装报错(一个 fs 只能有一个实现)。
func (c *Chain) Install(base FS) (kernel.Dispose, error) {
	if base == nil {
		return nil, Invalid("fs: 底座实现不能为 nil")
	}
	c.mu.Lock()
	if c.base != nil {
		c.mu.Unlock()
		return nil, Invalid("fs: 已安装底座实现")
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

// Decorate 注册装饰器;priority 高者在外层(先判),同级后注册者在外层。
func (c *Chain) Decorate(fn func(FS) FS, priority int) (kernel.Dispose, error) {
	if fn == nil {
		return nil, Invalid("fs: 装饰器不能为 nil")
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

// Get 组装当前链:未安装底座返回 EUNIMPLEMENTED。
//
// 顺序:优先级升序包一层(低优先级在内层),最后包的是外层;
// 同优先级按注册顺序,后注册的更靠外。
func (c *Chain) Get() (FS, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == nil {
		return nil, seamerr.New(seamerr.ErrUnimplemented, "fs", "没有安装 fs 实现", false)
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

// Root 返回底座实现的宿主根目录。
//
// 它只供宿主适配层(node:fs 的根、诊断)使用,不进入 FS 接口:
// 装饰器仍然只负责一次调用的策略,不应该被要求理解「根」。
func (c *Chain) Root(ctx context.Context) (string, error) {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	if base == nil {
		return "", seamerr.New(seamerr.ErrUnimplemented, "fs", "没有安装 fs 实现", false)
	}
	rooted, ok := base.(interface {
		Root(context.Context) (string, error)
	})
	if !ok {
		return "", seamerr.Unimplemented("fs", "fs 实现不暴露根目录")
	}
	return rooted.Root(ctx)
}

// Applied 返回装饰器的优先级序列(外层到内层,诊断用)。
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

// pluginID 是 seam 插件的 id。
const pluginID = "seam-fs"

// EventObserve 是 fs 操作观察事件(观察,不是门禁;payload 见 docs/seam-fs.md)。
const EventObserve = "fs/observe"

// Plugin 返回 seam/fs 插件:只提供 fs 服务(Chain),不提供实现。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("fs", NewChain(), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("fs")
}
