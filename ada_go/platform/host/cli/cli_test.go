package cli_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/host/cli"
	"github.com/issueye/ada_go_v2/platform/install"
)

func run(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := cli.Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// 1. plan 文本输出与退出码。
func TestPlanText(t *testing.T) {
	code, out, errOut := run("plan", "../../../examples/plugins")
	if code != 0 {
		t.Fatalf("plan 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "顺序: greeter → upper → presenter") {
		t.Fatalf("plan 输出不正确:\n%s", out)
	}
	if errOut != "" {
		t.Fatalf("成功时不应写 stderr: %s", errOut)
	}
}

// 2. plan -json:输出是合法 JSON 且 ok=true。
func TestPlanJSON(t *testing.T) {
	code, out, _ := run("plan", "../../../examples/plugins", "-json")
	if code != 0 {
		t.Fatalf("plan -json 应成功,code=%d", code)
	}
	var payload struct {
		OK    bool     `json:"ok"`
		Order []string `json:"order"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, out)
	}
	if !payload.OK || len(payload.Order) != 3 {
		t.Fatalf("JSON 内容不正确: %+v", payload)
	}
}

// 3. 加载失败 → 退出码 1 且写 stderr。
func TestPlanBadDir(t *testing.T) {
	code, _, errOut := run("plan", "../../../examples/nope")
	if code != 1 || !strings.Contains(errOut, "加载失败") {
		t.Fatalf("应返回 1 并报错: code=%d err=%s", code, errOut)
	}
}

// 4. 无参数 → 用法错误(2)。
func TestUsage(t *testing.T) {
	code, _, errOut := run()
	if code != 2 || !strings.Contains(errOut, "用法") {
		t.Fatalf("应返回 2 并打印用法: code=%d err=%s", code, errOut)
	}
}

// 5. verify:未签名 ts 插件是 third-party。
func TestVerify(t *testing.T) {
	code, out, errOut := run("verify", "../../../examples/git-ts/plugins/git-helper")
	if code != 0 {
		t.Fatalf("verify 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "信任: third-party") {
		t.Fatalf("verify 输出不正确:\n%s", out)
	}
}

// 6. inspect:挂载并输出快照。
func TestInspect(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("inspect", "--preset", "examples/git-ts/git.preset.json")
	if code != 0 {
		t.Fatalf("inspect 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "fibers:") || !strings.Contains(out, "services:") {
		t.Fatalf("inspect 输出不正确:\n%s", out)
	}
}

// 6.1 inspect --chain:输出 fs/shell/net 的装饰器优先级。
func TestInspectChain(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	code, out, errOut := run("inspect", "--chain", "--preset", "examples/agent/agent.preset.json")
	if code != 0 {
		t.Fatalf("inspect --chain 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "chains:") || !strings.Contains(out, "fs") {
		t.Fatalf("inspect --chain 输出不正确:\n%s", out)
	}
}

// 6.2 agent:回放 preset 跑一轮并输出最终文本。
func TestAgentReplay(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	code, out, errOut := run("agent", "--preset", "examples/agent/agent.preset.json", "-message", "读取 a.txt 并总结")
	if code != 0 {
		t.Fatalf("agent 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "文件内容是 hello") {
		t.Fatalf("agent 输出不正确:\n%s", out)
	}
}

// 6.2.1 agent + workspace:会话绑定只读工作区,工具查询与 fs 解析都落到该根。
func TestAgentWorkspace(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	code, out, errOut := run("agent", "--preset", "examples/workspace/workspace.preset.json",
		"--workspace", "docs", "-message", "查询工作区并读取 a.txt")
	if code != 0 {
		t.Fatalf("agent workspace 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "工作区查询完成,文件内容是 hello") {
		t.Fatalf("workspace agent 输出不正确:\n%s", out)
	}

	code, _, errOut = run("agent", "--preset", "examples/workspace/workspace.preset.json",
		"--workspace", "missing", "-message", "x")
	if code != 1 || !strings.Contains(errOut, "工作区不存在") {
		t.Fatalf("未知工作区应失败,code=%d err=%s", code, errOut)
	}

	code, out, errOut = run("inspect", "--chain", "--preset", "examples/workspace/workspace.preset.json")
	if code != 0 || !strings.Contains(out, "workspace priorities=[100]") {
		t.Fatalf("inspect --chain 应显示 workspace 链,code=%d err=%s\n%s", code, errOut, out)
	}
}

// 6.3 plan:jsonl session preset 必须可装配,且选中 session-jsonl。
func TestPlanJSONLPreset(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	code, out, errOut := run("plan", "--preset", "examples/agent/agent-jsonl.preset.json")
	if code != 0 {
		t.Fatalf("jsonl preset 应可装配,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "session-jsonl") || !strings.Contains(out, "可装配") {
		t.Fatalf("plan 输出不正确:\n%s", out)
	}
}

// 7. up:挂载后停止。
func TestUp(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("up", "--preset", "examples/git-ts/git.preset.json")
	if code != 0 {
		t.Fatalf("up 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "已停止。") {
		t.Fatalf("up 输出不正确:\n%s", out)
	}
}

// 8. install:安装到临时目录。
func TestInstall(t *testing.T) {
	dest := t.TempDir()
	code, out, errOut := run("install", "../../../examples/git-ts/plugins/git-helper", "-dest", dest)
	if code != 0 {
		t.Fatalf("install 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "已安装") {
		t.Fatalf("install 输出不正确:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dest, "git-helper@0.1.0", "plugin.ts")); err != nil {
		t.Fatalf("安装目录缺少插件文件: %v", err)
	}
}

// 9. 未知命令 → 用法错误。
func TestUnknownCommand(t *testing.T) {
	code, _, errOut := run("frobnicate")
	if code != 2 || !strings.Contains(errOut, "用法") {
		t.Fatalf("未知命令应返回 2: code=%d err=%s", code, errOut)
	}
}

// 10. verify 输出能力清单。
func TestVerifyCaps(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"id":"caps-demo","version":"0.1.0","kind":"go","caps":["fs.read","exec"]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	code, out, errOut := run("verify", dir)
	if code != 0 {
		t.Fatalf("verify 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "能力: fs.read, exec") {
		t.Fatalf("verify 应输出能力清单:\n%s", out)
	}
}

// 11. install 同意:声明能力的插件没有 -yes 时拒绝;-yes 后安装。
func TestInstallConsent(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"id":"caps-demo","version":"0.1.0","kind":"go","caps":["fs.read"]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	dest := t.TempDir()

	code, out, errOut := run("install", dir, "-dest", dest)
	if code != 2 || !strings.Contains(errOut, "需要 -yes") {
		t.Fatalf("未确认应拒绝安装: code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "能力: fs.read") {
		t.Fatalf("拒绝前应展示能力:\n%s", out)
	}

	code, out, errOut = run("install", dir, "-dest", dest, "-yes")
	if code != 0 || !strings.Contains(out, "已安装") {
		t.Fatalf("确认后应安装成功: code=%d err=%s out=%s", code, errOut, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "caps-demo@0.1.0", "plugin.json")); err != nil {
		t.Fatalf("安装产物缺失: %v", err)
	}
}

// 12. list:安装后能看到索引条目。
func TestList(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"list-demo","version":"0.1.0","kind":"go","caps":["fs.read"]}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	dest := t.TempDir()
	if code, _, errOut := run("install", src, "-dest", dest, "-yes"); code != 0 {
		t.Fatalf("安装失败: code=%d err=%s", code, errOut)
	}
	code, out, errOut := run("list", "-dest", dest)
	if code != 0 {
		t.Fatalf("list 应成功,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "list-demo@0.1.0") || !strings.Contains(out, "trust=builtin") || !strings.Contains(out, "caps=fs.read") {
		t.Fatalf("list 输出不正确:\n%s", out)
	}
}

// 13. list:空安装根目录。
func TestListEmpty(t *testing.T) {
	code, out, errOut := run("list", "-dest", t.TempDir())
	if code != 0 || !strings.Contains(out, "(空)") {
		t.Fatalf("空列表输出不正确: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 14. uninstall:卸载后 list 为空。
func TestUninstall(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"un-demo","version":"0.1.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	dest := t.TempDir()
	if code, _, errOut := run("install", src, "-dest", dest); code != 0 {
		t.Fatalf("安装失败: code=%d err=%s", code, errOut)
	}
	code, out, errOut := run("uninstall", "un-demo", "-dest", dest)
	if code != 0 || !strings.Contains(out, "已卸载: un-demo") {
		t.Fatalf("卸载输出不正确: code=%d err=%s out=%s", code, errOut, out)
	}
	code, out, _ = run("list", "-dest", dest)
	if code != 0 || !strings.Contains(out, "(空)") {
		t.Fatalf("卸载后 list 应为空: code=%d out=%s", code, out)
	}
}

// 15. uninstall:未知 id → 退出码 1。
func TestUninstallUnknown(t *testing.T) {
	code, _, errOut := run("uninstall", "nope", "-dest", t.TempDir())
	if code != 1 || !strings.Contains(errOut, "卸载失败") {
		t.Fatalf("未知 id 应返回 1: code=%d err=%s", code, errOut)
	}
}

// 16. upgrade:安装 0.1 后升级 0.2,旧目录被清理。
func TestUpgrade(t *testing.T) {
	dest := t.TempDir()
	for _, ver := range []string{"0.1.0", "0.2.0"} {
		src := t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "plugin.json"),
			[]byte(`{"id":"up-demo","version":"`+ver+`","kind":"go"}`), 0o644); err != nil {
			t.Fatalf("写清单失败: %v", err)
		}
		args := []string{"install", src, "-dest", dest}
		if ver == "0.2.0" {
			args = []string{"upgrade", src, "-dest", dest}
		}
		code, out, errOut := run(args...)
		if code != 0 {
			t.Fatalf("%s 失败: code=%d err=%s", ver, code, errOut)
		}
		if ver == "0.2.0" && !strings.Contains(out, "已升级") {
			t.Fatalf("upgrade 输出不正确: %s", out)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "up-demo@0.1.0")); err == nil {
		t.Fatalf("旧版本目录应被清理")
	}
	code, out, _ := run("list", "-dest", dest)
	if code != 0 || !strings.Contains(out, "up-demo@0.2.0") || strings.Contains(out, "0.1.0") {
		t.Fatalf("list 应只显示最新版本:\n%s", out)
	}
}

// 17. upgrade -keep 后 rollback:回到旧版本并清理新版本。
func TestRollback(t *testing.T) {
	dest := t.TempDir()
	for _, ver := range []string{"0.1.0", "0.2.0"} {
		src := t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "plugin.json"),
			[]byte(`{"id":"rb-demo","version":"`+ver+`","kind":"go"}`), 0o644); err != nil {
			t.Fatalf("写清单失败: %v", err)
		}
		args := []string{"install", src, "-dest", dest}
		if ver == "0.2.0" {
			args = []string{"upgrade", src, "-dest", dest, "-keep"}
		}
		if code, _, errOut := run(args...); code != 0 {
			t.Fatalf("%s 失败: code=%d err=%s", ver, code, errOut)
		}
	}
	code, out, errOut := run("rollback", "rb-demo", "-dest", dest)
	if code != 0 || !strings.Contains(out, "已回滚: rb-demo → 0.1.0") {
		t.Fatalf("回滚输出不正确: code=%d err=%s out=%s", code, errOut, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "rb-demo@0.2.0")); err == nil {
		t.Fatalf("新版本目录应被删除")
	}
	code, out, _ = run("list", "-dest", dest)
	if code != 0 || !strings.Contains(out, "rb-demo@0.1.0") || strings.Contains(out, "0.2.0") {
		t.Fatalf("回滚后 list 应只显示 0.1.0:\n%s", out)
	}
}

// 18. rollback:没有旧版本 → 退出码 1。
func TestRollbackNoPrevious(t *testing.T) {
	dest := t.TempDir()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"rb-demo","version":"0.1.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	if code, _, errOut := run("install", src, "-dest", dest); code != 0 {
		t.Fatalf("安装失败: code=%d err=%s", code, errOut)
	}
	code, _, errOut := run("rollback", "rb-demo", "-dest", dest)
	if code != 1 || !strings.Contains(errOut, "回滚失败") {
		t.Fatalf("单版本回滚应返回 1: code=%d err=%s", code, errOut)
	}
}

// 19. caps:无能力清单可授权。
func TestCapsNone(t *testing.T) {
	code, out, errOut := run("caps", "../../../examples/plugins")
	if code != 0 {
		t.Fatalf("caps 应成功: code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "(无)") || !strings.Contains(out, "结果: 可授权") {
		t.Fatalf("caps 输出不正确:\n%s", out)
	}
}

// 20. caps:能力与 trust 合法。
func TestCapsValid(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"id":"cap-demo","version":"0.1.0","kind":"go","trust":"third-party","caps":["fs.read"]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	code, out, errOut := run("caps", dir)
	if code != 0 {
		t.Fatalf("caps 应成功: code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "caps=fs.read") || !strings.Contains(out, "trust=third-party") {
		t.Fatalf("caps 输出不正确:\n%s", out)
	}
}

// 21. caps:trust 不足 → 退出码 1。
func TestCapsTrustDenied(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"id":"cap-demo","version":"0.1.0","kind":"go","trust":"third-party","caps":["fs.write"]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	code, out, errOut := run("caps", dir)
	if code != 1 || !strings.Contains(errOut, "授权失败") {
		t.Fatalf("trust 不足应返回 1: code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "结果: 不可授权") {
		t.Fatalf("应提示不可授权:\n%s", out)
	}
}

// 22. caps -json:结构化输出。
func TestCapsJSON(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"id":"cap-demo","version":"0.1.0","kind":"go","trust":"third-party","caps":["fs.read"]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	code, out, _ := run("caps", dir, "-json")
	if code != 0 {
		t.Fatalf("caps -json 应成功: code=%d", code)
	}
	var payload struct {
		OK      bool `json:"ok"`
		Plugins []struct {
			ID   string   `json:"id"`
			Caps []string `json:"caps"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, out)
	}
	if !payload.OK || len(payload.Plugins) != 1 || payload.Plugins[0].ID != "cap-demo" {
		t.Fatalf("JSON 内容不正确: %+v", payload)
	}
}

// 23. up:内置 Go 安全栈(seam/fs + fs-local + caps-static + caps-gate)能挂载。
func TestUpBuiltinSecurityStack(t *testing.T) {
	dir := t.TempDir()
	workspace := t.TempDir()
	writeManifest := func(id, body string) {
		t.Helper()
		sub := filepath.Join(dir, id)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sub, "plugin.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("写清单失败: %v", err)
		}
	}
	writeManifest("seam-fs", `{"id":"seam-fs","version":"0.1.0","kind":"go","provide":["fs"]}`)
	writeManifest("fs-local", `{"id":"fs-local","version":"0.1.0","kind":"go","inject":{"required":["fs"]},"config":{"workspace":"`+strings.ReplaceAll(workspace, `\`, `\\`)+`"}}`)
	writeManifest("caps-static", `{"id":"caps-static","version":"0.1.0","kind":"go","provide":["caps"]}`)
	writeManifest("caps-gate", `{"id":"caps-gate","version":"0.1.0","kind":"go","inject":{"required":["fs"],"optional":["caps"]}}`)

	code, out, errOut := run("up", dir)
	if code != 0 {
		t.Fatalf("内置安全栈应装配成功: code=%d err=%s out=%s", code, errOut, out)
	}
	if !strings.Contains(out, "已停止。") {
		t.Fatalf("up 输出不正确:\n%s", out)
	}
}

// 24. update:从离线仓库更新到更高版本;再次更新提示已是最新。
func TestUpdateFromRepo(t *testing.T) {
	dest := t.TempDir()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"up-demo","version":"0.1.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	if code, _, errOut := run("install", src, "-dest", dest); code != 0 {
		t.Fatalf("安装失败: %s", errOut)
	}

	repo := t.TempDir()
	rel := filepath.Join("plugins", "up-demo@0.2.0")
	abs := filepath.Join(repo, rel)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(abs, "plugin.json"),
		[]byte(`{"id":"up-demo","version":"0.2.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写仓库清单失败: %v", err)
	}
	info, err := install.VerifyDir(abs, install.Options{})
	if err != nil {
		t.Fatalf("校验仓库插件失败: %v", err)
	}
	index := map[string]any{"entries": []install.RepoEntry{
		{ID: "up-demo", Version: "0.2.0", Dir: rel, Digest: info.Digest},
	}}
	data, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(repo, install.RepoFile), data, 0o644); err != nil {
		t.Fatalf("写仓库索引失败: %v", err)
	}

	code, out, errOut := run("update", "up-demo", "-repo", repo, "-dest", dest)
	if code != 0 || !strings.Contains(out, "已更新: up-demo@0.2.0") {
		t.Fatalf("更新失败: code=%d err=%s out=%s", code, errOut, out)
	}
	code, out, _ = run("list", "-dest", dest)
	if code != 0 || !strings.Contains(out, "up-demo@0.2.0") || strings.Contains(out, "0.1.0") {
		t.Fatalf("更新后 list 不正确:\n%s", out)
	}

	code, _, errOut = run("update", "up-demo", "-repo", repo, "-dest", dest)
	if code != 1 || !strings.Contains(errOut, "更新失败") {
		t.Fatalf("已是最新应失败: code=%d err=%s", code, errOut)
	}
}

// 25. update -require-repo-signature:缺签名 → 退出码 1。
func TestUpdateRequiresRepoSignature(t *testing.T) {
	dest := t.TempDir()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"sig-demo","version":"0.1.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	if code, _, errOut := run("install", src, "-dest", dest); code != 0 {
		t.Fatalf("安装失败: %s", errOut)
	}
	repo := t.TempDir()
	rel := filepath.Join("plugins", "sig-demo@0.2.0")
	abs := filepath.Join(repo, rel)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(abs, "plugin.json"),
		[]byte(`{"id":"sig-demo","version":"0.2.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写仓库清单失败: %v", err)
	}
	info, err := install.VerifyDir(abs, install.Options{})
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	data, _ := json.Marshal(map[string]any{"entries": []install.RepoEntry{
		{ID: "sig-demo", Version: "0.2.0", Dir: rel, Digest: info.Digest},
	}})
	if err := os.WriteFile(filepath.Join(repo, install.RepoFile), data, 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	code, _, errOut := run("update", "sig-demo", "-repo", repo, "-dest", dest, "-require-repo-signature")
	if code != 1 || !strings.Contains(errOut, "缺少签名") {
		t.Fatalf("缺签名应拒绝: code=%d err=%s", code, errOut)
	}
}

// 26. repo:在线读取未签名索引;require 时缺签名失败。
func TestRepo(t *testing.T) {
	data := []byte(`{"entries":[{"id":"repo-demo","version":"0.1.0","dir":"plugins/repo-demo@0.1.0"}]}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	code, out, errOut := run("repo", srv.URL+"/repo.json")
	if code != 0 || !strings.Contains(out, "repo-demo@0.1.0") {
		t.Fatalf("repo 输出不正确: code=%d err=%s out=%s", code, errOut, out)
	}
	code, _, errOut = run("repo", srv.URL+"/repo.json", "-require-repo-signature")
	if code != 1 || !strings.Contains(errOut, "缺少签名") {
		t.Fatalf("缺签名应失败: code=%d err=%s", code, errOut)
	}
}

// 27. update -repo-url:在线索引 + artifact 更新。
func TestUpdateRemote(t *testing.T) {
	dest := t.TempDir()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"remote-demo","version":"0.1.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	if code, _, errOut := run("install", src, "-dest", dest); code != 0 {
		t.Fatalf("安装失败: %s", errOut)
	}

	pluginSrc := t.TempDir()
	manifest := `{"id":"remote-demo","version":"0.2.0","kind":"go"}`
	if err := os.WriteFile(filepath.Join(pluginSrc, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	info, err := install.VerifyDir(pluginSrc, install.Options{})
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("pkg/plugin.json")
	_, _ = f.Write([]byte(manifest))
	_ = zw.Close()
	zipData := buf.Bytes()

	var serverURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/p.zip", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(zipData) })
	mux.HandleFunc("/repo.json", func(w http.ResponseWriter, _ *http.Request) {
		index := map[string]any{"entries": []install.RepoEntry{
			{ID: "remote-demo", Version: "0.2.0", Source: serverURL + "/p.zip", Digest: info.Digest},
		}}
		b, _ := json.Marshal(index)
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	serverURL = srv.URL

	code, out, errOut := run("update", "remote-demo", "-repo-url", srv.URL+"/repo.json", "-dest", dest)
	if code != 0 || !strings.Contains(out, "已更新: remote-demo@0.2.0") {
		t.Fatalf("在线更新失败: code=%d err=%s out=%s", code, errOut, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "remote-demo@0.1.0")); err == nil {
		t.Fatalf("旧版本应被清理")
	}
}

// 28. check:合法目录通过;TS 目录编译通过;非法 caps 失败。
func TestCheck(t *testing.T) {
	code, out, errOut := run("check", "../../../examples/plugins")
	if code != 0 || !strings.Contains(out, "结果: 通过") {
		t.Fatalf("check 失败: code=%d err=%s out=%s", code, errOut, out)
	}
	tsDir := t.TempDir()
	tsPlugin := filepath.Join(tsDir, "ts-demo")
	if err := os.MkdirAll(tsPlugin, 0o755); err != nil {
		t.Fatalf("建 TS 目录失败: %v", err)
	}
	tsManifest := `{"id":"ts-demo","version":"0.1.0","kind":"ts","entry":"plugin.ts"}`
	if err := os.WriteFile(filepath.Join(tsPlugin, "plugin.json"), []byte(tsManifest), 0o644); err != nil {
		t.Fatalf("写 TS 清单失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tsPlugin, "plugin.ts"), []byte("export function apply(): void {}\n"), 0o644); err != nil {
		t.Fatalf("写 TS 源码失败: %v", err)
	}
	code, out, errOut = run("check", tsDir)
	if code != 0 || !strings.Contains(out, "编译: OK") {
		t.Fatalf("TS check 失败: code=%d err=%s out=%s", code, errOut, out)
	}

	bad := t.TempDir()
	sub := filepath.Join(bad, "bad")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	manifest := `{"id":"bad","version":"0.1.0","kind":"go","trust":"third-party","caps":["fs.write"]}`
	if err := os.WriteFile(filepath.Join(sub, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	code, out, errOut = run("check", bad)
	if code != 1 || !strings.Contains(out, "能力: 失败") {
		t.Fatalf("非法 caps 应失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 29. check -json:结构化输出。
func TestCheckJSON(t *testing.T) {
	code, out, _ := run("check", "../../../examples/plugins", "-json")
	if code != 0 {
		t.Fatalf("check -json 应通过: code=%d", code)
	}
	var payload struct {
		OK     bool `json:"ok"`
		PlanOK bool `json:"planOk"`
		CapsOK bool `json:"capsOk"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, out)
	}
	if !payload.OK || !payload.PlanOK || !payload.CapsOK {
		t.Fatalf("JSON 内容不正确: %+v", payload)
	}
}

func TestUpSecretsStack(t *testing.T) {
	code, out, errOut := run("up", "--preset", "../../../examples/secrets/secrets.preset.json")
	if code != 0 || !strings.Contains(out, "service secrets") {
		t.Fatalf("secrets 栈挂载失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 32. 脚本插件(git-helper TS)+ 注入模块 ada:shell 端到端。
func TestAgentGitScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("agent", "--preset", "examples/git-ts/git.preset.json", "-message", "看看 git 状态")
	if code != 0 || !strings.Contains(out, "已读取 git 状态(脚本插件)") {
		t.Fatalf("脚本插件运行失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 33. 脚本插件 task-helper + 注入 ada:fs 装配。
func TestUpTaskScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("up", "--preset", "examples/task-ts/task.preset.json")
	if code != 0 || !strings.Contains(out, "task-helper") {
		t.Fatalf("task-helper 装配失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 34. 脚本插件 task-helper:真实工具调用(add → list,经 ada:fs 持久化)。
func TestAgentTaskScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	workspace := t.TempDir()
	f, err := os.CreateTemp("examples/task-ts", "test-*.preset.json")
	if err != nil {
		t.Fatalf("创建临时 preset 失败: %v", err)
	}
	preset := f.Name()
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(preset) })
	body := `{"name":"task-test","plugins":["../agent/plugins","plugins"],"disable":["llm-openai","session-jsonl"],"patch":{"agent-loop":{"model":"replay"},"llm-replay":{"path":"examples/task-ts/replay.jsonl"},"fs-local":{"workspace":"` + strings.ReplaceAll(workspace, `\`, `\\`) + `"}}}`
	if err := os.WriteFile(preset, []byte(body), 0o644); err != nil {
		t.Fatalf("写临时 preset 失败: %v", err)
	}
	code, out, errOut := run("agent", "--preset", preset, "-message", "添加任务并列出")
	if code != 0 || !strings.Contains(out, "任务已添加并列出") {
		t.Fatalf("task-helper 运行失败: code=%d err=%s out=%s", code, errOut, out)
	}
	data, err := os.ReadFile(workspace + "/.ada/tasks.json")
	if err != nil || !strings.Contains(string(data), "验证脚本插件") {
		t.Fatalf("任务文件不正确: %v %s", err, data)
	}
}

// 35. 脚本插件 command-helper + ada:shell 装配与命令调用。
func TestUpCommandScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("up", "--preset", "examples/command-ts/command.preset.json")
	if code != 0 || !strings.Contains(out, "command-helper") {
		t.Fatalf("command-helper 装配失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

func TestAgentCommandScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("agent", "--preset", "examples/command-ts/command.preset.json", "-message", "执行 git --version")
	if code != 0 || !strings.Contains(out, "命令执行完成") {
		t.Fatalf("command-helper 运行失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 35.1 shell-helper(TS 实现)装配:检查 TS 工具注册到 tools 服务。
func TestUpToolShellScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("up", "--preset", "examples/shell-helper-ts/shell.preset.json")
	if code != 0 || !strings.Contains(out, "shell-helper") {
		t.Fatalf("shell-helper 装配失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 35.2 shell-helper(TS 实现)端到端:run_shell 经 ada:shell.exec 执行真实命令。
func TestAgentToolShellScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("agent", "--preset", "examples/shell-helper-ts/shell.preset.json", "-message", "执行命令")
	if code != 0 || !strings.Contains(out, "命令执行完成: shell-helper-ts") {
		t.Fatalf("shell-helper 运行失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 36. 脚本插件 text-helper:装配 + 读写/编辑/搜索(临时 workspace)。
func TestUpTextScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("up", "--preset", "examples/text-ts/text.preset.json")
	if code != 0 || !strings.Contains(out, "text-helper") {
		t.Fatalf("text-helper 装配失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

func TestAgentTextScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	workspace := t.TempDir()
	f, err := os.CreateTemp("examples/text-ts", "test-*.preset.json")
	if err != nil {
		t.Fatalf("创建临时 preset 失败: %v", err)
	}
	preset := f.Name()
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(preset) })
	body := `{"name":"text-test","plugins":["../agent/plugins","../../plugins/tool/text-helper"],"disable":["llm-openai","session-jsonl"],"patch":{"agent-loop":{"model":"replay"},"llm-replay":{"path":"examples/text-ts/replay.jsonl"},"fs-local":{"workspace":"` + strings.ReplaceAll(workspace, `\`, `\\`) + `"}}}`
	if err := os.WriteFile(preset, []byte(body), 0o644); err != nil {
		t.Fatalf("写临时 preset 失败: %v", err)
	}
	code, out, errOut := run("agent", "--preset", preset, "-message", "写文件、替换并搜索")
	if code != 0 || !strings.Contains(out, "文本操作完成") {
		t.Fatalf("text-helper 运行失败: code=%d err=%s out=%s", code, errOut, out)
	}
	data, err := os.ReadFile(workspace + "/a.txt")
	if err != nil || string(data) != "hello ada" {
		t.Fatalf("文本结果不正确: %v %q", err, data)
	}
}

// 37. 脚本插件 memory-helper:装配(清单 + TS 编译 + 工具注册)。
func TestUpMemoryScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("up", "--preset", "examples/memory-ts/memory.preset.json")
	if code != 0 || !strings.Contains(out, "memory-helper") {
		t.Fatalf("memory-helper 装配失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 38. 脚本插件 memory-helper:真实工具调用(write → search,经 ada:fs 持久化)。
func TestAgentMemoryScriptPlugin(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	workspace := t.TempDir()
	f, err := os.CreateTemp("examples/memory-ts", "test-*.preset.json")
	if err != nil {
		t.Fatalf("创建临时 preset 失败: %v", err)
	}
	preset := f.Name()
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(preset) })
	body := `{"name":"memory-test","plugins":["../agent/plugins","../../plugins/tool/memory-helper"],"disable":["llm-openai","session-jsonl"],"patch":{"agent-loop":{"model":"replay"},"llm-replay":{"path":"examples/memory-ts/replay.jsonl"},"memory-helper":{"autoRecall":true},"fs-local":{"workspace":"` + strings.ReplaceAll(workspace, `\`, `\\`) + `"}}}`
	if err := os.WriteFile(preset, []byte(body), 0o644); err != nil {
		t.Fatalf("写临时 preset 失败: %v", err)
	}
	code, out, errOut := run("agent", "--preset", preset, "-message", "记住我的偏好并回忆")
	if code != 0 || !strings.Contains(out, "已记住并检索到你的语言偏好") {
		t.Fatalf("memory-helper 运行失败: code=%d err=%s out=%s", code, errOut, out)
	}
	data, err := os.ReadFile(workspace + "/.ada/memory.json")
	if err != nil || !strings.Contains(string(data), "用户偏好中文回答") {
		t.Fatalf("记忆文件不正确: %v %s", err, data)
	}
}

// 39. tool-helper:Go 插件装配 + tool_find/tool_exec 端到端。
func TestUpToolHelper(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	code, out, errOut := run("up", "--preset", "examples/tool-helper/toolhelper.preset.json")
	if code != 0 || !strings.Contains(out, "tool-helper") {
		t.Fatalf("tool-helper 装配失败: code=%d err=%s out=%s", code, errOut, out)
	}
}

// 40. tool-helper:tool_exec 通过 tools 注册表调用另一个插件(TS)的工具。
func TestAgentToolHelper(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	workspace := t.TempDir()
	f, err := os.CreateTemp("examples/tool-helper", "test-*.preset.json")
	if err != nil {
		t.Fatalf("创建临时 preset 失败: %v", err)
	}
	preset := f.Name()
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(preset) })
	body := `{"name":"tool-helper-test","plugins":["../agent/plugins","../../plugins/tool/text-helper"],"disable":["llm-openai","session-jsonl"],"patch":{"agent-loop":{"model":"replay"},"llm-replay":{"path":"examples/tool-helper/replay.jsonl"},"fs-local":{"workspace":"` + strings.ReplaceAll(workspace, `\`, `\\`) + `"}}}`
	if err := os.WriteFile(preset, []byte(body), 0o644); err != nil {
		t.Fatalf("写临时 preset 失败: %v", err)
	}
	code, out, errOut := run("agent", "--preset", preset, "-message", "找到写文件的工具并调用")
	if code != 0 || !strings.Contains(out, "已找到工具并通过 tool_exec 写入 out.txt") {
		t.Fatalf("tool-helper 运行失败: code=%d err=%s out=%s", code, errOut, out)
	}
	data, err := os.ReadFile(workspace + "/out.txt")
	if err != nil || string(data) != "via tool_exec" {
		t.Fatalf("tool_exec 未写入目标文件: %v %q", err, data)
	}
}

// http-helper 示例:复用 agent 示例的 Go 清单 + TS 插件,必须可装配。
// 这里只验证装配,不发真实请求(测试不允许依赖外部网络)。
func TestPlanHTTPHelperExample(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	code, out, errOut := run("plan", "--preset", "examples/http/http.preset.json")
	if code != 0 {
		t.Fatalf("http preset 应可装配,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "http-helper") || !strings.Contains(out, "结果: 可装配") {
		t.Fatalf("http preset 输出不正确:\n%s", out)
	}
}

// plugin-helper 示例:插件开发工具(生成/体检/改 preset),同样只验证装配。
func TestPlanPluginHelperExample(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	code, out, errOut := run("plan", "--preset", "examples/plugin-helper/plugin.preset.json")
	if code != 0 {
		t.Fatalf("plugin-helper preset 应可装配,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "plugin-helper") || !strings.Contains(out, "结果: 可装配") {
		t.Fatalf("plugin-helper preset 输出不正确:\n%s", out)
	}
}

func TestPlanSkillHelperExample(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	code, out, errOut := run("plan", "--preset", "examples/skill/skill.preset.json")
	if code != 0 {
		t.Fatalf("skill preset 应可装配,code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "skill-helper") || !strings.Contains(out, "结果: 可装配") {
		t.Fatalf("skill preset 输出不正确:\n%s", out)
	}
}

// lockedBuffer 让测试可以在命令仍在写 stderr 时安全读取。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitACP(t *testing.T, lines <-chan map[string]json.RawMessage, pred func(map[string]json.RawMessage) bool) map[string]json.RawMessage {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case m, ok := <-lines:
			if !ok {
				t.Fatal("ACP 输出提前结束")
			}
			if pred(m) {
				return m
			}
		case <-deadline:
			t.Fatal("等待 ACP 消息超时")
		}
	}
}

func acpResponseID(m map[string]json.RawMessage, id int) bool {
	var got int
	return json.Unmarshal(m["id"], &got) == nil && got == id
}

// TestACPServe 以子进程等价方式跑一次真实 ACP 会话:
// initialize → session/new → session/prompt,并断言流式 session/update。
func TestACPServe(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../../.."); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errOut := &lockedBuffer{}
	done := make(chan int, 1)
	t.Cleanup(func() {
		_ = inW.Close()
		_ = outR.Close()
	})
	go func() {
		code := cli.RunWithIO([]string{"acp", "--preset", "examples/acp/acp.preset.json"}, inR, outW, errOut)
		_ = outW.Close()
		done <- code
	}()

	lines := make(chan map[string]json.RawMessage, 64)
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			var m map[string]json.RawMessage
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				lines <- m
			}
		}
		close(lines)
	}()

	send := func(id int, method string, params any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": method, "params": params,
		})
		if _, err := io.WriteString(inW, string(body)+"\n"); err != nil {
			t.Fatalf("写入请求失败: %v", err)
		}
	}

	send(1, "initialize", map[string]any{"protocolVersion": 1})
	init := waitACP(t, lines, func(m map[string]json.RawMessage) bool { return acpResponseID(m, 1) })
	if !strings.Contains(string(init["result"]), `"protocolVersion":1`) {
		t.Fatalf("initialize 结果不符: %s", init["result"])
	}

	send(2, "session/new", map[string]any{"cwd": "/workspace"})
	created := waitACP(t, lines, func(m map[string]json.RawMessage) bool { return acpResponseID(m, 2) })
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(created["result"], &session); err != nil || session.SessionID == "" {
		t.Fatalf("session/new 结果不符: %s err=%v", created["result"], err)
	}

	send(3, "session/prompt", map[string]any{
		"sessionId": session.SessionID,
		"prompt":    []map[string]string{{"type": "text", "text": "你好"}},
	})

	var promptDone, sawToolCall, sawToolResult bool
	var chunks []string
	deadline := time.After(20 * time.Second)
	for !promptDone || !sawToolCall || !sawToolResult ||
		!strings.Contains(strings.Join(chunks, ""), "你好，我是 ACP agent。") {
		select {
		case m, ok := <-lines:
			if !ok {
				t.Fatalf("ACP 输出提前结束,stderr=%s", errOut.String())
			}
			if acpResponseID(m, 3) {
				promptDone = true
				if !strings.Contains(string(m["result"]), `"stopReason":"end_turn"`) {
					t.Fatalf("session/prompt 结果不符: %s", m["result"])
				}
			}
			var method string
			_ = json.Unmarshal(m["method"], &method)
			if method != "session/update" {
				continue
			}
			var p struct {
				Update map[string]any `json:"update"`
			}
			if err := json.Unmarshal(m["params"], &p); err != nil {
				t.Fatalf("session/update 参数非法: %v", err)
			}
			switch p.Update["sessionUpdate"] {
			case "agent_message_chunk":
				if content, ok := p.Update["content"].(map[string]any); ok {
					if text, _ := content["text"].(string); text != "" {
						chunks = append(chunks, text)
					}
				}
			case "tool_call":
				sawToolCall = true
				if p.Update["toolCallId"] != "call-1" || p.Update["title"] != "tool_find" ||
					p.Update["kind"] != "search" || p.Update["status"] != "in_progress" {
					t.Fatalf("tool_call 映射不符: %+v", p.Update)
				}
			case "tool_call_update":
				sawToolResult = true
				if p.Update["toolCallId"] != "call-1" || p.Update["status"] != "completed" {
					t.Fatalf("tool_call_update 映射不符: %+v", p.Update)
				}
			}
		case <-deadline:
			t.Fatalf("等待 prompt 完成超时(chunks=%q tool=%v/%v),stderr=%s",
				strings.Join(chunks, ""), sawToolCall, sawToolResult, errOut.String())
		}
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "我先查一下工具") || !strings.Contains(joined, "你好，我是 ACP agent。") {
		t.Fatalf("assistant 文本未正确映射: %q", joined)
	}
	if strings.Contains(joined, `"message"`) {
		t.Fatalf("assistant 文本不应是原始 JSON: %q", joined)
	}

	_ = inW.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("acp 退出码 %d,stderr=%s", code, errOut.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("acp 没有退出,stderr=%s", errOut.String())
	}
}
