// node:fs 的文件描述符族:open / read / write / close / fsync / ftruncate / fstat。
//
// 为什么放在执行层:句柄必须跨调用存活(插件先 open 再在另一条 await 里 read),
// 而策略与路径约束仍然由 os.Root + 工作区写开关负责,与 node:fs 其余部分同一条边界。
//
// 边界:
//   - 句柄只在本运行时内有效,进程退出/运行时 Close 时统一关闭(不会泄漏到宿主);
//   - 单次读写上限 8 MiB,同时打开的句柄上限 256;
//   - flags 只接受 Node 的字符串写法(r / r+ / w / w+ / a / a+),
//     数字 flags 的平台差异太大,不在这里冒充;
//   - 并发发起、且没有 await 的多个操作**不保证完成顺序**(Node 也只保证入队,
//     不保证完成顺序);需要顺序时只需 await 上一个。
package jsrt

import (
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"os"
	"runtime"
	"time"
)

const (
	// maxFDReadSize 是单次读的上限(与 fsx 的输入上限同量级)。
	maxFDReadSize = 8 << 20
	// maxOpenFDs 是同时打开的句柄上限:超过就报 EMFILE,而不是把宿主 fd 耗尽。
	maxOpenFDs = 256
)

// fdOpenFlags 把 Node 的字符串 flags 映射成 os 标志,并报告是否允许写。
func fdOpenFlags(flags string) (int, bool, error) {
	switch flags {
	case "", "r":
		return os.O_RDONLY, false, nil
	case "r+":
		return os.O_RDWR, true, nil
	case "w":
		return os.O_WRONLY | os.O_CREATE | os.O_TRUNC, true, nil
	case "w+":
		return os.O_RDWR | os.O_CREATE | os.O_TRUNC, true, nil
	case "a":
		return os.O_WRONLY | os.O_CREATE | os.O_APPEND, true, nil
	case "a+":
		return os.O_RDWR | os.O_CREATE | os.O_APPEND, true, nil
	}
	return 0, false, fsxErrf("EINVAL", "", "不支持的 flags: %q(只支持 r/r+/w/w+/a/a+)", flags)
}

// fdEntry 是一个打开的句柄:文件对象 + 它在工作区里的相对路径。
//
// 为什么要记相对路径:futimes 只能按路径改时间(os.File 没有 Chtimes),而路径必须是
// 工作区内的相对路径,才能继续交给 os.Root 做越界与符号链接判定。
type fdEntry struct {
	f       *os.File
	rel     string
	dirRead int64
}

// fdPut 登记一个句柄,返回给 JS 的数字。
func (r *Runtime) fdPut(f *os.File, rel string) (int64, error) {
	r.fdMu.Lock()
	defer r.fdMu.Unlock()
	limit := r.fsMaxOpenFiles()
	if len(r.fds) >= limit {
		_ = f.Close()
		return 0, fsxErrf("EMFILE", "", "打开的文件过多(上限 %d)", limit)
	}
	r.fdSeq++
	id := r.fdSeq
	r.fds[id] = &fdEntry{f: f, rel: rel}
	return int64(id), nil
}

// fdRel 取句柄在工作区里的相对路径(给按路径操作的 fd 级 API 用)。
func (r *Runtime) fdRel(id int64) (string, error) {
	r.fdMu.Lock()
	defer r.fdMu.Unlock()
	entry, ok := r.fds[uint64(id)]
	if !ok {
		return "", fsxErrf("EBADF", "", "无效的文件描述符: %d", id)
	}
	return entry.rel, nil
}

// fsMaxOpenFiles 返回同时打开句柄的上限；0 表示默认值。
func (r *Runtime) fsMaxOpenFiles() int {
	if r.fsPerm.MaxOpenFiles > 0 {
		return r.fsPerm.MaxOpenFiles
	}
	return maxOpenFDs
}

// fsMaxFileBytes 返回单文件大小上限；0 表示不限。
func (r *Runtime) fsMaxFileBytes() int64 {
	if r.fsPerm.MaxFileBytes > 0 {
		return r.fsPerm.MaxFileBytes
	}
	return 0
}

// fsMaxDirEntries 返回单目录条目上限；0 表示不限。
func (r *Runtime) fsMaxDirEntries() int64 {
	if r.fsPerm.MaxDirEntries > 0 {
		return int64(r.fsPerm.MaxDirEntries)
	}
	return 0
}

// fdGet 取句柄;不存在返回 EBADF。
func (r *Runtime) fdGet(id int64) (*os.File, error) {
	r.fdMu.Lock()
	defer r.fdMu.Unlock()
	entry, ok := r.fds[uint64(id)]
	if !ok {
		return nil, fsxErrf("EBADF", "", "无效的文件描述符: %d", id)
	}
	return entry.f, nil
}

// fdDrop 删除并关闭句柄。
func (r *Runtime) fdDrop(id int64) error {
	r.fdMu.Lock()
	entry, ok := r.fds[uint64(id)]
	delete(r.fds, uint64(id))
	r.fdMu.Unlock()
	if !ok {
		return fsxErrf("EBADF", "", "无效的文件描述符: %d", id)
	}
	if err := entry.f.Close(); err != nil {
		return fsxNodeError(err, "")
	}
	return nil
}

// closeAllFDs 在运行时关闭时清掉所有句柄(可重复调用)。
func (r *Runtime) closeAllFDs() {
	r.fdMu.Lock()
	files := r.fds
	r.fds = make(map[uint64]*fdEntry)
	r.fdMu.Unlock()
	for _, entry := range files {
		_ = entry.f.Close()
	}
}

// fsxOpen 实现 open(path, flags, mode)。
func (r *Runtime) fsxOpen(root *os.Root, args []any) (any, error) {
	p, err := fsxStringArg(args, 0, "path")
	if err != nil {
		return nil, err
	}
	flags := "r"
	if len(args) > 1 {
		if s, ok := args[1].(string); ok && s != "" {
			flags = s
		}
	}
	mode := int64(0o666)
	if len(args) > 2 {
		if m, mErr := fsxIntArg(args, 2, "mode"); mErr == nil {
			mode = m
		}
	}
	oflag, writable, err := fdOpenFlags(flags)
	if err != nil {
		return nil, err
	}
	if writable && !r.nodeWritable() {
		return nil, fsxErrf("EROFS", p, "工作区是只读的")
	}
	rel, err := r.nodeRel(p)
	if err != nil {
		return nil, err
	}
	f, oErr := root.OpenFile(rel, oflag, os.FileMode(uint32(mode)))
	if oErr != nil {
		return nil, fsxNodeError(oErr, p)
	}
	return r.fdPut(f, rel)
}

// fsxClose 实现 close(fd)。
func (r *Runtime) fsxClose(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	if err := r.fdDrop(id); err != nil {
		return nil, err
	}
	return nil, nil
}

// fsxRead 实现 read(fd, length, position):返回 {data(base64), bytesRead}。
//
// position < 0 表示「从当前偏移继续读」,与 Node 的 null/undefined 语义一致。
func (r *Runtime) fsxRead(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	length, err := fsxIntArg(args, 1, "length")
	if err != nil {
		return nil, err
	}
	if length < 0 || length > maxFDReadSize {
		return nil, fsxErrf("EINVAL", "", "单次读取长度必须在 0..%d 之间", maxFDReadSize)
	}
	position := int64(-1)
	if len(args) > 2 {
		if p, pErr := fsxIntArg(args, 2, "position"); pErr == nil {
			position = p
		}
	}
	f, err := r.fdGet(id)
	if err != nil {
		return nil, err
	}
	if max := r.fsMaxFileBytes(); max > 0 {
		if st, statErr := f.Stat(); statErr == nil && st.Size() > max {
			return nil, fsxErrf("EFBIG", "", "文件超过 MaxFileBytes(%d)", max)
		}
	}
	buf := make([]byte, length)
	var n int
	if position >= 0 {
		n, err = f.ReadAt(buf, position)
	} else {
		n, err = f.Read(buf)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fsxNodeError(err, "")
	}
	return map[string]any{
		"data":      base64.StdEncoding.EncodeToString(buf[:n]),
		"bytesRead": n,
	}, nil
}

// fsxWrite 实现 write(fd, dataBase64, position):返回写入字节数。
func (r *Runtime) fsxWrite(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	if len(args) < 2 {
		return nil, fsxErrf("EINVAL", "", "data 缺失")
	}
	text, ok := args[1].(string)
	if !ok {
		return nil, fsxErrf("EINVAL", "", "data 必须是 base64 字符串")
	}
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fsxErrf("EINVAL", "", "data 不是合法的 base64")
	}
	if len(data) > maxFDReadSize {
		return nil, fsxErrf("EINVAL", "", "单次写入不能超过 %d 字节", maxFDReadSize)
	}
	position := int64(-1)
	if len(args) > 2 {
		if p, pErr := fsxIntArg(args, 2, "position"); pErr == nil {
			position = p
		}
	}
	f, err := r.fdGet(id)
	if err != nil {
		return nil, err
	}
	if max := r.fsMaxFileBytes(); max > 0 {
		st, statErr := f.Stat()
		if statErr != nil {
			return nil, fsxNodeError(statErr, "")
		}
		end := st.Size() + int64(len(data))
		if position >= 0 {
			end = position + int64(len(data))
		}
		if st.Size() > max || end > max {
			return nil, fsxErrf("EFBIG", "", "写入超过 MaxFileBytes(%d)", max)
		}
	}
	var n int
	if position >= 0 {
		n, err = f.WriteAt(data, position)
	} else {
		n, err = f.Write(data)
	}
	if err != nil {
		return nil, fsxNodeError(err, "")
	}
	return n, nil
}

// fsxFync 实现 fsync(fd)。
func (r *Runtime) fsxFync(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	f, err := r.fdGet(id)
	if err != nil {
		return nil, err
	}
	if sErr := f.Sync(); sErr != nil {
		return nil, fsxNodeError(sErr, "")
	}
	return nil, nil
}

// fsxFtruncate 实现 ftruncate(fd, len)。
func (r *Runtime) fsxFtruncate(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	size, err := fsxIntArg(args, 1, "len")
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, fsxErrf("EINVAL", "", "len 不能为负")
	}
	if max := r.fsMaxFileBytes(); max > 0 && size > max {
		return nil, fsxErrf("EFBIG", "", "truncate 超过 MaxFileBytes(%d)", max)
	}
	f, err := r.fdGet(id)
	if err != nil {
		return nil, err
	}
	if tErr := f.Truncate(size); tErr != nil {
		return nil, fsxNodeError(tErr, "")
	}
	return nil, nil
}

// fsxOpenDir 实现 opendir(path):只读打开一个目录,句柄进 fd 表。
//
// 用 root.Open 而不是 OpenFile:目录在 Windows 上不能按普通文件打开,
// 而 os.Open 对目录是可行的;先 Stat 再判 IsDir,给出明确的 ENOTDIR。
func (r *Runtime) fsxOpenDir(root *os.Root, args []any) (any, error) {
	p, err := fsxStringArg(args, 0, "path")
	if err != nil {
		return nil, err
	}
	rel, err := r.nodeRel(p)
	if err != nil {
		return nil, err
	}
	f, oErr := root.Open(rel)
	if oErr != nil {
		return nil, fsxNodeError(oErr, p)
	}
	st, sErr := f.Stat()
	if sErr != nil {
		_ = f.Close()
		return nil, fsxNodeError(sErr, p)
	}
	if !st.IsDir() {
		_ = f.Close()
		return nil, fsxErrf("ENOTDIR", p, "不是目录: %s", p)
	}
	return r.fdPut(f, rel)
}

// fsxReaddir 实现 readdir(fd, count):读一批目录项,返回 {entries, eof}。
//
// eof 表示「已经读完」——JS 的 Dir.read() 据此返回 null,而不是靠空数组猜。
func (r *Runtime) fsxReaddir(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	count := int64(1)
	if len(args) > 1 {
		if n, nErr := fsxIntArg(args, 1, "count"); nErr == nil {
			count = n
		}
	}
	if count < 1 {
		count = 1
	}
	if count > 1024 {
		count = 1024
	}
	f, err := r.fdGet(id)
	if err != nil {
		return nil, err
	}
	if max := r.fsMaxDirEntries(); max > 0 {
		r.fdMu.Lock()
		entry := r.fds[uint64(id)]
		seen := int64(0)
		if entry != nil {
			seen = entry.dirRead
		}
		r.fdMu.Unlock()
		if seen >= max {
			return nil, fsxErrf("EFBIG", "", "目录条目超过 MaxDirEntries(%d)", max)
		}
		if remaining := max - seen; int64(count) > remaining {
			count = remaining
		}
	}
	list, rErr := f.ReadDir(int(count))
	eof := errors.Is(rErr, io.EOF)
	if rErr != nil && !eof {
		return nil, fsxNodeError(rErr, "")
	}
	r.fdMu.Lock()
	if entry := r.fds[uint64(id)]; entry != nil {
		entry.dirRead += int64(len(list))
	}
	r.fdMu.Unlock()
	entries := make([]map[string]any, 0, len(list))
	for _, e := range list {
		kind := "other"
		switch {
		case e.IsDir():
			kind = "dir"
		case e.Type()&fs.ModeSymlink != 0:
			kind = "symlink"
		case e.Type().IsRegular():
			kind = "file"
		}
		entries = append(entries, map[string]any{"name": e.Name(), "type": kind})
	}
	return map[string]any{"entries": entries, "eof": eof}, nil
}

// fsxFstat 实现 fstat(fd) 的最小字段集(大小、模式、修改时间、是否普通文件)。
func (r *Runtime) fsxFstat(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	f, err := r.fdGet(id)
	if err != nil {
		return nil, err
	}
	st, sErr := f.Stat()
	if sErr != nil {
		return nil, fsxNodeError(sErr, "")
	}
	return map[string]any{
		"size":    st.Size(),
		"mode":    uint32(st.Mode().Perm()),
		"mtimeMs": float64(st.ModTime().UnixMilli()),
		"isFile":  st.Mode().IsRegular(),
	}, nil
}

// fsxFchmod 实现 fchmod(fd, mode):按句柄改权限,不经过路径解析(没有 TOCTOU)。
func (r *Runtime) fsxFchmod(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	mode, err := fsxIntArg(args, 1, "mode")
	if err != nil {
		return nil, err
	}
	f, err := r.fdGet(id)
	if err != nil {
		return nil, err
	}
	if cErr := f.Chmod(os.FileMode(uint32(mode))); cErr != nil {
		return nil, fsxNodeError(cErr, "")
	}
	return nil, nil
}

// fsxFchown 实现 fchown(fd, uid, gid);Windows 没有 POSIX 属主,返回 ENOTSUP。
func (r *Runtime) fsxFchown(args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	uid, err := fsxIntArg(args, 1, "uid")
	if err != nil {
		return nil, err
	}
	gid, err := fsxIntArg(args, 2, "gid")
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		return nil, fsxErrf("ENOTSUP", "", "当前平台不支持 fchown")
	}
	f, err := r.fdGet(id)
	if err != nil {
		return nil, err
	}
	if cErr := f.Chown(int(uid), int(gid)); cErr != nil {
		return nil, fsxNodeError(cErr, "")
	}
	return nil, nil
}

// fsxFutimes 实现 futimes(fd, atime, mtime)。
//
// os.File 没有 Chtimes,只能按路径改时间;路径取句柄登记时的相对路径,
// 再交给 os.Root,越界与符号链接判定与其余 fs 操作一致(Unix 上仍有与 Root.Chtimes
// 相同的竞态窗口,见 os.Root 文档)。
func (r *Runtime) fsxFutimes(root *os.Root, args []any) (any, error) {
	id, err := fsxIntArg(args, 0, "fd")
	if err != nil {
		return nil, err
	}
	atime, err := fsxFloatArg(args, 1, "atime")
	if err != nil {
		return nil, err
	}
	mtime, err := fsxFloatArg(args, 2, "mtime")
	if err != nil {
		return nil, err
	}
	rel, err := r.fdRel(id)
	if err != nil {
		return nil, err
	}
	toTime := func(sec float64) time.Time {
		return time.Unix(0, int64(sec*float64(time.Second)))
	}
	if tErr := root.Chtimes(rel, toTime(atime), toTime(mtime)); tErr != nil {
		return nil, fsxNodeError(tErr, "")
	}
	return nil, nil
}
