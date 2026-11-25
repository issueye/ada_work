package kernel

import (
	"context"
	"errors"
	"fmt"
)

// Code 是内核层的错误码。跨边界传递的错误必须带码，
// 否则调用方无法区分「依赖缺失」「越权」「插件故障」这些完全不同的处置方式。
type Code string

const (
	CodeInvalid           Code = "EINVALID"        // 参数/状态非法
	CodeNotInjected       Code = "ENOTINJECTED"    // 尝试使用未声明的服务
	CodeServiceMissing    Code = "ESERVICE"        // 服务尚未提供
	CodeServiceRegistered Code = "ESERVICEINUSE"   // 服务槽已被占用
	CodeNotProvider       Code = "ENOTPROVIDER"    // 非提供者尝试写服务值
	CodeEffectDisposed    Code = "EEFFECTDISPOSED" // 在已卸载的作用域上注册副作用
	CodeFiberDisposed     Code = "EFIBERDISPOSED"  // fiber 已终结
	CodeFiberFailed       Code = "EFIBERFAILED"    // fiber 装配失败
	CodePending           Code = "EPENDING"        // 依赖未满足，仍在等待
	CodeConfig            Code = "ECONFIG"         // 配置校验失败
	CodeTimeout           Code = "ETIMEOUT"        // 监听器/副作用超时
	CodePanic             Code = "EPANIC"          // 插件回调 panic
	CodeCycle             Code = "ECYCLE"          // 依赖成环
	CodeUnimplemented     Code = "EUNIMPLEMENTED"  // 能力未实现（M1 预留给 seam）
)

// Error 是内核错误。它同时携带错误码与原始错误，便于 errors.Is/As 与日志归因。
type Error struct {
	Code Code
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil && e.Msg != "" {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Fail 构造一个带错误码的错误。所有内核错误都经它产生。
func Fail(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Wrap 在保留错误码的前提下附加上下文。
func Wrap(code Code, err error, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...), Err: err}
}

// IsCode 判断错误链上任一环的错误码。
func IsCode(err error, code Code) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	return false
}

// Dispose 是所有注册 API 的返回值：调用即撤销，重复调用无副作用。
type Dispose func(context.Context) error

// Nop 是不做任何事的 Dispose，用于「注册成功但无需清理」的情况。
func Nop(context.Context) error { return nil }

// All 把多个 Dispose 合成一个；按传入顺序执行，返回聚合错误。
func All(ds ...Dispose) Dispose {
	return func(ctx context.Context) error {
		var errs []error
		for _, d := range ds {
			if d == nil {
				continue
			}
			if err := d(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}
