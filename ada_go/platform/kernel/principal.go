package kernel

import "context"

type principalKey struct{}

// WithPrincipal 返回携带调用者插件身份的 context;空 id 表示「无身份」。
func WithPrincipal(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, principalKey{}, id)
}

// Principal 从 context 读取调用者插件 id。
func Principal(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(principalKey{}).(string)
	return id, ok && id != ""
}
