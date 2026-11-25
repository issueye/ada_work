package rpcjson

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const frameHeaderSize = 5

// defaultMaxFrame 是默认帧上限(16 MiB);配置范围之外的值在 plugin 里修正。
const defaultMaxFrame = 16 << 20

// readFrame 读一个长度前缀帧:4 字节大端长度(含 kind)+ 1 字节 kind + body。
func readFrame(r io.Reader, max int) (byte, []byte, error) {
	var hdr [frameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n < 1 {
		return 0, nil, fmt.Errorf("%w: 长度为 0", errFrame)
	}
	if max <= 0 {
		max = defaultMaxFrame
	}
	if int64(n) > int64(max) {
		return 0, nil, fmt.Errorf("%w: 帧 %d 超过上限 %d", errFrame, n, max)
	}
	body := make([]byte, int(n)-1)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return hdr[4], body, nil
}

// writeFrame 编一个帧。body 为 nil 时仍写 kind。
func encodeFrame(kind byte, body any, max int) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("rpc: 编码帧失败: %w", err)
	}
	if max <= 0 {
		max = defaultMaxFrame
	}
	if len(b)+1 > max {
		return nil, fmt.Errorf("%w: 帧 %d 超过上限 %d", errFrame, len(b)+1, max)
	}
	out := make([]byte, frameHeaderSize+len(b))
	binary.BigEndian.PutUint32(out[:4], uint32(len(b)+1))
	out[4] = kind
	copy(out[frameHeaderSize:], b)
	return out, nil
}

func writeFrame(w io.Writer, kind byte, body any, max int) error {
	data, err := encodeFrame(kind, body, max)
	if err != nil {
		return err
	}
	return writeFull(w, data)
}

// writeFull 处理 net.Conn / 管道上的短写；Write 返回 n<len 时必须继续。
func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
