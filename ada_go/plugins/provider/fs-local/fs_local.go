// Package fslocal 是 fs seam 的本地实现:虚拟路径映射到根目录、
// 越界拒绝、有界读、原子写、版本守卫、精确替换。
//
// 它不是安全边界的全部:策略(只读/工作区写)由 plugins/policy/fs-sandbox 装饰器负责;
// 这里只保证「不会因为实现漏洞绕过根目录」。
package fslocal

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// ProviderName 是 provider 名与插件 id。
const ProviderName = "fs-local"

// Options 配置根目录。
type Options struct {
	// Workspace 是独立模式的 /workspace 根;托管模式下可为空。
	Workspace string
	// Tmp 是 /tmp 的宿主根目录;为空时放在 Workspace 下的 .ada-tmp。
	// 托管模式且 Workspace 为空时必须显式给出,避免把 /tmp 落到进程 cwd。
	Tmp string
	// WorkspaceChain 非 nil 时启用托管模式:优先用 workspace 服务解析 /workspace。
	WorkspaceChain *workspace.Chain
}

// FS 是本地实现。
type FS struct {
	workspace   string
	tmp         string
	resolvedWS  string
	resolvedTmp string
	wschain     *workspace.Chain
}

// New 创建实现;根目录必须是绝对路径。
func New(opts Options) (*FS, error) {
	if opts.Workspace == "" && opts.WorkspaceChain == nil {
		return nil, seamfs.Invalid("fs-local: Workspace 不能为空(或启用 workspace 托管模式)")
	}
	var ws string
	var err error
	if opts.Workspace != "" {
		ws, err = filepath.Abs(opts.Workspace)
		if err != nil {
			return nil, seamfs.Invalid(fmt.Sprintf("fs-local: Workspace 非法: %v", err))
		}
	}
	tmp := opts.Tmp
	if tmp == "" {
		if ws == "" {
			return nil, seamfs.Invalid("fs-local: 托管模式且 Workspace 为空时必须配置 tmp")
		}
		tmp = filepath.Join(ws, ".ada-tmp")
	}
	tmp, err = filepath.Abs(tmp)
	if err != nil {
		return nil, seamfs.Invalid(fmt.Sprintf("fs-local: Tmp 非法: %v", err))
	}
	f := &FS{workspace: ws, tmp: tmp, wschain: opts.WorkspaceChain}
	f.resolvedWS = resolvedOrSelf(ws)
	f.resolvedTmp = resolvedOrSelf(tmp)
	return f, nil
}

// Name 返回 provider 名。
func (f *FS) Name() string { return ProviderName }

// Root 返回 /workspace 的宿主根,供 node:fs 安装时使用。
//
// 托管模式下按当前 workspace 绑定解析;独立模式直接用配置的 workspace。
func (f *FS) Root(ctx context.Context) (string, error) {
	if f.wschain != nil {
		if svc, err := f.wschain.Get(); err == nil {
			target, rerr := svc.ResolveHost(ctx, workspace.OpRead, workspace.MountRoot)
			if rerr != nil {
				return "", rerr
			}
			return target.Root, nil
		}
	}
	if f.workspace == "" {
		return "", seamfs.Invalid("fs-local: 没有可用的 workspace 根")
	}
	return f.workspace, nil
}

// resolve 把虚拟路径解析成宿主路径;任何越界都返回 EDENIED(source=sandbox)。
//
// 托管模式优先问 workspace 服务;只有服务在位但 provider 尚未就绪且本机仍配置了
// 根时才回退到独立模式。两者根不一致时 fail-closed,避免读写落到不同目录。
func (f *FS) resolve(ctx context.Context, op workspace.Op, v seamfs.Path) (host string, root string, err error) {
	s := string(v)
	if s == "" {
		return "", "", seamfs.Invalid("fs-local: 路径不能为空")
	}
	if strings.ContainsRune(s, 0) {
		return "", "", seamfs.Invalid("fs-local: 路径包含 NUL")
	}
	if strings.Contains(s, `\`) {
		return "", "", seamfs.Invalid("fs-local: 路径不能包含反斜杠")
	}
	if !strings.HasPrefix(s, "/") {
		return "", "", seamfs.Invalid("fs-local: 虚拟路径必须以 / 开头")
	}
	clean := path.Clean(s)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", seamfs.Denied("sandbox", fmt.Sprintf("路径越界: %s", s))
	}

	// 托管模式只接管 /workspace;/tmp 仍由本 provider 自己拥有。
	if f.wschain != nil && (clean == workspace.MountRoot || strings.HasPrefix(clean, workspace.MountRoot+"/")) {
		svc, gerr := f.wschain.Get()
		if gerr != nil {
			if f.workspace == "" {
				return "", "", gerr
			}
			// 已配置独立根:provider 尚未安装时保持旧行为;provider 出现后自动托管。
		} else {
			target, rerr := svc.ResolveHost(ctx, op, clean)
			if rerr != nil {
				return "", "", rerr
			}
			if f.workspace != "" && !sameDirectory(f.workspace, target.Root) {
				return "", "", workspace.Conflict("fs-local: 配置的 workspace 与 workspace 服务根不一致")
			}
			return target.Host, target.Root, nil
		}
	}

	var prefix, rel string
	switch {
	case clean == "/workspace" || strings.HasPrefix(clean, "/workspace/"):
		root = f.workspace
		prefix = "/workspace"
		rel = strings.TrimPrefix(clean, prefix)
	case clean == "/tmp" || strings.HasPrefix(clean, "/tmp/"):
		root = f.tmp
		prefix = "/tmp"
		rel = strings.TrimPrefix(clean, prefix)
	default:
		return "", "", seamfs.Denied("sandbox", fmt.Sprintf("不支持的虚拟根: %s", clean))
	}
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		rel = "."
	}
	host = filepath.Join(root, filepath.FromSlash(rel))
	if !within(root, host) {
		return "", "", seamfs.Denied("sandbox", fmt.Sprintf("路径越界: %s", s))
	}

	// 符号链接:目标存在时解析它;目标不存在时解析「最深的已存在祖先」。
	// 只检查最终路径的 EvalSymlinks 会在「新文件 + 父目录是根外链接」时漏掉。
	target, err := secureTarget(root, host)
	if err != nil {
		return "", "", err
	}
	return target, root, nil
}

// secureTarget 返回一个解析掉符号链接、且确定在 root 内的宿主路径。
//
// 对不存在的目标,逐级向上找已存在祖先并解析它,再把缺失的相对段拼回;
// 这样 Write 的父目录链接不会把临时文件/rename 带到根外。
func secureTarget(root, host string) (string, error) {
	resolvedRoot := resolvedOrSelf(root)
	if target, err := filepath.EvalSymlinks(host); err == nil {
		if !within(resolvedRoot, target) {
			return "", seamfs.Denied("sandbox", fmt.Sprintf("符号链接指向根外: %s", host))
		}
		return target, nil
	}

	cur := host
	var missing []string
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		// 先把当前段记为「缺失」,再判断父目录是否存在:
		// 父目录存在时,当前段仍要拼回最终路径。
		missing = append(missing, filepath.Base(cur))
		if _, err := os.Lstat(parent); err == nil {
			resolvedParent, err := filepath.EvalSymlinks(parent)
			if err != nil {
				return "", err
			}
			if !within(resolvedRoot, resolvedParent) {
				return "", seamfs.Denied("sandbox", fmt.Sprintf("路径父目录指向根外: %s", host))
			}
			parts := append([]string{resolvedParent}, reverse(missing)...)
			return filepath.Join(parts...), nil
		}
		cur = parent
	}
	return host, nil
}

func reverse(in []string) []string {
	out := make([]string, len(in))
	for i := range in {
		out[i] = in[len(in)-1-i]
	}
	return out
}

func resolvedOrSelf(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// sameDirectory 用 os.SameFile 比较两个目录,避免大小写/符号链接导致的误判。
func sameDirectory(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (f *FS) checkCtx(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "调用已取消", err)
	}
	return nil
}

// Stat 返回文件信息。
func (f *FS) Stat(ctx context.Context, p seamfs.Path) (seamfs.FileInfo, error) {
	if err := f.checkCtx(ctx); err != nil {
		return seamfs.FileInfo{}, err
	}
	host, _, err := f.resolve(ctx, workspace.OpRead, p)
	if err != nil {
		return seamfs.FileInfo{}, err
	}
	st, err := os.Stat(host)
	if err != nil {
		if os.IsNotExist(err) {
			return seamfs.FileInfo{}, seamfs.NotFound(p)
		}
		return seamfs.FileInfo{}, err
	}
	if st.IsDir() {
		return seamfs.FileInfo{Path: p, Kind: seamfs.KindDir, Size: 0, ModTime: st.ModTime()}, nil
	}
	version, err := versionFor(host)
	if err != nil {
		return seamfs.FileInfo{}, err
	}
	return seamfs.FileInfo{Path: p, Kind: kindFor(host, prefixBytes(host)), Size: st.Size(), ModTime: st.ModTime(), Version: version}, nil
}

// Read 有界读;offset/limit 越界返回 EINVALID。
func (f *FS) Read(ctx context.Context, req seamfs.ReadRequest) (seamfs.ReadResult, error) {
	if err := f.checkCtx(ctx); err != nil {
		return seamfs.ReadResult{}, err
	}
	if req.Offset < 0 {
		return seamfs.ReadResult{}, seamfs.Invalid("fs-local: offset 不能为负")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = seamfs.DefaultReadLimit
	}
	host, _, err := f.resolve(ctx, workspace.OpRead, req.Path)
	if err != nil {
		return seamfs.ReadResult{}, err
	}
	st, err := os.Stat(host)
	if err != nil {
		if os.IsNotExist(err) {
			return seamfs.ReadResult{}, seamfs.NotFound(req.Path)
		}
		return seamfs.ReadResult{}, err
	}
	if st.IsDir() {
		return seamfs.ReadResult{}, &seamerr.Error{Code: seamfs.CodeIsDir, Provider: ProviderName, Message: fmt.Sprintf("是目录: %s", req.Path)}
	}

	fh, err := os.Open(host)
	if err != nil {
		return seamfs.ReadResult{}, err
	}
	defer fh.Close()
	if req.Offset > 0 {
		if _, err := fh.Seek(req.Offset, io.SeekStart); err != nil {
			return seamfs.ReadResult{}, err
		}
	}
	buf := make([]byte, limit+1)
	n, rerr := io.ReadFull(fh, buf)
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		return seamfs.ReadResult{}, rerr
	}
	truncated := int64(n) > limit
	content := buf[:int64(n)]
	if truncated {
		content = content[:limit]
	}

	kind := kindFor(host, prefixBytes(host))
	format := req.Format
	if format == "" {
		format = seamfs.FormatAuto
	}
	var out string
	if format == seamfs.FormatBytes {
		out = base64.StdEncoding.EncodeToString(content)
	} else {
		if kind != seamfs.KindText {
			return seamfs.ReadResult{}, &seamerr.Error{Code: seamfs.CodeBinary, Provider: ProviderName,
				Message: fmt.Sprintf("不是文本文件: %s(%s)", req.Path, kind)}
		}
		out = string(content)
	}
	version, err := versionFor(host)
	if err != nil {
		return seamfs.ReadResult{}, err
	}
	return seamfs.ReadResult{
		Content: out, Truncated: truncated, Version: version,
		Kind: kind, MimeType: mimeFor(host), Size: st.Size(),
	}, nil
}

// Write 原子写:临时文件 + rename;失败不留半文件。
func (f *FS) Write(ctx context.Context, req seamfs.WriteRequest) (seamfs.WriteResult, error) {
	if err := f.checkCtx(ctx); err != nil {
		return seamfs.WriteResult{}, err
	}
	if int64(len(req.Content)) > seamfs.DefaultWriteLimit {
		return seamfs.WriteResult{}, seamfs.TooLarge(fmt.Sprintf("写入超过上限(%d)", seamfs.DefaultWriteLimit))
	}
	host, _, err := f.resolve(ctx, workspace.OpWrite, req.Path)
	if err != nil {
		return seamfs.WriteResult{}, err
	}
	dir := filepath.Dir(host)
	if req.CreateDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return seamfs.WriteResult{}, err
		}
	}
	if req.IfVersion != "" {
		current, err := versionFor(host)
		if err != nil {
			return seamfs.WriteResult{}, seamfs.Conflict(fmt.Sprintf("版本守卫失败(无法读取当前版本): %s", req.Path))
		}
		if current != req.IfVersion {
			return seamfs.WriteResult{}, seamfs.Conflict(fmt.Sprintf("版本已变化: %s", req.Path))
		}
	}

	tmp, err := os.CreateTemp(dir, ".ada-write-*")
	if err != nil {
		return seamfs.WriteResult{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(req.Content); err != nil {
		tmp.Close()
		return seamfs.WriteResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return seamfs.WriteResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return seamfs.WriteResult{}, err
	}
	if err := os.Rename(tmpName, host); err != nil {
		return seamfs.WriteResult{}, err
	}
	version, err := versionFor(host)
	if err != nil {
		return seamfs.WriteResult{}, err
	}
	return seamfs.WriteResult{Version: version, Size: int64(len(req.Content))}, nil
}

// Edit 精确替换:必须带 IfVersion(read-before-edit);多处命中需 All。
func (f *FS) Edit(ctx context.Context, req seamfs.EditRequest) (seamfs.EditResult, error) {
	if err := f.checkCtx(ctx); err != nil {
		return seamfs.EditResult{}, err
	}
	if req.IfVersion == "" {
		return seamfs.EditResult{}, seamfs.Conflict("缺少 IfVersion:必须先读后写")
	}
	if req.Old == "" {
		return seamfs.EditResult{}, seamfs.Invalid("fs-local: Old 不能为空")
	}
	host, _, err := f.resolve(ctx, workspace.OpEdit, req.Path)
	if err != nil {
		return seamfs.EditResult{}, err
	}
	info, err := os.Stat(host)
	if err != nil {
		if os.IsNotExist(err) {
			return seamfs.EditResult{}, seamfs.NotFound(req.Path)
		}
		return seamfs.EditResult{}, err
	}
	if info.Size() > seamfs.DefaultWriteLimit {
		return seamfs.EditResult{}, seamfs.TooLarge("文件超过编辑上限")
	}
	current, err := versionFor(host)
	if err != nil {
		return seamfs.EditResult{}, err
	}
	if current != req.IfVersion {
		return seamfs.EditResult{}, seamfs.Conflict("版本已变化")
	}
	content, err := os.ReadFile(host)
	if err != nil {
		return seamfs.EditResult{}, err
	}
	text := string(content)
	count := strings.Count(text, req.Old)
	if count == 0 {
		return seamfs.EditResult{}, seamfs.Invalid("fs-local: 未命中待替换内容")
	}
	if count > 1 && !req.All {
		return seamfs.EditResult{}, seamfs.Invalid(fmt.Sprintf("命中 %d 处,需要 All=true", count))
	}
	replaced := strings.Replace(text, req.Old, req.New, 1)
	if req.All {
		replaced = strings.ReplaceAll(text, req.Old, req.New)
	}
	write, err := f.Write(ctx, seamfs.WriteRequest{Path: req.Path, Content: []byte(replaced), IfVersion: req.IfVersion})
	if err != nil {
		return seamfs.EditResult{}, err
	}
	return seamfs.EditResult{Replacements: count, Version: write.Version}, nil
}

// List 列目录;递归受 MaxDepth 限制,超过 Limit 返回 ETOOLARGE。
func (f *FS) List(ctx context.Context, req seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	if err := f.checkCtx(ctx); err != nil {
		return nil, err
	}
	host, root, err := f.resolve(ctx, workspace.OpList, req.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(host)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, seamfs.NotFound(req.Path)
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, &seamerr.Error{Code: seamfs.CodeNotDir, Provider: ProviderName, Message: fmt.Sprintf("不是目录: %s", req.Path)}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = seamfs.DefaultListLimit
	}
	depth := 1
	if req.Recursive {
		depth = req.MaxDepth
		if depth <= 0 {
			depth = 32
		}
	}
	virtualRoot := virtualPrefix(root, f)

	var out []seamfs.DirEntry
	err = filepath.WalkDir(host, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(host, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		curDepth := strings.Count(filepath.ToSlash(rel), "/") + 1
		if curDepth > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info2, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		vp := seamfs.Path(virtualRoot + "/" + filepath.ToSlash(mustRel(root, p)))
		kind := seamfs.KindOther
		if d.IsDir() {
			kind = seamfs.KindDir
		} else {
			kind = kindFor(p, prefixBytes(p))
		}
		out = append(out, seamfs.DirEntry{Path: vp, Name: d.Name(), Kind: kind, Size: info2.Size()})
		if len(out) > limit {
			return errListLimit
		}
		return nil
	})
	if err == errListLimit {
		return nil, seamfs.TooLarge(fmt.Sprintf("目录条目超过上限(%d)", limit))
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

var errListLimit = fmt.Errorf("fs-local: 列表超过上限")

func virtualPrefix(root string, f *FS) string {
	if filepath.Clean(root) == filepath.Clean(f.tmp) {
		return "/tmp"
	}
	return "/workspace"
}

// Search 在工作区/临时目录内按字面量搜索文本文件。
//
// 只搜索文本;单文件读取上限 1 MiB;命中数量受 req.Limit 限制(默认 200)。
func (f *FS) Search(ctx context.Context, req seamfs.SearchRequest) (seamfs.SearchResult, error) {
	if err := f.checkCtx(ctx); err != nil {
		return seamfs.SearchResult{}, err
	}
	if req.Query == "" {
		return seamfs.SearchResult{}, seamfs.Invalid("fs-local: 搜索词不能为空")
	}
	host, root, err := f.resolve(ctx, workspace.OpSearch, req.Path)
	if err != nil {
		return seamfs.SearchResult{}, err
	}
	info, err := os.Stat(host)
	if err != nil {
		if os.IsNotExist(err) {
			return seamfs.SearchResult{}, seamfs.NotFound(req.Path)
		}
		return seamfs.SearchResult{}, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	virtualRoot := virtualPrefix(root, f)

	var matches []seamfs.SearchMatch
	addFile := func(p string) error {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.Contains(data, []byte(req.Query)) {
			return nil
		}
		if kindFor(p, prefixBytes(p)) != seamfs.KindText {
			return nil
		}
		vp := seamfs.Path(virtualRoot + "/" + filepath.ToSlash(mustRel(root, p)))
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, req.Query) {
				matches = append(matches, seamfs.SearchMatch{Path: vp, Line: i + 1, Text: strings.TrimSpace(line)})
				if len(matches) >= limit {
					return errSearchLimit
				}
			}
		}
		return nil
	}

	if info.IsDir() {
		err = filepath.WalkDir(host, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			if info2, ierr := d.Info(); ierr == nil && info2.Size() > seamfs.DefaultReadLimit {
				return nil
			}
			return addFile(p)
		})
	} else {
		err = addFile(host)
	}
	if err == errSearchLimit {
		err = nil
	}
	if err != nil {
		return seamfs.SearchResult{}, err
	}
	return seamfs.SearchResult{Matches: matches}, nil
}

// errSearchLimit 用于停止 WalkDir,不是对外错误。
var errSearchLimit = errors.New("fs-local: 搜索结果达到上限")

func mustRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return rel
}

// versionFor 计算版本凭证(有界:最多读 1 MiB 参与哈希)。
func versionFor(host string) (seamfs.Version, error) {
	st, err := os.Stat(host)
	if err != nil {
		return "", err
	}
	content := prefixBytes(host)
	return seamfs.VersionOf(content, st.ModTime(), st.Size()), nil
}

// prefixBytes 读取文件前 1 MiB(有界,版本与类型探测共用)。
func prefixBytes(host string) []byte {
	fh, err := os.Open(host)
	if err != nil {
		return nil
	}
	defer fh.Close()
	buf := make([]byte, seamfs.DefaultReadLimit)
	n, _ := io.ReadFull(fh, buf)
	return buf[:n]
}

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".svg": true,
}

var mimeByExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".svg": "image/svg+xml", ".json": "application/json",
	".md": "text/markdown", ".go": "text/plain", ".ts": "text/plain", ".txt": "text/plain",
	".log": "text/plain", ".yaml": "text/yaml", ".yml": "text/yaml",
}

func kindFor(host string, prefix []byte) seamfs.FileKind {
	if imageExts[strings.ToLower(filepath.Ext(host))] {
		return seamfs.KindImage
	}
	if !utf8.Valid(prefix) {
		return seamfs.KindBinary
	}
	for _, b := range prefix {
		if b == 0 {
			return seamfs.KindBinary
		}
	}
	return seamfs.KindText
}

func mimeFor(host string) string {
	if m := mimeByExt[strings.ToLower(filepath.Ext(host))]; m != "" {
		return m
	}
	return "text/plain"
}

// Plugin 返回 fs-local 插件:inject fs seam,把本地实现安装进 Chain。
//
// workspace 是可选依赖:旧 preset 没有 workspace 栈时完全按自己的根工作。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		var wschain *workspace.Chain
		if v, ok := ctx.TryService(workspace.ServiceName); ok {
			if ch, ok := v.(*workspace.Chain); ok {
				wschain = ch
			}
		}
		workspaceCfg := cfg.String("workspace")
		if wschain != nil && workspaceCfg != "" {
			if svc, gerr := wschain.Get(); gerr == nil {
				target, rerr := svc.ResolveHost(ctx.Context(), workspace.OpRead, workspace.MountRoot)
				if rerr != nil {
					return nil, rerr
				}
				abs, aerr := filepath.Abs(workspaceCfg)
				if aerr != nil || !sameDirectory(abs, target.Root) {
					return nil, workspace.Conflict("fs-local: 配置的 workspace 与 workspace 服务根不一致")
				}
			}
		}
		impl, err := New(Options{
			Workspace:      workspaceCfg,
			Tmp:            cfg.String("tmp"),
			WorkspaceChain: wschain,
		})
		if err != nil {
			return nil, err
		}
		return chain.Install(impl)
	}).WithInject("fs").WithOptional(workspace.ServiceName)
}
