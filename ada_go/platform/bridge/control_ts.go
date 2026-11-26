package bridge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/issueye/ada_jsrt/jsrt"

	"github.com/issueye/ada_go_v2/plugins/seam/control"
)

// tsControl 把 TS 插件提供的控制层对象包装成语言中立的 control.Invoker。
//
// TS 约定只暴露一个 invoke(method, params) 方法;bridge 不改变 kernel 的
// ctx.provide,也不要求 TS 实现 Go 接口。Go 实现则直接实现 Invoker。
type tsControl struct {
	svc *jsrt.Service
}

func wrapControlService(svc *jsrt.Service) (control.Invoker, error) {
	if svc == nil {
		return nil, fmt.Errorf("control 服务不能为 nil")
	}
	found := false
	for _, name := range svc.Methods() {
		if name == "invoke" {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("TS control 服务必须暴露 invoke(method, params) 方法")
	}
	return &tsControl{svc: svc}, nil
}

func (t *tsControl) Invoke(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	out, err := t.svc.Call(ctx, "invoke", method, params)
	if err != nil {
		return nil, control.Unavailable("TS control 调用失败: " + err.Error())
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, control.Invalid("TS control 结果无法序列化: " + err.Error())
	}
	return b, nil
}
