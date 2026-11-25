package jsrt

// 本文件是 node:zlib 的宿主能力:同步压缩/解压与流式 zlib 句柄。
//
// 放在执行层的理由与 node:crypto 一致:纯计算,没有策略语义(不读文件、
// 不发请求、不改进程状态),因此默认可用;`node:zlib` 模块只负责把它包成
// Node 形状。跨边界仍然只传 base64 字符串,不传二进制对象。
//
// 硬限制:解压的输出有上限(maxZlibOutBytes)。压缩比可以做到上万倍,
// 不设上限就等于把一个 zip bomb 交给插件,而且它会在 JS 的 heap 里展开。

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

const (
	// maxZlibInBytes 是单次输入上限(原始字节)。与 http 的响应体上限同量级。
	maxZlibInBytes = 8 << 20
	// maxZlibOutBytes 是单次输出上限(解压后字节)。
	maxZlibOutBytes = 8 << 20
)

// nativeZlib 实现 `__adaHost.zlib(op, base64)`。
//
// op 与 Node 的同步接口一一对应:gzip / gunzip / deflate / inflate /
// deflateRaw / inflateRaw / unzip。unzip 按 Node 的语义只认 gzip 与 zlib
// 两种容器;HTTP 里那种裸 deflate 流用 inflateRaw。
func (r *Runtime) nativeZlib(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("zlib(op, base64) 需要两个参数")
		}
		op, err := toString(raw, args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("zlib 的操作名必须是字符串: %w", err)
		}
		data, err := toString(raw, args[1])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("zlib 的数据必须是 base64 字符串: %w", err)
		}
		in, derr := base64.StdEncoding.DecodeString(data)
		if derr != nil {
			return engine.Undefined(), fmt.Errorf("zlib 的输入不是合法 base64: %w", derr)
		}
		if len(in) > maxZlibInBytes {
			return engine.Undefined(), fmt.Errorf("zlib 输入 %d 字节,超过上限 %d", len(in), maxZlibInBytes)
		}
		out, cerr := zlibApply(op, in)
		if cerr != nil {
			return engine.Undefined(), cerr
		}
		return raw.FromGo(base64.StdEncoding.EncodeToString(out))
	}
}

// zlibApply 按操作名做一次同步转换,返回原始字节。
func zlibApply(op string, in []byte) ([]byte, error) {
	name := strings.ToLower(strings.TrimSpace(op))
	switch name {
	case "gzip":
		return zlibPack(name, in, func(w io.Writer) (io.WriteCloser, error) {
			return gzip.NewWriter(w), nil
		})
	case "deflate":
		return zlibPack(name, in, func(w io.Writer) (io.WriteCloser, error) {
			return zlib.NewWriter(w), nil
		})
	case "deflateraw":
		return zlibPack(name, in, func(w io.Writer) (io.WriteCloser, error) {
			// 与 Node 的默认档一致:flate.DefaultCompression 即 -1。
			return flate.NewWriter(w, flate.DefaultCompression)
		})
	case "gunzip":
		rd, err := gzip.NewReader(bytes.NewReader(in))
		if err != nil {
			return nil, fmt.Errorf("zlib gunzip 失败: %w", err)
		}
		defer func() { _ = rd.Close() }()
		return zlibUnpack(name, rd)
	case "inflate":
		rd, err := zlib.NewReader(bytes.NewReader(in))
		if err != nil {
			return nil, fmt.Errorf("zlib inflate 失败: %w", err)
		}
		defer func() { _ = rd.Close() }()
		return zlibUnpack(name, rd)
	case "inflateraw":
		return zlibUnpack(name, flate.NewReader(bytes.NewReader(in)))
	case "unzip":
		// Node 的 unzip 只识别 gzip 与 zlib 容器;按魔数判断,再回落到 zlib 解析。
		if len(in) >= 2 && in[0] == 0x1f && in[1] == 0x8b {
			rd, err := gzip.NewReader(bytes.NewReader(in))
			if err != nil {
				return nil, fmt.Errorf("zlib unzip 失败: %w", err)
			}
			defer func() { _ = rd.Close() }()
			return zlibUnpack(name, rd)
		}
		rd, err := zlib.NewReader(bytes.NewReader(in))
		if err != nil {
			return nil, fmt.Errorf("zlib unzip 失败(既不是 gzip 也不是 zlib): %w", err)
		}
		defer func() { _ = rd.Close() }()
		return zlibUnpack(name, rd)
	default:
		return nil, fmt.Errorf("node:zlib 不支持的操作 %q", op)
	}
}

// zlibPack 用给出的 writer 构造器压一次,并把输出上限也应用在压缩侧:
// 压缩后的体积同样会跨 JSON 边界。
func zlibPack(op string, in []byte, ctor func(io.Writer) (io.WriteCloser, error)) ([]byte, error) {
	var buf bytes.Buffer
	w, err := ctor(&buf)
	if err != nil {
		return nil, fmt.Errorf("zlib %s 失败: %w", op, err)
	}
	if _, werr := w.Write(in); werr != nil {
		_ = w.Close()
		return nil, fmt.Errorf("zlib %s 写入失败: %w", op, werr)
	}
	if cerr := w.Close(); cerr != nil {
		return nil, fmt.Errorf("zlib %s 关闭失败: %w", op, cerr)
	}
	if buf.Len() > maxZlibOutBytes {
		return nil, fmt.Errorf("zlib %s 输出 %d 字节,超过上限 %d", op, buf.Len(), maxZlibOutBytes)
	}
	return buf.Bytes(), nil
}

// zlibUnpack 读一次已解压的流,超出上限立即报错而不是读到内存耗尽。
func zlibUnpack(op string, rd io.Reader) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(rd, maxZlibOutBytes+1))
	if err != nil {
		return nil, fmt.Errorf("zlib %s 解压失败: %w", op, err)
	}
	if int64(len(out)) > maxZlibOutBytes {
		return nil, fmt.Errorf("zlib %s 解压结果超过上限 %d 字节", op, maxZlibOutBytes)
	}
	return out, nil
}

// —— 流式 zlib ——
//
// 压缩端用 gzip/zlib/flate Writer + Flush：每次写入后取走已产生的字节。
// 解压端用 io.Pipe + 后台 reader goroutine：写入压缩块，读取当前已解出的字节；
// End 关闭管道并取剩余输出。总输出超过 maxZlibOutBytes 时立即失败。

type zlibStream struct {
	op     string
	writer interface {
		io.WriteCloser
		Flush() error
	}
	buf bytes.Buffer

	pw   *io.PipeWriter
	done chan struct{}

	outMu    sync.Mutex
	out      bytes.Buffer
	err      error
	outTotal int64
	maxOut   int64
	closed   bool
}

func newZlibStream(op string, maxOut int64) (*zlibStream, error) {
	switch op {
	case "gzip", "deflate", "deflateRaw":
		s := &zlibStream{op: op, maxOut: maxOut}
		switch op {
		case "gzip":
			s.writer = gzip.NewWriter(&s.buf)
		case "deflate":
			s.writer = zlib.NewWriter(&s.buf)
		case "deflateRaw":
			w, err := flate.NewWriter(&s.buf, flate.DefaultCompression)
			if err != nil {
				return nil, err
			}
			s.writer = w
		}
		return s, nil
	case "gunzip", "inflate", "inflateRaw", "unzip":
		return newZlibReaderStream(op, maxOut)
	}
	return nil, fmt.Errorf("不支持的 zlib 操作: %s", op)
}

func newZlibReaderStream(op string, maxOut int64) (*zlibStream, error) {
	pr, pw := io.Pipe()
	s := &zlibStream{op: op, pw: pw, done: make(chan struct{}), maxOut: maxOut}
	go func() {
		defer close(s.done)
		var reader io.Reader
		var err error
		switch op {
		case "gunzip":
			reader, err = gzip.NewReader(pr)
		case "inflate":
			reader, err = zlib.NewReader(pr)
		case "inflateRaw":
			reader = flate.NewReader(pr)
		case "unzip":
			br := bufio.NewReader(pr)
			magic, peekErr := br.Peek(2)
			if peekErr != nil && len(magic) < 2 {
				s.setErr(peekErr)
				_ = pw.CloseWithError(peekErr)
				return
			}
			if magic[0] == 0x1f && magic[1] == 0x8b {
				reader, err = gzip.NewReader(br)
			} else {
				reader, err = zlib.NewReader(br)
			}
		}
		if err != nil {
			s.setErr(err)
			_ = pw.CloseWithError(err)
			return
		}
		if closer, ok := reader.(io.Closer); ok {
			defer func() { _ = closer.Close() }()
		}
		buf := make([]byte, 32<<10)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				if appendErr := s.appendOut(buf[:n]); appendErr != nil {
					s.setErr(appendErr)
					_ = pw.CloseWithError(appendErr)
					return
				}
			}
			if readErr == io.EOF {
				return
			}
			if readErr != nil {
				if !errors.Is(readErr, io.ErrUnexpectedEOF) {
					s.setErr(readErr)
				}
				return
			}
		}
	}()
	return s, nil
}

func (s *zlibStream) setErr(err error) {
	s.outMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.outMu.Unlock()
}

func (s *zlibStream) appendOut(data []byte) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.maxOut > 0 && s.outTotal+int64(len(data)) > s.maxOut {
		return fmt.Errorf("zlib 输出超过上限 %d", s.maxOut)
	}
	s.outTotal += int64(len(data))
	_, err := s.out.Write(data)
	return err
}

func (s *zlibStream) takeOut() ([]byte, error) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	out := append([]byte(nil), s.out.Bytes()...)
	s.out.Reset()
	return out, s.err
}

func (s *zlibStream) write(input []byte) ([]byte, error) {
	if s.writer != nil {
		if _, err := s.writer.Write(input); err != nil {
			return nil, err
		}
		if err := s.writer.Flush(); err != nil {
			return nil, err
		}
		out := append([]byte(nil), s.buf.Bytes()...)
		s.buf.Reset()
		if s.maxOut > 0 && s.outTotal+int64(len(out)) > s.maxOut {
			return nil, fmt.Errorf("zlib 输出超过上限 %d", s.maxOut)
		}
		s.outTotal += int64(len(out))
		return out, nil
	}
	if s.pw == nil {
		return nil, fmt.Errorf("zlib 流已关闭")
	}
	if _, err := s.pw.Write(input); err != nil {
		return nil, err
	}
	return s.takeOut()
}

func (s *zlibStream) end() ([]byte, error) {
	if s.writer != nil {
		if err := s.writer.Close(); err != nil {
			return nil, err
		}
		out := append([]byte(nil), s.buf.Bytes()...)
		s.buf.Reset()
		if s.maxOut > 0 && s.outTotal+int64(len(out)) > s.maxOut {
			return nil, fmt.Errorf("zlib 输出超过上限 %d", s.maxOut)
		}
		return out, nil
	}
	if s.pw != nil {
		_ = s.pw.Close()
		<-s.done
	}
	return s.takeOut()
}

func (s *zlibStream) close() {
	s.outMu.Lock()
	if s.closed {
		s.outMu.Unlock()
		return
	}
	s.closed = true
	s.outMu.Unlock()
	if s.writer != nil {
		_ = s.writer.Close()
	}
	if s.pw != nil {
		_ = s.pw.CloseWithError(fmt.Errorf("zlib 流已关闭"))
	}
}

func (r *Runtime) registerZlibStream(s *zlibStream) uint64 {
	r.zlibMu.Lock()
	defer r.zlibMu.Unlock()
	r.zlibSeq++
	id := r.zlibSeq
	r.zlibStreams[id] = s
	return id
}

func (r *Runtime) takeZlibStream(id uint64) (*zlibStream, error) {
	r.zlibMu.Lock()
	defer r.zlibMu.Unlock()
	s, ok := r.zlibStreams[id]
	if !ok {
		return nil, fmt.Errorf("zlib 流句柄无效: %d", id)
	}
	return s, nil
}

func (r *Runtime) removeZlibStream(id uint64) *zlibStream {
	r.zlibMu.Lock()
	defer r.zlibMu.Unlock()
	s := r.zlibStreams[id]
	delete(r.zlibStreams, id)
	return s
}

func (r *Runtime) closeZlibStreams() {
	r.zlibMu.Lock()
	streams := make([]*zlibStream, 0, len(r.zlibStreams))
	for id, s := range r.zlibStreams {
		streams = append(streams, s)
		delete(r.zlibStreams, id)
	}
	r.zlibMu.Unlock()
	for _, s := range streams {
		s.close()
	}
}

func (r *Runtime) nativeZlibStreamNew(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("zlibStreamNew(op) 需要 op")
		}
		op, err := toString(raw, args[0])
		if err != nil {
			return engine.Undefined(), err
		}
		s, err := newZlibStream(op, maxZlibOutBytes)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(float64(r.registerZlibStream(s)))
	}
}

func (r *Runtime) nativeZlibStreamWrite(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("zlibStreamWrite(handle, base64) 需要两个参数")
		}
		id := uint64(numberArg(raw, args, 0, 0))
		s, err := r.takeZlibStream(id)
		if err != nil {
			return engine.Undefined(), err
		}
		text, err := toString(raw, args[1])
		if err != nil {
			return engine.Undefined(), err
		}
		in, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return engine.Undefined(), fmt.Errorf("zlib 输入不是合法 base64: %w", err)
		}
		if len(in) > maxZlibInBytes {
			return engine.Undefined(), fmt.Errorf("zlib 输入超过上限 %d", maxZlibInBytes)
		}
		out, err := s.write(in)
		if err != nil {
			_ = r.removeZlibStream(id)
			s.close()
			return engine.Undefined(), err
		}
		return raw.FromGo(base64.StdEncoding.EncodeToString(out))
	}
}

func (r *Runtime) nativeZlibStreamEnd(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("zlibStreamEnd(handle) 需要 handle")
		}
		id := uint64(numberArg(raw, args, 0, 0))
		s, err := r.takeZlibStream(id)
		if err != nil {
			return engine.Undefined(), err
		}
		_ = r.removeZlibStream(id)
		out, err := s.end()
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(base64.StdEncoding.EncodeToString(out))
	}
}

func (r *Runtime) nativeZlibStreamClose(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("zlibStreamClose(handle) 需要 handle")
		}
		if s := r.removeZlibStream(uint64(numberArg(raw, args, 0, 0))); s != nil {
			s.close()
		}
		return engine.Undefined(), nil
	}
}
