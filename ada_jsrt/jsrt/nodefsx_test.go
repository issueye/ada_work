package jsrt_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

// newFSExtRuntime 装配一个装了 node:fs 的运行时;readOnly 决定写权限。
func newFSExtRuntime(t *testing.T, files map[string]string, readOnly bool) (*jsrt.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	rt := newRuntime(t, files, jsrt.Options{Node: &jsrt.NodeOptions{
		Buffer: true,
		FS:     &jsrt.NodeFSOptions{Root: dir, Mount: "/workspace", Cwd: "/workspace", Write: !readOnly},
	}})
	return rt, dir
}

// 1. 补齐层三种形态都能用:sync / callback / promise,并且真的落到磁盘。
func TestNodeFSExtThreeForms(t *testing.T) {
	rt, dir := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import fs from "node:fs"
import { mkdtemp as mkdtempAsync } from "node:fs/promises"

export async function run(): Promise<any> {
  const syncDir = fs.mkdtempSync("/workspace/s-")
  const promiseDir = await mkdtempAsync("/workspace/a-")
  const callbackDir = await new Promise<string>((resolve, reject) => {
    fs.mkdtemp("/workspace/c-", (err: any, p: string) => (err ? reject(err) : resolve(p)))
  })
  fs.writeFileSync("/workspace/t.txt", "0123456789")
  fs.truncateSync("/workspace/t.txt", 4)
  await fs.promises.truncate("/workspace/t.txt", 4)
  await mkdtempAsync("/workspace/a2-")
  const afterTruncate = fs.readFileSync("/workspace/t.txt", "utf8")
  fs.utimesSync("/workspace/t.txt", 1000, 2000)
  fs.chmodSync("/workspace/t.txt", 0o600)
  fs.symlinkSync("t.txt", "/workspace/link.txt")
  fs.linkSync("/workspace/t.txt", "/workspace/hard.txt")
  return { syncDir, promiseDir, callbackDir, afterTruncate }
}`,
	}, false)

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("返回值应为对象,实际 %T(%v)", got, got)
	}
	for _, key := range []string{"syncDir", "promiseDir", "callbackDir"} {
		p, _ := obj[key].(string)
		if !strings.HasPrefix(p, "/workspace/") {
			t.Fatalf("%s 应返回虚拟路径,实际 %q", key, p)
		}
		if st, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(p, "/workspace/"))); err != nil || !st.IsDir() {
			t.Fatalf("%s 对应的目录没有建出来: %v", key, err)
		}
	}
	if s, _ := obj["afterTruncate"].(string); s != "0123" {
		t.Fatalf("truncate 后应为 0123,实际 %q", s)
	}
	if st, err := os.Stat(filepath.Join(dir, "t.txt")); err != nil {
		t.Fatalf("文件不存在: %v", err)
	} else if st.ModTime().Unix() != 2000 {
		t.Fatalf("utimes 没有生效: mtime=%d", st.ModTime().Unix())
	}
	if target, err := os.Readlink(filepath.Join(dir, "link.txt")); err != nil || target != "t.txt" {
		t.Fatalf("symlink 不对: %q %v", target, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "hard.txt")); err != nil || st.Size() != 4 {
		t.Fatalf("硬链接不对: %v", err)
	}
}

// 2. 错误码:越界、只读、不存在,都给 Node 风格的 code。
func TestNodeFSExtErrorCodes(t *testing.T) {
	const src = `
import fs from "node:fs"

export function probe(): any {
  const out: any = {}
  try { fs.mkdtempSync("/etc/tmp-") } catch (e: any) { out.escape = e.code }
  try { fs.mkdtempSync("/workspace/../out-") } catch (e: any) { out.dotdot = e.code }
  try { fs.mkdtempSync("/workspace/missing/sub-") } catch (e: any) { out.missing = e.code }
  try { fs.chmodSync("/workspace/nope.txt", 384) } catch (e: any) { out.noent = e.code }
  try { fs.symlinkSync("/etc/passwd", "/workspace/abs.txt") } catch (e: any) { out.absolute = e.code }
  return out
}`

	rt, _ := newFSExtRuntime(t, map[string]string{"plugin.ts": src}, false)
	got, err := rt.Call(context.Background(), "probe")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	want := map[string]string{
		"escape":   "EPERM",
		"dotdot":   "EPERM",
		"missing":  "ENOENT",
		"noent":    "ENOENT",
		"absolute": "EPERM",
	}
	for key, code := range want {
		if got, _ := obj[key].(string); got != code {
			t.Fatalf("%s 期望 %s,实际 %q(全部结果 %v)", key, code, got, obj)
		}
	}
}

// 3. 只读工作区:补齐层的写操作一律 EROFS。
func TestNodeFSExtReadOnly(t *testing.T) {
	rt, _ := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import fs from "node:fs"
export function probe(): string {
  try { fs.mkdtempSync("/workspace/x-") } catch (e: any) { return e.code }
  return "no-error"
}`,
	}, true)

	got, err := rt.Call(context.Background(), "probe")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if s, _ := got.(string); s != "EROFS" {
		t.Fatalf("只读工作区应返回 EROFS,实际 %q", got)
	}
}

// 5. 文件描述符族:open/write/fsync/fstat/read/close,关闭后 EBADF。
func TestNodeFSFileDescriptors(t *testing.T) {
	rt, dir := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import fs from "node:fs"

export function roundtrip(): any {
  const fd = fs.openSync("/workspace/fd.txt", "w+")
  const written = fs.writeSync(fd, "hello-fd")
  fs.fsyncSync(fd)
  const st = fs.fstatSync(fd)
  const buf = Buffer.alloc(8)
  const bytesRead = fs.readSync(fd, buf, 0, 8, 0)
  fs.closeSync(fd)
  let bad = "no-error"
  try {
    fs.readSync(fd, buf, 0, 1, 0)
  } catch (e: any) {
    bad = e.code
  }
  return { written, size: st.size, bytesRead, text: buf.toString("utf8", 0, bytesRead), bad }
}`,
	}, false)

	got, err := rt.Call(context.Background(), "roundtrip")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if n := asInt(t, obj["written"]); n != 8 {
		t.Fatalf("writeSync 应返回 8,实际 %v", n)
	}
	if n := asInt(t, obj["size"]); n != 8 {
		t.Fatalf("fstat.size 应为 8,实际 %v", n)
	}
	if s, _ := obj["text"].(string); s != "hello-fd" {
		t.Fatalf("读回内容不对: %q", s)
	}
	if s, _ := obj["bad"].(string); s != "EBADF" {
		t.Fatalf("关闭后的读应是 EBADF,实际 %q", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "fd.txt")); err != nil {
		t.Fatalf("文件没有落盘: %v", err)
	}
}

// 6. createWriteStream / createReadStream:写完整、读回同样内容,且能按块迭代。
func TestNodeFSFileStreams(t *testing.T) {
	rt, _ := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import fs from "node:fs"

function collect(stream: any): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const parts: string[] = []
    stream.on("data", (c: any) => parts.push(String(c)))
    stream.on("end", () => resolve(parts.join("")))
    stream.on("error", (e: any) => reject(e))
  })
}

export async function run(): Promise<any> {
  await new Promise<void>((resolve, reject) => {
    const w = fs.createWriteStream("/workspace/stream.txt")
    w.on("finish", () => resolve())
    w.on("error", (e: any) => reject(e))
    w.write("hello-")
    w.end("streams")
  })
  const text = await collect(fs.createReadStream("/workspace/stream.txt", { highWaterMark: 4 }))
  const chunks: string[] = []
  for await (const chunk of fs.createReadStream("/workspace/stream.txt", { highWaterMark: 4 })) {
    chunks.push(String(chunk))
  }
  return { text, chunks: chunks.length, joined: chunks.join("") }
}`,
	}, false)

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if s, _ := obj["text"].(string); s != "hello-streams" {
		t.Fatalf("createWriteStream/createReadStream 内容不对: %q", s)
	}
	if n := asInt(t, obj["chunks"]); n < 2 {
		t.Fatalf("按 4 字节分块应至少 2 块,实际 %v", n)
	}
	if s, _ := obj["joined"].(string); s != "hello-streams" {
		t.Fatalf("分块拼接结果不对: %q", s)
	}
}

// 7. FileHandle 形态(fs.promises.open 返回句柄),以及只读工作区的 EROFS。
func TestNodeFSPromiseFDAndReadOnly(t *testing.T) {
	rt, _ := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import { open } from "node:fs/promises"

export async function run(): Promise<any> {
  const fh = await open("/workspace/async.txt", "w+")
  await fh.writeFile("async-fd")
  await fh.appendFile("-tail")
  const st = await fh.stat()
  const buf = Buffer.alloc(16)
  const res = await fh.read(buf, 0, 16, 0)
  const all = await fh.readFile("utf8")
  await fh.truncate(4)
  const afterTruncate = await fh.readFile("utf8")
  await fh.close()
  let closed = "no-error"
  try {
    await fh.readFile()
  } catch (e: any) {
    closed = e.code
  }
  return { size: st.size, bytesRead: res.bytesRead, text: buf.toString("utf8", 0, res.bytesRead), all, afterTruncate, closed }
}`,
	}, false)

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("FileHandle 调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if n := asInt(t, obj["size"]); n != 13 {
		t.Fatalf("fstat.size 应为 13,实际 %v", n)
	}
	if s, _ := obj["text"].(string); s != "async-fd-tail" {
		t.Fatalf("FileHandle.read 读回内容不对: %q", s)
	}
	if s, _ := obj["all"].(string); s != "async-fd-tail" {
		t.Fatalf("FileHandle.readFile 内容不对: %q", s)
	}
	if s, _ := obj["afterTruncate"].(string); s != "asyn" {
		t.Fatalf("FileHandle.truncate 后应为 asyn,实际 %q", s)
	}
	if s, _ := obj["closed"].(string); s != "EBADF" {
		t.Fatalf("关闭后的句柄操作应是 EBADF,实际 %q", s)
	}

	rtRO, _ := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import fs from "node:fs"
export function probe(): any {
  const out: any = {}
  try { fs.openSync("/workspace/x.txt", "w") } catch (e: any) { out.write = e.code }
  try { fs.openSync("/workspace/missing.txt", "r") } catch (e: any) { out.read = e.code }
  return out
}`,
	}, true)

	got, err = rtRO.Call(context.Background(), "probe")
	if err != nil {
		t.Fatalf("只读运行时调用失败: %v", err)
	}
	obj, _ = got.(map[string]any)
	if s, _ := obj["write"].(string); s != "EROFS" {
		t.Fatalf("只读工作区 open(w) 应返回 EROFS,实际 %q", s)
	}
	if s, _ := obj["read"].(string); s != "ENOENT" {
		t.Fatalf("只读工作区 open(r) 读不存在的文件应返回 ENOENT,实际 %q", s)
	}
}

// 8. opendir/Dir:同步与异步两种读法、Dirent 判定、以及异步迭代。
func TestNodeFSDir(t *testing.T) {
	rt, dir := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import fs from "node:fs"
import { opendir as opendirAsync } from "node:fs/promises"

export async function run(): Promise<any> {
  const syncDir = fs.opendirSync("/workspace/sub")
  const names: string[] = []
  let entry = syncDir.readSync()
  while (entry !== null) {
    names.push(entry.name + ":" + (entry.isDirectory() ? "dir" : entry.isFile() ? "file" : "other"))
    entry = syncDir.readSync()
  }
  syncDir.closeSync()

  const iterNames: string[] = []
  for await (const dirent of await opendirAsync("/workspace/sub")) {
    iterNames.push(dirent.name)
  }
  return { names: names.sort().join(","), iterNames: iterNames.sort().join(",") }
}`,
	}, false)

	if err := os.MkdirAll(filepath.Join(dir, "sub", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("Dir 调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if s, _ := obj["names"].(string); s != "a.txt:file,inner:dir" {
		t.Fatalf("opendirSync 结果不对: %q", s)
	}
	if s, _ := obj["iterNames"].(string); s != "a.txt,inner" {
		t.Fatalf("opendir 异步迭代结果不对: %q", s)
	}
}

// 10. 句柄级元数据(chmod/chown/utimes)与向量 I/O(readv/writev)。
func TestNodeFSFDMetadataAndVectors(t *testing.T) {
	rt, _ := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import fs from "node:fs"
import { open } from "node:fs/promises"

export async function run(): Promise<any> {
  const fh = await open("/workspace/fd2.txt", "w+")
  await fh.writeFile("hello-vectors")
  await fh.chmod(384)

  let chown = "ok"
  try {
    await fh.chown(1, 1)
  } catch (e: any) {
    chown = e.code
  }

  const when = new Date(1700000000000)
  await fh.utimes(when, when)
  const st = await fh.stat()

  const b1 = Buffer.alloc(4)
  const b2 = Buffer.alloc(4)
  const rv = await fh.readv([b1, b2], 0)
  const wv = await fh.writev([Buffer.from("AB"), Buffer.from("CD")], 0)
  const after = await fh.readFile("utf8")
  await fh.close()
  return {
    chown,
    mtime: st.mtimeMs,
    readv: rv.bytesRead,
    part1: b1.toString("utf8"),
    part2: b2.toString("utf8"),
    writev: wv.bytesWritten,
    after,
  }
}`,
	}, false)

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if n := asInt(t, obj["mtime"]); n != 1700000000000 {
		t.Fatalf("futimes 没有生效: mtimeMs=%v", n)
	}
	if n := asInt(t, obj["readv"]); n != 8 {
		t.Fatalf("readv 应读 8 字节,实际 %v", n)
	}
	if s, _ := obj["part1"].(string); s != "hell" {
		t.Fatalf("readv 第一个缓冲不对: %q", s)
	}
	if s, _ := obj["part2"].(string); s != "o-ve" {
		t.Fatalf("readv 第二个缓冲不对: %q", s)
	}
	if n := asInt(t, obj["writev"]); n != 4 {
		t.Fatalf("writev 应写 4 字节,实际 %v", n)
	}
	if s, _ := obj["after"].(string); s != "ABCDo-vectors" {
		t.Fatalf("writev 后内容不对: %q", s)
	}
	// fchown 在 Windows 上应当是 ENOTSUP,其他平台允许成功或 EPERM(权限受限)。
	chown, _ := obj["chown"].(string)
	if runtime.GOOS == "windows" {
		if chown != "ENOTSUP" {
			t.Fatalf("Windows 上 fchown 应是 ENOTSUP,实际 %q", chown)
		}
	} else if chown != "ok" && chown != "EPERM" && chown != "EACCES" {
		t.Fatalf("fchown 结果异常: %q", chown)
	}
}

// 11. FileHandle.readLines 与 readableWebStream(最小 WHATWG 形状)。
func TestNodeFSReadLinesAndWebStream(t *testing.T) {
	rt, _ := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import { open } from "node:fs/promises"

export async function run(): Promise<any> {
  const fh = await open("/workspace/lines.txt", "w+")
  await fh.writeFile("a\nb\r\nc\n")

  const lines: string[] = []
  for await (const line of fh.readLines()) lines.push(String(line))

  const stream = fh.readableWebStream({ autoAllocateChunkSize: 3 })
  const reader = stream.getReader()
  const parts: string[] = []
  for (;;) {
    const res = await reader.read()
    if (res.done) break
    parts.push(Buffer.from(res.value).toString("utf8"))
  }

  let pipeTo = "no-error"
  try {
    stream.pipeTo({})
  } catch (e: any) {
    pipeTo = e.message.indexOf("未实现") >= 0 ? "unsupported" : "other"
  }

  await fh.close()
  return { lines: lines.join("|"), chunks: parts.join(""), pipeTo }
}`,
	}, false)

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if s, _ := obj["lines"].(string); s != "a|b|c" {
		t.Fatalf("readLines 结果不对: %q", s)
	}
	if s, _ := obj["chunks"].(string); s != "a\nb\r\nc\n" {
		t.Fatalf("readableWebStream 分块拼接不对: %q", s)
	}
	if s, _ := obj["pipeTo"].(string); s != "unsupported" {
		t.Fatalf("pipeTo 应明确报未实现,实际 %q", s)
	}
}

// 12. watch 的拒绝形状:明确 ERR_NOT_SUPPORTED,而不是静默无效的 watcher。
func TestNodeFSWatchRequiresResident(t *testing.T) {
	rt, _ := newFSExtRuntime(t, map[string]string{
		"plugin.ts": `
import fs from "node:fs"

export function probe(): any {
  const out: any = {}
  try { fs.watch("/workspace", () => {}) } catch (e: any) { out.watch = e.code }
  try { fs.watchFile("/workspace", () => {}) } catch (e: any) { out.watchFile = e.code }
  try { fs.unwatchFile("/workspace"); out.unwatchFile = "ok" } catch (e: any) { out.unwatchFile = e.code }
  return out
}`,
	}, false)

	got, err := rt.Call(context.Background(), "probe")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	for _, key := range []string{"watch", "watchFile"} {
		if s, _ := obj[key].(string); s != "ERR_NOT_SUPPORTED" {
			t.Fatalf("%s 应是 ERR_NOT_SUPPORTED,实际 %q", key, s)
		}
	}
	if s, _ := obj["unwatchFile"].(string); s != "ok" {
		t.Fatalf("unwatchFile 应可安全调用，实际 %q", s)
	}
}

// 4. 没有工作区根时 node:fs 仍然不可用(补齐层不能绕开这条边界)。
func TestNodeFSExtNeedsRoot(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `import fs from "node:fs"
export function size(): number { return fs.readdirSync("/workspace").length }`,
	})
	if _, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art}); err == nil {
		t.Fatalf("没有工作区根时 node:fs 应当装配失败")
	}
}
