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
func writeFrame(w io.Writer, kind byte, body any, max int) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("rpc: 编码帧失败: %w", err)
	}
	if max <= 0 {
		max = defaultMaxFrame
	}
	if len(b)+1 > max {
		return fmt.Errorf("%w: 帧 %d 超过上限 %d", errFrame, len(b)+1, max)
	}
	var hdr [frameHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(b)+1))
	hdr[4] = kind
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(b) > 0 {
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}
