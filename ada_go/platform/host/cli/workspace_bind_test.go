package cli

// 本文件是包内测试(不是 cli_test 外部包):验证 ACP 的工作区绑定这类内部语义。

import (
	"path/filepath"
	"testing"

	"github.com/issueye/ada_go_v2/platform/plugin"
)

// TestBindWorkspaceToCwd 覆盖 ACP 的工作区语义:fs-local 一定绑到 cwd;
// shell-local 只在没有 workspace 服务时绑到 cwd(有服务时根由链解析)。
func TestBindWorkspaceToCwd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "项目")
	manifests := []*plugin.Manifest{
		{ID: "fs-local", Config: map[string]any{"workspace": "workspace", "tmp": "workspace/.ada-tmp"}},
		{ID: "shell-local", Config: map[string]any{"workspace": "."}},
		{ID: "system-prompt", Config: map[string]any{"identity": "你是测试机器人"}},
		{ID: "llm-replay", Config: map[string]any{"path": "data/agent-replay.jsonl"}},
	}
	bindWorkspaceToCwd(manifests, dir)

	if got := manifests[0].Config["workspace"]; got != dir {
		t.Fatalf("fs-local workspace=%v, want %q", got, dir)
	}
	if got := manifests[0].Config["tmp"]; got != filepath.Join(dir, ".ada-tmp") {
		t.Fatalf("fs-local tmp=%v, want %q", got, filepath.Join(dir, ".ada-tmp"))
	}
	if got := manifests[1].Config["workspace"]; got != dir {
		t.Fatalf("shell-local workspace=%v, want %q(独立模式)", got, dir)
	}
	// 不属于工作区的插件配置不能被顺手改掉。
	if manifests[2].Config["identity"] != "你是测试机器人" {
		t.Fatalf("system-prompt 配置被改动: %v", manifests[2].Config)
	}
	if manifests[3].Config["path"] != "data/agent-replay.jsonl" {
		t.Fatalf("llm-replay 配置被改动: %v", manifests[3].Config)
	}
}

// TestBindWorkspaceKeepsManagedShellRoot 有 workspace 服务时,shell 的根由
// workspace 链解析;把 preset 里的根覆盖成 cwd 会让会话切换工作区失效。
func TestBindWorkspaceKeepsManagedShellRoot(t *testing.T) {
	dir := t.TempDir()
	manifests := []*plugin.Manifest{
		{ID: "workspace-local", Config: map[string]any{}},
		{ID: "shell-local", Config: map[string]any{"workspace": "workspace"}},
		{ID: "fs-local", Config: nil},
	}
	bindWorkspaceToCwd(manifests, dir)
	if manifests[1].Config["workspace"] != "workspace" {
		t.Fatalf("托管模式下不应覆盖 shell-local 的根: %v", manifests[1].Config)
	}
	if manifests[2].Config["workspace"] != dir {
		t.Fatalf("fs-local 仍应绑到 cwd: %v", manifests[2].Config)
	}
	// nil Config 也要能写入。
	if manifests[2].Config["tmp"] != filepath.Join(dir, ".ada-tmp") {
		t.Fatalf("nil Config 未初始化: %v", manifests[2].Config)
	}
}
