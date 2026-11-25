// Package seamerr 定义所有 seam 共用的错误码与错误类型(seam-api §1.2)。
//
// 为什么单独成包:错误码是**跨 seam 的公共词汇**;如果每个 seam 各写一套,
// 消费者就得按来源分支处理同一个语义(例如「未配置后端」)。
package seamerr

import (
	"errors"
	"fmt"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// Code 是跨 seam 的统一错误码。
type Code string

const (
	ErrUnimplemented Code = "EUNIMPLEMENTED" // seam 在位、无 provider
	ErrInvalid       Code = "EINVALID"       // 请求非法
	ErrUnavailable   Code = "EUNAVAILABLE"   // provider 不可达
	ErrAuth          Code = "EAUTH"          // 凭据无效
	ErrRateLimit     Code = "ERATELIMIT"     // 限流
	ErrContext       Code = "ECONTEXT"       // 上下文超长
	ErrCanceled      Code = "ECANCELED"      // 调用方取消
	ErrTimeout       Code = "ETIMEOUT"       // 超时
	ErrDenied        Code = "EDENIED"        // 审批/guard 拒绝
	ErrSchema        Code = "ESCHEMA"        // 输出不符合 schema
	ErrInternal      Code = "EINTERNAL"      // 实现内部错误(细节不进边界)
	ErrLimit         Code = "ELIMIT"         // 超出配额/队列/帧上限
)

// Error 是 seam 层的错误。它保留 kernel 的错误码形制,同时带 retryable 与来源。
type Error struct {
	Code      Code
	Message   string
	Retryable bool
	Provider  string
	Err       error
}

func (e *Error) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("%s: %s: %s", e.Code, e.Provider, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// New 构造一个 seam 错误。
func New(code Code, provider, message string, retryable bool) *Error {
	return &Error{Code: code, Provider: provider, Message: message, Retryable: retryable}
}

// Wrap 在保留错误码的前提下附上下层错误。
func Wrap(code Code, provider, message string, err error) *Error {
	return &Error{Code: code, Provider: provider, Message: message, Err: err}
}

// Unimplemented 表示「seam 在位,但没有可用的 provider」。
// 四个 seam 都必须在无 provider 时空转返回它(seam-api §0)。
func Unimplemented(provider, message string) error {
	return New(ErrUnimplemented, provider, message, false)
}

// Is 判断错误链上的 seam 错误码。
func Is(err error, code Code) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	// 兼容 kernel 层错误码(kernel 用 kernel.Code 字符串枚举)。
	var ke *kernel.Error
	if errors.As(err, &ke) {
		return string(ke.Code) == string(code)
	}
	return false
}
