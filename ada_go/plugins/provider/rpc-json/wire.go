// Package rpcjson 是 `provider/rpc-json`:RPC 协议的 JSON 帧实现。
//
// 它只做编解码、握手、请求分发、事件与取消;传输由 rpc 服务注入,
// 地址允许表、审批、审计属于 policy 装饰器。
package rpcjson

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ProviderName 是插件 id。
const ProviderName = "rpc-json"

// protocolVersion 是 body 里的协议版本。
const protocolVersion = 1

// 帧 kind;新增 kind 属于破坏性扩展,必须升 protocolVersion。
const (
	kindChallenge byte = 0x01
	kindAuth      byte = 0x02
	kindReady     byte = 0x03
	kindRequest   byte = 0x04
	kindResponse  byte = 0x05
	kindEvent     byte = 0x06
	kindCancel    byte = 0x07
	kindSubscribe byte = 0x08
	kindPing      byte = 0x09
	kindPong      byte = 0x0a
	kindBye       byte = 0x0b
)

type challenge struct {
	V     int      `json:"v"`
	Nonce string   `json:"nonce"`
	Modes []string `json:"modes"`
}

type authMsg struct {
	V      int    `json:"v"`
	Client string `json:"client"`
	Proof  string `json:"proof,omitempty"`
}

type ready struct {
	V        int      `json:"v"`
	Peer     string   `json:"peer"`
	Features []string `json:"features"`
}

type request struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	NoReply bool            `json:"noReply,omitempty"`
}

type response struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type event struct {
	V     int             `json:"v"`
	Topic string          `json:"topic"`
	Seq   uint64          `json:"seq"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type cancelMsg struct {
	V      int    `json:"v"`
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

type subscribeMsg struct {
	V      int      `json:"v"`
	Topics []string `json:"topics"`
}

type pingMsg struct {
	V  int   `json:"v"`
	Ts int64 `json:"ts"`
}

type byeMsg struct {
	V       int    `json:"v"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type wireError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// toWire 把 seam 错误映射成线上错误;非 seam 错误一律 EINTERNAL,细节不进边界。
func toWire(err error) *wireError {
	if err == nil {
		return nil
	}
	var se *seamerr.Error
	if errors.As(err, &se) {
		return &wireError{Code: string(se.Code), Message: se.Message, Retryable: se.Retryable}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &wireError{Code: string(seamerr.ErrCanceled), Message: "调用已取消"}
	}
	return &wireError{Code: string(seamerr.ErrInternal), Message: "rpc: 内部错误"}
}

// fromWire 把线上错误还原成带码的 seam 错误。
func fromWire(w *wireError) error {
	if w == nil {
		return nil
	}
	code := seamerr.Code(w.Code)
	switch code {
	case seamerr.ErrAuth, seamerr.ErrDenied, seamerr.ErrInvalid, seamerr.ErrUnavailable,
		seamerr.ErrRateLimit, seamerr.ErrContext, seamerr.ErrCanceled, seamerr.ErrTimeout,
		seamerr.ErrSchema, seamerr.ErrUnimplemented, seamerr.ErrInternal, seamerr.ErrLimit:
	default:
		code = seamerr.ErrInternal
	}
	return &seamerr.Error{Code: code, Provider: "rpc-json", Message: w.Message, Retryable: w.Retryable}
}

// proof 计算挑战应答:HMAC-SHA256(token, "rpc1:"+nonce+":"+client)。
func proof(token []byte, nonce, client string) string {
	m := hmac.New(sha256.New, token)
	_, _ = m.Write([]byte("rpc1:" + nonce + ":" + client))
	return hex.EncodeToString(m.Sum(nil))
}

// validClient 校验客户端名:≤64、只允许 [a-z0-9._-]。
func validClient(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// validID 校验请求 id:非空且 ≤128 字节。
func validID(id string) bool {
	return id != "" && len(id) <= 128
}

func validMethod(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// topicMatch 支持 "*" 与末段 "/*" 两种通配,不做正则。
func topicMatch(pattern, topic string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		return strings.HasPrefix(topic, strings.TrimSuffix(pattern, "/*")+"/")
	}
	return pattern == topic
}

// errFrame 是帧级错误;读循环据此关闭连接。
var errFrame = errors.New("rpc: 帧非法")
