package flow_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/issueye/ada_go_v2/platform/flow"
)

// 1. Save/Load/Delete 往返,目录自动创建。
func TestFileStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoints")
	store, err := flow.NewFileStore(dir)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, "run-1", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	data, err := store.Load(ctx, "run-1")
	if err != nil || string(data) != `{"a":1}` {
		t.Fatalf("读取失败: %q %v", data, err)
	}
	if err := store.Delete(ctx, "run-1"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := store.Load(ctx, "run-1"); err == nil {
		t.Fatalf("删除后应读取失败")
	}
	// 幂等删除。
	if err := store.Delete(ctx, "run-1"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
}

// 2. 非法 runID 拒绝。
func TestFileStoreRunID(t *testing.T) {
	store, err := flow.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	for _, bad := range []string{"", "../x", "a/b", "a\\b", ".."} {
		if err := store.Save(context.Background(), bad, []byte("x")); err == nil {
			t.Fatalf("runID %q 应拒绝", bad)
		}
	}
}

// 3. 原子写不留下临时文件。
func TestFileStoreNoTemp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cps")
	store, err := flow.NewFileStore(dir)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if err := store.Save(context.Background(), "run-1", []byte("x")); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "run-1.json" {
		t.Fatalf("目录里只应有 run-1.json: %v", entries)
	}
}
