package secretsguard_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	secretsguard "github.com/issueye/ada_go_v2/plugins/policy/secrets-guard"
	capsstatic "github.com/issueye/ada_go_v2/plugins/provider/caps-static"
	secretsenv "github.com/issueye/ada_go_v2/plugins/provider/secrets-env"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
)

type fakeGetter struct{ value string }

func (f *fakeGetter) Get(context.Context, string) (string, error) { return f.value, nil }

func brokerWith(t *testing.T, grants map[string][]string) *caps.Broker {
	t.Helper()
	b := caps.NewBroker(nil)
	for plugin, list := range grants {
		if err := b.Grant(plugin, list...); err != nil {
			t.Fatalf("授予失败: %v", err)
		}
	}
	return b
}

// 1. 授权具体 secret / 通配 secrets.* 放行。
func TestAllowed(t *testing.T) {
	ctx := kernel.WithPrincipal(context.Background(), "p1")
	inner := &fakeGetter{value: "tok"}
	if v, err := secretsguard.New(inner, brokerWith(t, map[string][]string{"p1": {"secrets.GITHUB_TOKEN"}})).
		Get(ctx, "GITHUB_TOKEN"); err != nil || v != "tok" {
		t.Fatalf("授权读取失败: %q %v", v, err)
	}
	if v, err := secretsguard.New(inner, brokerWith(t, map[string][]string{"p1": {"secrets.*"}})).
		Get(ctx, "GITHUB_TOKEN"); err != nil || v != "tok" {
		t.Fatalf("通配授权失败: %q %v", v, err)
	}
}

// 2. 未授权 / 无身份拒绝。
func TestDenied(t *testing.T) {
	g := secretsguard.New(&fakeGetter{value: "tok"}, brokerWith(t, map[string][]string{"p1": {"secrets.AWS_KEY"}}))
	if _, err := g.Get(kernel.WithPrincipal(context.Background(), "p1"), "GITHUB_TOKEN"); err == nil {
		t.Fatalf("未授权 secret 应拒绝")
	}
	if _, err := g.Get(context.Background(), "GITHUB_TOKEN"); err == nil {
		t.Fatalf("无身份应拒绝")
	}
}

// 3. 内核集成:优先使用注入的 caps 服务。
func TestServiceBroker(t *testing.T) {
	t.Setenv(secretsenv.EnvPrefix+"GITHUB_TOKEN", "tok")
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mount := func(name string, p kernel.Plugin, cfg map[string]any) {
		t.Helper()
		if _, err := app.Plugin(p, kernel.NewConfig(cfg)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", name, err)
		}
	}
	mount("seam/secrets", seamsecrets.Plugin(), nil)
	mount("secrets-env", secretsenv.Plugin(), map[string]any{"allow": []string{"GITHUB_TOKEN"}})
	mount("caps-static", capsstatic.Plugin(), map[string]any{
		"grants": map[string][]string{"consumer": {"secrets.GITHUB_TOKEN"}},
	})
	mount("secrets-guard", secretsguard.Plugin(), nil)

	var got string
	consumer := kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamsecrets.Chain](ctx, "secrets")
		if err != nil {
			return nil, err
		}
		g, err := chain.Get()
		if err != nil {
			return nil, err
		}
		got, err = g.Get(ctx.Context(), "GITHUB_TOKEN")
		return nil, err
	}).WithInject("secrets")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("消费者应通过 caps 服务授权: %v", err)
	}
	if got != "tok" {
		t.Fatalf("读取内容不正确: %q", got)
	}
}
