package jsrt

// 本文件是 secrets 的宿主能力:按名读取 secret。
//
// 为什么放在执行层而不是桥接层:JS 侧只该拿到「按名取值」这一个动作,
// 真正的来源(环境变量、密钥库)、白名单与授权都在 seam/secrets 的 provider
// 与 policy 装饰器里。这里只做一次字符串转换,并把调用级 ctx 透传下去——
// secrets-guard 需要从 ctx 里取 principal,脱离 ctx 读 secret 会被拒绝。
//
// 返回值永远是字符串,不缓存、不落日志;失败原样返回错误,不吞成空串。

import (
	"fmt"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// nativeSecretsGet 实现 `__adaHost.secretsGet(name)`。
func (r *Runtime) nativeSecretsGet(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("secretsGet(name) 需要一个参数")
		}
		name, err := toString(raw, args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("secretsGet 的名字必须是字符串: %w", err)
		}
		if r.opts.Host.Secrets == nil {
			return engine.Undefined(), fmt.Errorf("secretsGet: 宿主没有安装 secrets 能力")
		}
		value, serr := r.opts.Host.Secrets(r.currentContext(), name)
		if serr != nil {
			return engine.Undefined(), serr
		}
		return raw.FromGo(value)
	}
}
