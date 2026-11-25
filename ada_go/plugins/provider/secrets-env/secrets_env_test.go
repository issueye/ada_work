package secretsenv_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	secretsenv "github.com/issueye/ada_go_v2/plugins/provider/secrets-env"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
)

// 1. 白名单内的 secret 从环境读取。
func TestGet(t *testing.T) {
	t.Setenv(secretsenv.EnvPrefix+"GITHUB_TOKEN", "tok-123")
	g := secretsenv.New([]string{"GITHUB_TOKEN"})
	v, err := g.Get(context.Background(), "GITHUB_TOKEN")
	if err != nil || v != "tok-123" {
		t.Fatalf("读取失败: %q %v", v, err)
	}
}

// 2. 不在白名单 → 拒绝。
func TestNotAllowed(t *testing.T) {
	g := secretsenv.New([]string{"GITHUB_TOKEN"})
	if _, err := g.Get(context.Background(), "AWS_KEY"); err == nil {
		t.Fatalf("未允许的 secret 应拒绝")
	}
}

// 3. 白名单内但环境缺失 → 拒绝。
func TestMissingEnv(t *testing.T) {
	g := secretsenv.New([]string{"GITHUB_TOKEN"})
	if _, err := g.Get(context.Background(), "GITHUB_TOKEN"); err == nil {
		t.Fatalf("缺少环境变量应拒绝")
	}
}

// 4. 非法名 → 参数错误。
func TestInvalidName(t *testing.T) {
	g := secretsenv.New([]string{"GITHUB_TOKEN"})
	if _, err := g.Get(context.Background(), "github_token"); err == nil {
		t.Fatalf("小写名应拒绝")
	}
	if _, err := g.Get(context.Background(), ""); err == nil {
		t.Fatalf("空名应拒绝")
	}
}

// 5. 内核集成:seam/secrets + secrets-env + consumer。
func TestKernelIntegration(t *testing.T) {
	t.Setenv(secretsenv.EnvPrefix+"GITHUB_TOKEN", "tok-123")
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(seamsecrets.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam/secrets 失败: %v", err)
	}
	if _, err := app.Plugin(secretsenv.Plugin(), kernel.NewConfig(map[string]any{
		"allow": []string{"GITHUB_TOKEN"},
	})); err != nil {
		t.Fatalf("挂载 secrets-env 失败: %v", err)
	}
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
		got, err = g.Get(context.Background(), "GITHUB_TOKEN")
		return nil, err
	}).WithInject("secrets")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("消费者失败: %v", err)
	}
	if got != "tok-123" {
		t.Fatalf("读取内容不正确: %q", got)
	}
}
