package auditlog_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	auditlog "github.com/issueye/ada_go_v2/plugins/policy/audit-log"
	capsstatic "github.com/issueye/ada_go_v2/plugins/provider/caps-static"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

// 1. 事件被写成一行 JSON。
func TestWrites(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl"
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(auditlog.Plugin(), kernel.NewConfig(map[string]any{"path": path})); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	app.Root().Emit(seamfs.EventObserve, map[string]any{"op": "read", "path": "/workspace/a"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计文件失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("应有 1 行,实际 %d", len(lines))
	}
	var rec struct {
		Event   string         `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("行不是合法 JSON: %v", err)
	}
	if rec.Event != seamfs.EventObserve || rec.Payload["op"] != "read" {
		t.Fatalf("记录不正确: %+v", rec)
	}
}

// 2. 多类事件按顺序追加。
func TestAppendsMultiple(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl"
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(auditlog.Plugin(), kernel.NewConfig(map[string]any{"path": path})); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	app.Root().Emit(seamfs.EventObserve, map[string]any{"op": "read"})
	app.Root().Emit(seamshell.EventObserve, map[string]any{"op": "run"})
	app.Root().Emit(caps.EventDeny, map[string]any{"op": "deny"})
	data, _ := os.ReadFile(path)
	if got := strings.Count(string(data), "\n"); got != 3 {
		t.Fatalf("应有 3 行,实际 %d", got)
	}
}

// 3. 缺少 path → 插件失败。
func TestMissingPath(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	fiber, err := app.Plugin(auditlog.Plugin(), kernel.NewConfig(map[string]any{}))
	if err != nil {
		t.Fatalf("挂载调用失败: %v", err)
	}
	rctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := fiber.Ready(rctx); err == nil {
		t.Fatalf("缺 path 应失败")
	}
}

// 4. 按大小轮转,保留 keep 个历史文件。
func TestRotation(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl"
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(auditlog.Plugin(), kernel.NewConfig(map[string]any{
		"path": path, "maxBytes": int64(1), "keep": 2,
	})); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	app.Root().Emit(seamfs.EventObserve, map[string]any{"n": 1})
	app.Root().Emit(seamfs.EventObserve, map[string]any{"n": 2})
	app.Root().Emit(seamfs.EventObserve, map[string]any{"n": 3})

	read := func(p string) string {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", p, err)
		}
		return string(data)
	}
	if !strings.Contains(read(path), `"n":3`) {
		t.Fatalf("当前文件应含第 3 条")
	}
	if !strings.Contains(read(path+".1"), `"n":2`) {
		t.Fatalf(".1 应含第 2 条")
	}
	if !strings.Contains(read(path+".2"), `"n":1`) {
		t.Fatalf(".2 应含第 1 条")
	}
}

// 5. 非法轮转配置 → 插件失败。
func TestRotationInvalid(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	fiber, err := app.Plugin(auditlog.Plugin(), kernel.NewConfig(map[string]any{
		"path": t.TempDir() + "/a.jsonl", "maxBytes": int64(-1),
	}))
	if err != nil {
		t.Fatalf("挂载调用失败: %v", err)
	}
	rctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := fiber.Ready(rctx); err == nil {
		t.Fatalf("负 maxBytes 应失败")
	}
}

// 6. approval/decided 也落盘。
func TestApprovalDecided(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl"
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(auditlog.Plugin(), kernel.NewConfig(map[string]any{"path": path})); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	app.Root().Emit(auditlog.EventApprovalDecided, map[string]any{"allow": true, "target": "echo"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计文件失败: %v", err)
	}
	if !strings.Contains(string(data), auditlog.EventApprovalDecided) {
		t.Fatalf("缺少 approval/decided 记录: %s", data)
	}
}

// 8. 观察/审批 payload 的敏感 key 与 URL query 统一脱敏。
func TestRedactsPayload(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl"
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(auditlog.Plugin(), kernel.NewConfig(map[string]any{"path": path})); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	app.Root().Emit(auditlog.EventApprovalDecided, map[string]any{
		"target":  "https://user:pass@example.com/x?token=SECRET",
		"headers": map[string]any{"Authorization": "Bearer SECRET"},
		"nested":  map[string]any{"apiKey": "SECRET", "ok": "keep"},
		"reason":  "fetch https://example.com/y?token=SECRET failed",
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计文件失败: %v", err)
	}
	if strings.Contains(string(data), "SECRET") || strings.Contains(string(data), "pass") {
		t.Fatalf("审计 payload 未脱敏: %s", data)
	}
	if !strings.Contains(string(data), "REDACTED") || !strings.Contains(string(data), `"ok":"keep"`) {
		t.Fatalf("脱敏结果不正确: %s", data)
	}
}

func TestCapsDeny(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl"
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(auditlog.Plugin(), kernel.NewConfig(map[string]any{"path": path})); err != nil {
		t.Fatalf("挂载 audit-log 失败: %v", err)
	}
	if _, err := app.Plugin(capsstatic.Plugin(), kernel.NewConfig(map[string]any{})); err != nil {
		t.Fatalf("挂载 caps-static 失败: %v", err)
	}
	raw, err := app.Root().Service("caps")
	if err != nil {
		t.Fatal(err)
	}
	broker, ok := raw.(*caps.Broker)
	if !ok {
		t.Fatalf("caps 服务类型不对: %T", raw)
	}
	if err := broker.Grant("p", "fs.read"); err != nil {
		t.Fatal(err)
	}
	if err := broker.Authorize(context.Background(), "p", "fs.write", "https://example.com/x?token=SECRET"); err == nil {
		t.Fatalf("未授权应拒绝")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计文件失败: %v", err)
	}
	if !strings.Contains(string(data), caps.EventDeny) || strings.Contains(string(data), "SECRET") {
		t.Fatalf("caps 拒绝审计不正确: %s", data)
	}
}
