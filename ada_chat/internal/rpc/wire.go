package rpc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const protocolVersion = 1

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
	Code       Code   `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	ProviderID string `json:"provider,omitempty"`
}

// toWire maps errors to the wire shape; non-seam errors become EINTERNAL.
func toWire(err error) *wireError {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		return &wireError{Code: se.Code, Message: se.Message, Retryable: se.Retryable}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &wireError{Code: ErrCanceled, Message: "调用已取消"}
	}
	return &wireError{Code: ErrInternal, Message: "rpc: 内部错误"}
}

// fromWire restores a coded error from the wire.
func fromWire(w *wireError) error {
	if w == nil {
		return nil
	}
	code := w.Code
	switch code {
	case ErrAuth, ErrDenied, ErrInvalid, ErrUnavailable, ErrRateLimit, ErrContext,
		ErrCanceled, ErrTimeout, ErrSchema, ErrUnimplemented, ErrInternal, ErrLimit:
	default:
		code = ErrInternal
	}
	return &Error{Code: code, Provider: "rpc-json", Message: w.Message, Retryable: w.Retryable}
}

// proof computes HMAC-SHA256(token, "rpc1:"+nonce+":"+client) as hex.
func proof(token []byte, nonce, client string) string {
	m := hmac.New(sha256.New, token)
	_, _ = m.Write([]byte("rpc1:" + nonce + ":" + client))
	return hex.EncodeToString(m.Sum(nil))
}

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

func validMethod(name string) bool {
	if name == "" || len(name) > 128 {
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

func topicMatch(pattern, topic string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		return strings.HasPrefix(topic, strings.TrimSuffix(pattern, "/*")+"/")
	}
	return pattern == topic
}
