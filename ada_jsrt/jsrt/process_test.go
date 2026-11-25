package jsrt_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestProcessEnvPermission(t *testing.T) {
	t.Setenv("ADA_TOKEN", "secret")
	t.Setenv("PATH", "/usr/bin")
	art, err := compile.Build("env", "plugin.ts", map[string]string{
		"plugin.ts": `
import process from "node:process";
export function read(k: string): string { return process.env[k] || "" }
export function write(k: string, v: string): string {
  try { (process.env as any)[k] = v; return "ok" } catch (e: any) { return String((e && e.code) || e) }
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact:    art,
		Permissions: jsrt.Permissions{Env: jsrt.EnvPermission{Allow: []string{"ADA_*"}, Set: true}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	ctx := context.Background()

	if got, err := rt.Call(ctx, "read", "ADA_TOKEN"); err != nil || got != "secret" {
		t.Fatalf("允许的 env 读取不符: got=%v err=%v", got, err)
	}
	if got, err := rt.Call(ctx, "read", "PATH"); err != nil || got != "" {
		t.Fatalf("未授权 env 应不可见: got=%v err=%v", got, err)
	}
	if got, err := rt.Call(ctx, "write", "ADA_NEW", "x"); err != nil || got != "ok" {
		t.Fatalf("允许的 env 写入失败: got=%v err=%v", got, err)
	}
	if got, err := rt.Call(ctx, "read", "ADA_NEW"); err != nil || got != "x" {
		t.Fatalf("写入后读取不符: got=%v err=%v", got, err)
	}

	art2, _ := compile.Build("env2", "plugin.ts", map[string]string{
		"plugin.ts": `import process from "node:process";
export function write(): string { try { (process.env as any).ADA_X = "1"; return "ok" } catch (e: any) { return String((e && e.code) || e) } }`,
	}, compile.Options{})
	rt2, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact:    art2,
		Permissions: jsrt.Permissions{Env: jsrt.EnvPermission{Allow: []string{"ADA_*"}, Set: false}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt2.Close(context.Background()) })
	if got, err := rt2.Call(ctx, "write"); err != nil || got != "ERR_ACCESS_DENIED" {
		t.Fatalf("禁止写入应返回 ERR_ACCESS_DENIED: got=%v err=%v", got, err)
	}
}

func TestProcessExitRequest(t *testing.T) {
	rt := newResident(t, `
import process from "node:process";
export function quit(): void { process.exit(7) }
export function ping(): string { return "pong" }
`, jsrt.Options{ExitPolicy: jsrt.ExitRequest})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = rt.Call(ctx, "quit")
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	err := rt.Wait(waitCtx)
	var exitErr *jsrt.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("Wait 应返回 ExitError(7)，实际 %v", err)
	}
}

func TestProcessExitDeniedByDefault(t *testing.T) {
	rt := newResident(t, `
import process from "node:process";
export function quit(): void { process.exit(1) }
export function ping(): string { return "pong" }
`, jsrt.Options{})
	ctx := context.Background()
	if _, err := rt.Call(ctx, "quit"); err == nil {
		t.Fatal("默认 ExitDeny 下 process.exit 应抛错")
	} else if !strings.Contains(err.Error(), "禁用") {
		t.Fatalf("错误信息不符: %v", err)
	}
	if got, err := rt.Call(ctx, "ping"); err != nil || got != "pong" {
		t.Fatalf("ExitDeny 后运行时仍应可用: got=%v err=%v", got, err)
	}
}

func TestProcessTiming(t *testing.T) {
	rt := newResident(t, `
import process from "node:process";
export async function run(): Promise<string> {
  const start = process.hrtime();
  await new Promise((resolve) => setTimeout(resolve, 20));
  const delta = process.hrtime(start);
  const uptimeOk = process.uptime() > 0;
  const bigintOk = typeof process.hrtime.bigint() === "bigint";
  const deltaOk = delta[0] >= 0 && delta[1] >= 0 && (delta[0] > 0 || delta[1] > 0);
  return uptimeOk && bigintOk && deltaOk ? "ok" : "bad";
}
`, jsrt.Options{})
	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("timing 调用失败: %v", err)
	}
	if got != "ok" {
		t.Fatalf("process timing 不符: %v", got)
	}
}
