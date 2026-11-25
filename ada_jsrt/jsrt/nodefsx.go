// node:fs 的补齐层(宿主侧):实现 moejs 的 node 包没有提供、但 Node 里常用的
// 元数据类文件操作。
//
// 边界与 moejs 自带的 node:fs 一致:
//   - 路径先映射到虚拟挂载点(/workspace),再用 os.Root 打开真实文件,越界由 os.Root 拒绝;
//   - 写权限沿用 installed 时的 Write 开关,只读工作区返回 EROFS;
//   - 这一层只做「一次操作」,不认识任何后台任务/策略;策略仍在 fs seam 上。
//
// 形态(callback / promise / sync)在本包的 fsx.js 里,这里只产出结果信封:
// {"ok":true,"value":...} 或 {"ok":false,"code":"ENOENT","message":"...","path":"/workspace/x"}。
// 用信封而不是 Go error:JS 需要 Node 风格的 err.code/err.syscall,而不是一段字符串。
package jsrt

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// fsxError 是补齐层对外暴露的 Node 风格错误。
type fsxError struct {
	Code    string
	Path    string
	Message string
}

func (e *fsxError) Error() string { return e.Message }

func fsxErrf(code, p, format string, args ...any) error {
	return &fsxError{Code: code, Path: p, Message: fmt.Sprintf(format, args...)}
}

// fsxResult 是宿主交回 JS 的结果信封。
type fsxResult struct {
	OK      bool   `json:"ok"`
	Value   any    `json:"value,omitempty"`
	Code    string `json:"code,omitempty"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message,omitempty"`
}

// openNodeRoot 打开工作区根的 os.Root;node:fs 的补齐操作与 moejs 的 node:fs 共用同一个根。
//
// 只在装配期调用一次;Root 本身可以并发使用,所以异步操作直接在 goroutine 里用它。
func (r *Runtime) openNodeRoot() error {
	fs := r.fsPerm
	if !fs.Enabled || strings.TrimSpace(fs.Root) == "" {
		return nil
	}
	abs, err := filepath.Abs(fs.Root)
	if err != nil {
		return fmt.Errorf("jsrt: 解析 node:fs 根失败: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return fmt.Errorf("jsrt: 打开 node:fs 根失败: %w", err)
	}
	mount := fs.Mount
	if mount == "" {
		mount = "/workspace"
	}
	r.mu.Lock()
	r.nodeRoot = root
	r.nodeHostRoot = abs
	r.nodeMount = mount
	r.nodeWrite = fs.Access == FSReadWrite
	r.mu.Unlock()
	return nil
}

// closeNodeRoot 释放根句柄;可重复调用。
func (r *Runtime) closeNodeRoot() {
	r.mu.Lock()
	root := r.nodeRoot
	r.nodeRoot = nil
	r.mu.Unlock()
	if root != nil {
		_ = root.Close()
	}
}

// nodeRel 把虚拟路径换算成 os.Root 内的相对路径。
//
// 越界、含 NUL、或是绝对路径落在挂载点外,一律 EPERM:与「路径不在工作区内」同一个回答,
// 免得调用方靠错误差异探测宿主目录结构。
func (r *Runtime) nodeRel(p string) (string, error) {
	r.mu.Lock()
	mount := r.nodeMount
	r.mu.Unlock()
	if mount == "" {
		mount = "/workspace"
	}
	if p == "" {
		return "", fsxErrf("EINVAL", p, "路径不能为空")
	}
	if strings.ContainsRune(p, 0) {
		return "", fsxErrf("EINVAL", p, "路径不能包含 NUL")
	}
	if p == mount || p == mount+"/" {
		return ".", nil
	}
	if !strings.HasPrefix(p, mount+"/") {
		return "", fsxErrf("EPERM", p, "路径不在工作区内: %s", p)
	}
	rel := strings.TrimPrefix(p, mount+"/")
	// 先按相对路径归一,再判越界:用 "/"+rel 归一会把 ".." 折成根目录,
	// 那样 "/workspace/../x" 就悄悄变成了工作区内的 x,越界检查形同虚设。
	rel = path.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fsxErrf("EPERM", p, "路径越出工作区: %s", p)
	}
	if rel == "." || rel == "" {
		return ".", nil
	}
	return rel, nil
}

// nodeWritable 报告 node:fs 是否允许写;只读工作区上的补写操作返回 EROFS。
func (r *Runtime) nodeWritable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodeWrite
}

// nodeRootOf 取根句柄的当前值(可能已被 Close 释放)。
func (r *Runtime) nodeRootOf() *os.Root {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodeRoot
}

// nativeFSXCall 实现 __adaHost.fsx.call(op, argsJSON):同步执行一次补齐操作。
func (r *Runtime) nativeFSXCall(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		op, argsJSON, err := fsxArgsOf(raw, args)
		if err != nil {
			return engine.Undefined(), err
		}
		out := r.fsxDo(op, argsJSON)
		return raw.FromGo(out)
	}
}

// nativeFSXCallAsync 实现 __adaHost.fsx.callAsync(op, argsJSON):返回 Promise。
//
// 文件操作在单独的 goroutine 里做,结算时回到事件循环:这样 await 才真的是异步,
// 而不是把循环卡在系统调用上(循环与调用方是同一个 goroutine)。
func (r *Runtime) nativeFSXCallAsync(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		op, argsJSON, err := fsxArgsOf(raw, args)
		if err != nil {
			return engine.Undefined(), err
		}
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			return engine.Undefined(), fmt.Errorf("fsx: 事件循环不可用")
		}
		// NewPromise 必须在循环上调用(原生函数正是在循环上跑的),它会保持循环存活,
		// 直到 settle 被调用——否则一次「发起后就不再 await」的写会在结算前被丢弃。
		p, settle := loop.NewPromise()
		go func() {
			out := r.fsxDo(op, argsJSON)
			settle(func(rt *moejs.Runtime) (engine.Value, error) { return rt.FromGo(out) })
		}()
		return p, nil
	}
}

// fsxArgsOf 取出 (op, argsJSON)。
func fsxArgsOf(raw *moejs.Runtime, args []engine.Value) (string, string, error) {
	if len(args) < 1 {
		return "", "", fmt.Errorf("fsx.call(op, argsJSON) 需要一个操作名")
	}
	op, err := toString(raw, args[0])
	if err != nil || op == "" {
		return "", "", fmt.Errorf("fsx: 操作名必须是非空字符串")
	}
	argsJSON := "[]"
	if len(args) > 1 {
		if s, err := toString(raw, args[1]); err == nil && s != "" {
			argsJSON = s
		}
	}
	return op, argsJSON, nil
}

// fsxDo 执行一次操作并返回结果信封(永远是合法 JSON,不返回 Go error)。
func (r *Runtime) fsxDo(op, argsJSON string) string {
	value, err := r.fsxRun(op, argsJSON)
	res := fsxResult{}
	if err != nil {
		var pe *PermissionError
		var fe *fsxError
		switch {
		case errors.As(err, &pe):
			res.Code, res.Path, res.Message = pe.Code, pe.Resource, pe.Error()
		case errors.As(err, &fe):
			res.Code, res.Path, res.Message = fe.Code, fe.Path, fe.Message
		default:
			res.Code, res.Message = "EIO", err.Error()
		}
	} else {
		res.OK = true
		res.Value = value
	}
	out, mErr := json.Marshal(res)
	if mErr != nil {
		return `{"ok":false,"code":"EIO","message":"结果无法序列化"}`
	}
	return string(out)
}

// fsxRun 分派具体操作;返回的值会被放进信封的 value。
func (r *Runtime) fsxRun(op, argsJSON string) (any, error) {
	root := r.nodeRootOf()
	if root == nil {
		return nil, fsxErrf("ENOTSUP", "", "node:fs 未启用(没有工作区根)")
	}
	args, err := decodeFSXArgs(argsJSON)
	if err != nil {
		return nil, fsxErrf("EINVAL", "", "参数不是合法 JSON: %v", err)
	}
	if err := r.checkFSXPermission(op, args); err != nil {
		return nil, err
	}
	// 会改文件系统状态的操作:只读工作区一律 EROFS。
	// open 不在这里判断——它是否允许写取决于 flags(见 fsxOpen)。
	switch op {
	case "mkdtemp", "symlink", "link", "chmod", "chown", "truncate", "utimes", "write", "ftruncate", "fchmod", "fchown", "futimes":
		if !r.nodeWritable() {
			return nil, fsxErrf("EROFS", "", "工作区是只读的")
		}
	}
	switch op {
	case "mkdtemp":
		return r.fsxMkdtemp(root, args)
	case "symlink":
		return nil, r.fsxSymlink(root, args)
	case "link":
		return nil, r.fsxLink(root, args)
	case "chmod":
		return nil, r.fsxChmod(root, args)
	case "chown":
		return nil, r.fsxChown(root, args)
	case "truncate":
		return nil, r.fsxTruncate(root, args)
	case "utimes":
		return nil, r.fsxUtimes(root, args)
	// 文件描述符族(见 nodefsfd.go):句柄跨调用存活,由 fd 表管理。
	case "open":
		return r.fsxOpen(root, args)
	case "close":
		return r.fsxClose(args)
	case "read":
		return r.fsxRead(args)
	case "write":
		return r.fsxWrite(args)
	case "fsync":
		return r.fsxFync(args)
	case "ftruncate":
		return r.fsxFtruncate(args)
	case "fstat":
		return r.fsxFstat(args)
	case "fchmod":
		return r.fsxFchmod(args)
	case "fchown":
		return r.fsxFchown(args)
	case "futimes":
		return r.fsxFutimes(root, args)
	// 目录句柄(见 nodefsfd.go):opendir 返回 fd,readdir 按批读。
	case "opendir":
		return r.fsxOpenDir(root, args)
	case "readdir":
		return r.fsxReaddir(args)
	default:
		return nil, fsxErrf("ENOTSUP", "", "未实现的操作: %s", op)
	}
}

func decodeFSXArgs(argsJSON string) ([]any, error) {
	dec := json.NewDecoder(strings.NewReader(argsJSON))
	dec.UseNumber()
	var args []any
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	return args, nil
}

func fsxStringArg(args []any, i int, name string) (string, error) {
	if i >= len(args) {
		return "", fsxErrf("EINVAL", "", "%s 缺失", name)
	}
	s, ok := args[i].(string)
	if !ok || s == "" {
		return "", fsxErrf("EINVAL", "", "%s 必须是非空字符串", name)
	}
	return s, nil
}

func fsxIntArg(args []any, i int, name string) (int64, error) {
	if i >= len(args) {
		return 0, fsxErrf("EINVAL", "", "%s 缺失", name)
	}
	n, ok := args[i].(json.Number)
	if !ok {
		return 0, fsxErrf("EINVAL", "", "%s 必须是数字", name)
	}
	v, err := n.Int64()
	if err != nil {
		return 0, fsxErrf("EINVAL", "", "%s 必须是整数", name)
	}
	return v, nil
}

func fsxFloatArg(args []any, i int, name string) (float64, error) {
	if i >= len(args) {
		return 0, fsxErrf("EINVAL", "", "%s 缺失", name)
	}
	n, ok := args[i].(json.Number)
	if !ok {
		return 0, fsxErrf("EINVAL", "", "%s 必须是数字", name)
	}
	v, err := n.Float64()
	if err != nil {
		return 0, fsxErrf("EINVAL", "", "%s 必须是数字", name)
	}
	return v, nil
}

// fsxMkdtemp 实现 fs.mkdtemp:在 prefix 所在目录里建一个带随机后缀的目录,返回虚拟路径。
//
// 不用 os.MkdirTemp:它拿宿主绝对路径工作,绕过了 os.Root 的符号链接判定;
// 这里自己在 Root 里逐个试名字,越界与链接都由 Root 兜住。
func (r *Runtime) fsxMkdtemp(root *os.Root, args []any) (any, error) {
	prefix, err := fsxStringArg(args, 0, "prefix")
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	mount := r.nodeMount
	r.mu.Unlock()
	if mount == "" {
		mount = "/workspace"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = mount + "/" + prefix
	}
	dir, name := path.Split(prefix)
	if name == "" {
		name = "tmp-"
	}
	relDir, err := r.nodeRel(strings.TrimSuffix(dir, "/"))
	if err != nil {
		return nil, err
	}
	for i := 0; i < 10; i++ {
		suffix, sErr := randomSuffix()
		if sErr != nil {
			return nil, fsxErrf("EIO", prefix, "生成临时目录名失败: %v", sErr)
		}
		rel := path.Join(relDir, name+suffix)
		if mkErr := root.Mkdir(rel, 0o700); mkErr != nil {
			if errors.Is(mkErr, fs.ErrExist) {
				continue
			}
			return nil, fsxNodeError(mkErr, prefix)
		}
		return path.Join(dir, name+suffix), nil
	}
	return nil, fsxErrf("EEXIST", prefix, "连续 10 次都撞名: %s", prefix)
}

func (r *Runtime) fsxSymlink(root *os.Root, args []any) error {
	target, err := fsxStringArg(args, 0, "target")
	if err != nil {
		return err
	}
	p, err := fsxStringArg(args, 1, "path")
	if err != nil {
		return err
	}
	rel, err := r.nodeRel(p)
	if err != nil {
		return err
	}
	// os.Root 要求链接目标相对且不越界;Node 允许绝对目标,这里给出明确的 EPERM
	// 而不是让 os.Root 报一个含糊的错误。
	if path.IsAbs(target) {
		return fsxErrf("EPERM", p, "符号链接目标必须是相对路径(os.Root 不允许绝对目标)")
	}
	if lErr := root.Symlink(target, rel); lErr != nil {
		return fsxNodeError(lErr, p)
	}
	return nil
}

func (r *Runtime) fsxLink(root *os.Root, args []any) error {
	oldPath, err := fsxStringArg(args, 0, "existingPath")
	if err != nil {
		return err
	}
	newPath, err := fsxStringArg(args, 1, "newPath")
	if err != nil {
		return err
	}
	relOld, err := r.nodeRel(oldPath)
	if err != nil {
		return err
	}
	relNew, err := r.nodeRel(newPath)
	if err != nil {
		return err
	}
	if lErr := root.Link(relOld, relNew); lErr != nil {
		return fsxNodeError(lErr, oldPath)
	}
	return nil
}

func (r *Runtime) fsxChmod(root *os.Root, args []any) error {
	p, err := fsxStringArg(args, 0, "path")
	if err != nil {
		return err
	}
	mode, err := fsxIntArg(args, 1, "mode")
	if err != nil {
		return err
	}
	rel, err := r.nodeRel(p)
	if err != nil {
		return err
	}
	if cErr := root.Chmod(rel, os.FileMode(uint32(mode))); cErr != nil {
		return fsxNodeError(cErr, p)
	}
	return nil
}

func (r *Runtime) fsxChown(root *os.Root, args []any) error {
	p, err := fsxStringArg(args, 0, "path")
	if err != nil {
		return err
	}
	uid, err := fsxIntArg(args, 1, "uid")
	if err != nil {
		return err
	}
	gid, err := fsxIntArg(args, 2, "gid")
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Windows 没有 POSIX 属主;Node 在 Windows 上同样不支持。
		return fsxErrf("ENOTSUP", p, "当前平台不支持 chown")
	}
	rel, err := r.nodeRel(p)
	if err != nil {
		return err
	}
	if cErr := root.Chown(rel, int(uid), int(gid)); cErr != nil {
		return fsxNodeError(cErr, p)
	}
	return nil
}

func (r *Runtime) fsxTruncate(root *os.Root, args []any) error {
	p, err := fsxStringArg(args, 0, "path")
	if err != nil {
		return err
	}
	size, err := fsxIntArg(args, 1, "len")
	if err != nil {
		return err
	}
	if size < 0 {
		return fsxErrf("EINVAL", p, "len 不能为负")
	}
	if max := r.fsMaxFileBytes(); max > 0 && size > max {
		return fsxErrf("EFBIG", p, "truncate 超过 MaxFileBytes(%d)", max)
	}
	rel, err := r.nodeRel(p)
	if err != nil {
		return err
	}
	f, oErr := root.OpenFile(rel, os.O_WRONLY, 0)
	if oErr != nil {
		return fsxNodeError(oErr, p)
	}
	defer func() { _ = f.Close() }()
	if tErr := f.Truncate(size); tErr != nil {
		return fsxNodeError(tErr, p)
	}
	return nil
}

func (r *Runtime) fsxUtimes(root *os.Root, args []any) error {
	p, err := fsxStringArg(args, 0, "path")
	if err != nil {
		return err
	}
	atime, err := fsxFloatArg(args, 1, "atime")
	if err != nil {
		return err
	}
	mtime, err := fsxFloatArg(args, 2, "mtime")
	if err != nil {
		return err
	}
	rel, err := r.nodeRel(p)
	if err != nil {
		return err
	}
	toTime := func(sec float64) time.Time {
		nsec := int64(sec * float64(time.Second))
		return time.Unix(0, nsec)
	}
	if tErr := root.Chtimes(rel, toTime(atime), toTime(mtime)); tErr != nil {
		return fsxNodeError(tErr, p)
	}
	return nil
}

// randomSuffix 生成 6 位十六进制后缀(Node 的 mkdtemp 是 6 位)。
func randomSuffix() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// fsxNodeError 把 os 错误映射成 Node 的错误码。
//
// 只认跨平台稳定的几类(errors.Is + io/fs);其余落 EIO 并保留原始消息。
// 不按 syscall.Errno 细分:那些常量在 windows 构建下并不齐全,而差分错误码
// 只会把平台差异泄露给脚本。
func fsxNodeError(err error, p string) error {
	code := "EIO"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = "ENOENT"
	case errors.Is(err, fs.ErrExist):
		code = "EEXIST"
	case errors.Is(err, fs.ErrPermission):
		code = "EACCES"
	case errors.Is(err, fs.ErrInvalid):
		code = "EINVAL"
	}
	return fsxErrf(code, p, "%s", err.Error())
}

// checkFSXPermission 在补齐层执行前做路径级权限检查。
//
// 只在启用新的 Permissions.FS 时生效；旧的 Node.FS 保持原有行为。
// 带路径的操作按 read/write 检查；没有路径的 fd 操作只校验整体写权限，
// 因为打开 fd 时已经检查过路径。
func (r *Runtime) checkFSXPermission(op string, args []any) error {
	if !r.fsEnforce || r.fsPolicy == nil {
		return nil
	}
	vpath, write, hasPath := fsxPermissionTarget(op, args)
	opName := "read"
	if write {
		opName = "write"
	}
	var err error
	switch {
	case hasPath && vpath != "":
		// fsx 的路径是未解析的虚拟路径；先解析符号链接，再做权限判定，
		// 避免「允许的链接目录指向被拒绝目录」绕过 Allow/Deny。
		resolved, rerr := r.resolveFSXVirtual(vpath)
		if rerr != nil {
			r.emitEvent(Event{
				Kind: "permission", Err: rerr, Scope: "fs", Op: opName,
				Resource: vpath, Allowed: false,
			})
			return rerr
		}
		vpath = resolved
		err = r.fsPolicy.check(opName, resolved)
	case write:
		err = r.fsPolicy.checkAccess("write")
	}
	if err != nil {
		r.emitEvent(Event{
			Kind: "permission", Err: err, Scope: "fs", Op: opName,
			Resource: vpath, Allowed: false,
		})
	}
	return err
}

// fsxPermissionTarget 给出一次 fsx 操作要检查的虚拟路径与读写类型。
//
// 返回 hasPath=false 表示该操作没有路径（fd 族），由调用方决定是否校验
// 整体写权限；返回 write=true 表示会改变文件系统。
func fsxPermissionTarget(op string, args []any) (vpath string, write, hasPath bool) {
	str := func(i int) (string, bool) {
		if i >= len(args) {
			return "", false
		}
		s, ok := args[i].(string)
		return s, ok
	}
	switch op {
	case "mkdtemp", "chmod", "chown", "truncate", "utimes":
		p, ok := str(0)
		return p, true, ok
	case "symlink", "link":
		p, ok := str(1)
		return p, true, ok
	case "open":
		p, ok := str(0)
		if !ok {
			return "", false, false
		}
		flags, _ := str(1)
		return p, strings.ContainsAny(flags, "wa+"), true
	case "opendir":
		p, ok := str(0)
		return p, false, ok
	case "write", "writev", "ftruncate", "fchmod", "fchown", "futimes", "fsync":
		return "", true, false
	case "read", "readv", "fstat":
		return "", false, false
	default:
		return "", false, false
	}
}

// resolveFSXVirtual 把虚拟路径中的符号链接解析到根内的真实位置，再换回虚拟路径。
//
// fsx 的检查发生在其 Go 操作之前；moejs 核心 fs 的 Allow 已经是解析后的路径。
// 这里做同样的解析，避免「/workspace/allowed/link -> /workspace/secret」这类
// 链接绕过 Allow/Deny。解析失败（路径尚不存在）时退化为解析父目录。
func (r *Runtime) resolveFSXVirtual(v string) (string, error) {
	if r.fsPolicy == nil {
		return v, nil
	}
	clean, err := r.fsPolicy.virtual(v)
	if err != nil {
		return "", err
	}
	mount := r.fsPolicy.perm.Mount
	r.mu.Lock()
	rootDir, runtimeMount := r.nodeHostRoot, r.nodeMount
	r.mu.Unlock()
	if runtimeMount != "" {
		mount = runtimeMount
	}
	if rootDir == "" {
		return clean, nil
	}
	if clean != mount && !strings.HasPrefix(clean, mount+"/") {
		// 交给 check 统一报「不在 Mount 内」。
		return clean, nil
	}
	rel := "."
	if clean != mount {
		rel = strings.TrimPrefix(clean, mount+"/")
	}
	host := filepath.Join(rootDir, filepath.FromSlash(rel))
	resolved, err := filepath.EvalSymlinks(host)
	if err != nil {
		parent := filepath.Dir(host)
		base := filepath.Base(host)
		if rp, perr := filepath.EvalSymlinks(parent); perr == nil {
			resolved = filepath.Join(rp, base)
		} else {
			resolved = host
		}
	}
	relResolved, err := filepath.Rel(rootDir, resolved)
	if err != nil || relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(filepath.Separator)) {
		return "", deny("fs", "read", v, "符号链接越界", "EACCES")
	}
	if relResolved == "." {
		return mount, nil
	}
	return path.Join(mount, filepath.ToSlash(relResolved)), nil
}

// nativeFSXWatchCheck 在 fs.watch 启动前检查 Watch 权限与路径读权限。
func (r *Runtime) nativeFSXWatchCheck(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("fsx.watchCheck(path) 需要 path")
		}
		p, err := toString(raw, args[0])
		if err != nil || p == "" {
			return engine.Undefined(), fsxErrf("EINVAL", "", "watch 路径必须是非空字符串")
		}
		if !r.fsPerm.Watch {
			perr := deny("fs", "watch", p, "Watch 未启用", "ERR_ACCESS_DENIED")
			r.emitPermission("fs", "watch", p, perr)
			return engine.Undefined(), perr
		}
		if r.fsPolicy != nil {
			if cerr := r.fsPolicy.check("read", p); cerr != nil {
				r.emitPermission("fs", "watch", p, cerr)
				return engine.Undefined(), cerr
			}
		}
		return engine.Undefined(), nil
	}
}
