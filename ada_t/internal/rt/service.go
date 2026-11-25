// 服务桥的 Go 侧:把任意 Go 函数包装成「JSON 进、JSON 出」的可反射调用器。
//
// 契约(rt.Expose / App.Expose):
//   - 参数按函数签名逐个从 JSON 反序列化;返回值序列化回 JS
//   - 签名支持 func() T、func(...) (T, error)、func(...) error、func()
//   - 首参若是 context.Context,自动注入(无调用级取消:长任务请改用 Notify 回推)
//   - 不支持变参
//
// 线程安全:调用发生在 loop goroutine 上(原生函数回调)——服务实现方
// 不必加锁,但慢调用会阻塞渲染,长任务应立即返回并用 rt.Notify 回推结果。
package rt

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
)

// serviceInvoker 是一个已校验的服务方法。
type serviceInvoker struct {
	service    string
	method     string
	fn         reflect.Value
	paramTypes []reflect.Type
	hasCtx     bool
}

var errType = reflect.TypeOf((*error)(nil)).Elem()
var ctxType = reflect.TypeOf((*context.Context)(nil)).Elem()

func newInvoker(service, method string, fn any) (*serviceInvoker, error) {
	if service == "" || method == "" {
		return nil, fmt.Errorf("rt: 服务名与方法名不能为空")
	}
	rv := reflect.ValueOf(fn)
	if rv.Kind() != reflect.Func || rv.IsNil() {
		return nil, fmt.Errorf("rt: 服务 %s.%s 需要 func,得到 %T", service, method, fn)
	}
	t := rv.Type()
	if t.IsVariadic() {
		return nil, fmt.Errorf("rt: 服务 %s.%s 不支持变参函数", service, method)
	}
	inv := &serviceInvoker{service: service, method: method, fn: rv}
	for i := 0; i < t.NumIn(); i++ {
		pt := t.In(i)
		if i == 0 && pt == ctxType {
			inv.hasCtx = true
			continue
		}
		inv.paramTypes = append(inv.paramTypes, pt)
	}
	// 返回值形态校验:每个返回值要么是 error,要么是(唯一的)结果
	results := 0
	for i := 0; i < t.NumOut(); i++ {
		if t.Out(i) == errType {
			if i != t.NumOut()-1 {
				return nil, fmt.Errorf("rt: 服务 %s.%s 的 error 返回值必须在最后", service, method)
			}
			continue
		}
		results++
		if results > 1 {
			return nil, fmt.Errorf("rt: 服务 %s.%s 最多一个非 error 返回值", service, method)
		}
	}
	return inv, nil
}

// call 执行一次调用:args 已是 JSON 反序列化后的 []any。
// panic 被回收为 error:服务实现崩溃不能带走 eventloop/进程。
func (inv *serviceInvoker) call(args []any) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			result = nil
			err = fmt.Errorf("rt: 服务 %s.%s panic: %v", inv.service, inv.method, p)
		}
	}()
	if len(args) != len(inv.paramTypes) {
		return nil, fmt.Errorf("rt: 服务 %s.%s 参数个数不符:需要 %d,得到 %d",
			inv.service, inv.method, len(inv.paramTypes), len(args))
	}
	in := make([]reflect.Value, 0, len(args)+1)
	if inv.hasCtx {
		in = append(in, reflect.ValueOf(context.Background()))
	}
	for i, pt := range inv.paramTypes {
		v, err := convertArg(args[i], pt)
		if err != nil {
			return nil, fmt.Errorf("rt: 服务 %s.%s 参数 %d: %w", inv.service, inv.method, i, err)
		}
		in = append(in, v)
	}
	outs := inv.fn.Call(in)
	for _, o := range outs {
		if o.Type() == errType {
			if !o.IsNil() {
				return nil, o.Interface().(error)
			}
			continue
		}
		result = o.Interface()
	}
	return result, nil
}

// convertArg 把 JSON 反序列化后的 any 转成参数类型。
func convertArg(arg any, t reflect.Type) (reflect.Value, error) {
	if arg == nil {
		return reflect.Zero(t), nil
	}
	b, err := json.Marshal(arg)
	if err != nil {
		return reflect.Value{}, err
	}
	p := reflect.New(t)
	if err := json.Unmarshal(b, p.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return p.Elem(), nil
}
