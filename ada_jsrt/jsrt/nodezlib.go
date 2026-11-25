package jsrt

// 本文件是 node:zlib 的宿主能力:同步压缩与解压。
//
// 放在执行层的理由与 node:crypto 一致:纯计算,没有策略语义(不读文件、
// 不发请求、不改进程状态),因此默认可用;`node:zlib` 模块只负责把它包成
// Node 形状。跨边界仍然只传 base64 字符串,不传二进制对象。
//
// 硬限制:解压的输出有上限(maxZlibOutBytes)。压缩比可以做到上万倍,
// 不设上限就等于把一个 zip bomb 交给插件,而且它会在 JS 的 heap 里展开。

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

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
