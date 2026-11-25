package jsrt

// 本文件处理 JS 与 Go waterfall 之间的类型落差。
//
// JS 只能看到 JSON,而 Go 侧的消费者按具体类型断言输入(例如 *llm.Request、
// *prompts.Assembly、*tools.CallInfo)。如果不把 JS 返回的 JSON 还原成输入的具体类型,
// JS 中间件的改写会被下游静默忽略——这会把「注册了却不起作用」伪装成成功。
//
// 只做一件事:输入是「指向结构体的指针」时,把 JS 值按 JSON 解码回同一类型;
// 其它形态(标量、切片、map、非结构体指针)原样返回,避免强行转换无关对象。

import (
	"encoding/json"
	"reflect"
)

// coerceWaterfall 把 JS 跨边界传来/返回的值还原成 input 的具体类型。
//
// next(v) 的参数和中间件的返回值都走这里:前者保证下游拿到类型化值,
// 后者保证宿主拿到同一类型(否则 `v.(*T)` 会失败,改写被丢弃)。
func coerceWaterfall(input, value any) any {
	if input == nil || value == nil {
		return value
	}
	targetType := reflect.TypeOf(input)
	if targetType.Kind() != reflect.Pointer || targetType.Elem().Kind() != reflect.Struct {
		return value
	}
	if reflect.TypeOf(value) == targetType {
		return value
	}
	// nil 结构体指针无法从 JSON 里区分「空对象」还是「零值」,按原值返回更安全。
	if reflect.ValueOf(input).IsNil() {
		return value
	}
	body, err := json.Marshal(value)
	if err != nil {
		return value
	}
	out := reflect.New(targetType.Elem())
	if err := json.Unmarshal(body, out.Interface()); err != nil {
		return value
	}
	return out.Interface()
}
