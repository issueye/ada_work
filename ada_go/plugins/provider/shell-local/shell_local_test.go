package shelllocal_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	shelllocal "github.com/issueye/ada_go_v2/plugins/provider/shell-local"
	workspacelocal "github.com/issueye/ada_go_v2/plugins/provider/workspace-local"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

func errCode(err error) seamerr.Code {
	var e *seamerr.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func newShell(t *testing.T, opts shelllocal.Options) *shelllocal.Shell {
	t.Helper()
	if opts.Workspace == "" {
		opts.Workspace = t.TempDir()
	}
	s, err := shelllocal.New(opts)
	if err != nil {
		t.Fatalf("创建 shell-local 失败: %v", err)
	}
	return s
}

func echoCommand(args ...string) seamshell.Command {
	if runtime.GOOS == "windows" {
		return seamshell.Command{Program: "cmd", Args: append([]string{"/c", "echo"}, args...)}
	}
	return seamshell.Command{Program: "echo", Args: args}
}

func sleepCommand() seamshell.Command {
	// Windows 用测试二进制自身作为长任务,避免 powershell 启动/杀进程的额外抖动。
	if runtime.GOOS == "windows" {
		return seamshell.Command{
			Program: os.Args[0],
			Args:    []string{"-test.run=TestShellHelperProcess"},
			Env:     map[string]string{"ADA_SHELL_HELPER": "1"},
		}
	}
	return seamshell.Command{Program: "sleep", Args: []string{"20"}}
}

func exitCommand(code string) seamshell.Command {
	if runtime.GOOS == "windows" {
		return seamshell.Command{Program: "cmd", Args: []string{"/c", "exit /b " + code}}
	}
	return seamshell.Command{Program: "sh", Args: []string{"-c", "exit " + code}}
}

func pwdCommand() seamshell.Command {
	if runtime.GOOS == "windows" {
		return seamshell.Command{Program: "cmd", Args: []string{"/c", "cd"}}
	}
	return seamshell.Command{Program: "pwd"}
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func newManagedShell(t *testing.T, cfgRoot string, workspaces []workspacelocal.Workspace) *shelllocal.Shell {
	t.Helper()
	chain := workspace.NewChain()
	provider, err := workspacelocal.New(workspacelocal.Options{Active: workspaces[0].ID, Workspaces: workspaces}, nil)
	if err != nil {
		t.Fatalf("创建 workspace provider 失败: %v", err)
	}
	if _, err := chain.Install(provider); err != nil {
		t.Fatalf("安装 workspace provider 失败: %v", err)
	}
	s, err := shelllocal.New(shelllocal.Options{
		Workspace: cfgRoot, Tmp: t.TempDir(), WorkspaceChain: chain,
	})
	if err != nil {
		t.Fatalf("创建托管 shell-local 失败: %v", err)
	}
	return s
}

// 1. 基本执行:输出、退出码、耗时、Facts 可见。
func TestRunEcho(t *testing.T) {
	s := newShell(t, shelllocal.Options{})
	res, err := s.Run(context.Background(), echoCommand("hello"))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Fatalf("输出不正确: %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("退出码应为 0,实际 %d", res.ExitCode)
	}
	if res.Duration <= 0 {
		t.Fatalf("应记录耗时")
	}
	if res.Facts.Enforced {
		t.Fatalf("未接沙箱时 Facts.Enforced 应为 false(结果必须如实反映)")
	}
}

// 2. 环境净化:未白名单的变量不可见,注入也失败。
func TestEnvIsolation(t *testing.T) {
	s := newShell(t, shelllocal.Options{AllowedEnv: []string{"PATH"}})
	if _, ok := s.Env().Get("SECRET"); ok {
		t.Fatalf("SECRET 不应出现在受管环境")
	}
	if _, ok := s.Env().Get("PATH"); !ok {
		t.Fatalf("PATH 应在受管环境里")
	}
	if _, err := s.Run(context.Background(), seamshell.Command{Program: echoProgram(), Env: map[string]string{"SECRET": "x"}}); err == nil || errCode(err) != seamshell.CodeInvalid {
		t.Fatalf("注入未白名单变量应 EINVALID,实际 %v", err)
	}
}

// 3. Program 含空白:拒绝(防止被误当成命令串)。
func TestRejectsProgramWithSpace(t *testing.T) {
	s := newShell(t, shelllocal.Options{})
	if _, err := s.Run(context.Background(), seamshell.Command{Program: "echo; rm -rf /"}); err == nil || errCode(err) != seamshell.CodeInvalid {
		t.Fatalf("含空白的 Program 应拒绝,实际 %v", err)
	}
}

// 4. 超时:ETIMEOUT,且及时返回。
func TestTimeout(t *testing.T) {
	s := newShell(t, shelllocal.Options{})
	cmd := sleepCommand()
	cmd.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := s.Run(context.Background(), cmd)
	if err == nil || errCode(err) != seamshell.CodeTimeout {
		t.Fatalf("超时应 ETIMEOUT,实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时没有及时返回: %s", elapsed)
	}
}

// TestShellHelperProcess 是 sleepCommand 的 Windows 子进程:
// 只有被 shell-local 以 ADA_SHELL_HELPER=1 启动时才长时间休眠。
func TestShellHelperProcess(t *testing.T) {
	if os.Getenv("ADA_SHELL_HELPER") != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(5 * time.Second)
}

// 5. 输出上限:截断可见,命令被终止。
func TestOutputTruncation(t *testing.T) {
	s := newShell(t, shelllocal.Options{MaxOutput: 64})
	long := strings.Repeat("x", 200)
	res, err := s.Run(context.Background(), echoCommand(long))
	if err != nil {
		t.Fatalf("截断不应作为错误返回: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("应标记 Truncated")
	}
	if int64(len(res.Stdout)) > 64 {
		t.Fatalf("输出不应超过上限,实际 %d", len(res.Stdout))
	}
}

// 5.1 Dir 是根外符号链接:拒绝,命令不得在根外执行。
func TestDirSymlinkEscape(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Skipf("平台无法创建符号链接: %v", err)
	}
	s := newShell(t, shelllocal.Options{Workspace: ws})
	cmd := echoCommand("x")
	cmd.Dir = "/workspace/link"
	if _, err := s.Run(context.Background(), cmd); err == nil {
		t.Fatalf("根外符号链接目录应拒绝")
	}
}

func TestStream(t *testing.T) {
	s := newShell(t, shelllocal.Options{})
	var text strings.Builder
	chunks := 0
	res, err := s.Stream(context.Background(), echoCommand("hello"), func(c seamshell.Chunk) error {
		chunks++
		text.WriteString(c.Text)
		return nil
	})
	if err != nil {
		t.Fatalf("流式执行失败: %v", err)
	}
	if chunks == 0 {
		t.Fatalf("应至少收到一个 chunk")
	}
	if !strings.Contains(text.String(), "hello") {
		t.Fatalf("chunk 内容不正确: %q", text.String())
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Fatalf("汇总输出不正确: %q", res.Stdout)
	}
}

// 7. Shell=true 默认拒绝。
func TestShellModeDenied(t *testing.T) {
	s := newShell(t, shelllocal.Options{})
	if _, err := s.Run(context.Background(), seamshell.Command{Program: "echo", Shell: true}); err == nil || errCode(err) != seamshell.CodeDenied {
		t.Fatalf("Shell=true 默认应 EDENIED,实际 %v", err)
	}
}

// 8. 非零退出码不是 seam 错误,退出码就是结果。
func TestNonZeroExit(t *testing.T) {
	s := newShell(t, shelllocal.Options{})
	res, err := s.Run(context.Background(), exitCommand("3"))
	if err != nil {
		t.Fatalf("非零退出不应返回错误: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("退出码应为 3,实际 %d", res.ExitCode)
	}
}

// 9. 工作目录:虚拟路径越界拒绝。
func TestDirEscape(t *testing.T) {
	s := newShell(t, shelllocal.Options{})
	if _, err := s.Run(context.Background(), seamshell.Command{Program: echoProgram(), Dir: "/workspace/../etc"}); err == nil {
		t.Fatalf("越界 Dir 应拒绝")
	}
}

func echoProgram() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "echo"
}

// 10. 托管模式:默认 cwd 落到 provider 根,绑定优先,显式 Dir 受根检查,/tmp 仍本地。
func TestManagedWorkspace(t *testing.T) {
	root1, root2 := t.TempDir(), t.TempDir()
	s := newManagedShell(t, "", []workspacelocal.Workspace{
		{ID: "alpha", Root: root1}, {ID: "beta", Root: root2},
	})
	ctx := context.Background()

	res, err := s.Run(ctx, pwdCommand())
	if err != nil {
		t.Fatalf("默认 cwd 执行失败: %v", err)
	}
	if !samePath(strings.TrimSpace(string(res.Stdout)), root1) {
		t.Fatalf("默认 cwd 应为 alpha 根: %q want %q", res.Stdout, root1)
	}

	bound := workspace.WithBinding(ctx, workspace.Binding{SessionID: "s1", Workspace: "beta"})
	res, err = s.Run(bound, pwdCommand())
	if err != nil {
		t.Fatalf("绑定 cwd 执行失败: %v", err)
	}
	if !samePath(strings.TrimSpace(string(res.Stdout)), root2) {
		t.Fatalf("绑定 cwd 应为 beta 根: %q want %q", res.Stdout, root2)
	}

	if err := os.MkdirAll(filepath.Join(root1, "sub"), 0o755); err != nil {
		t.Fatalf("创建子目录失败: %v", err)
	}
	res, err = s.Run(ctx, seamshell.Command{Program: pwdCommand().Program, Args: pwdCommand().Args, Dir: "/workspace/sub"})
	if err != nil {
		t.Fatalf("显式 Dir 执行失败: %v", err)
	}
	if !samePath(strings.TrimSpace(string(res.Stdout)), filepath.Join(root1, "sub")) {
		t.Fatalf("显式 Dir 不正确: %q", res.Stdout)
	}

	res, err = s.Run(ctx, seamshell.Command{Program: pwdCommand().Program, Args: pwdCommand().Args, Dir: "/tmp"})
	if err != nil {
		t.Fatalf("/tmp 执行失败: %v", err)
	}
	if samePath(strings.TrimSpace(string(res.Stdout)), root1) {
		t.Fatalf("/tmp 不应落在工作区根")
	}
}

// 11. 托管模式:只读工作区拒绝执行;根不一致 ECONFLICT。
func TestManagedReadOnlyAndConflict(t *testing.T) {
	root1, root2 := t.TempDir(), t.TempDir()
	s := newManagedShell(t, "", []workspacelocal.Workspace{{ID: "alpha", Root: root1, ReadOnly: true}})
	if _, err := s.Run(context.Background(), pwdCommand()); err == nil || errCode(err) != workspace.CodeDenied {
		t.Fatalf("只读工作区执行应 EDENIED,实际 %v", err)
	}

	bad := newManagedShell(t, root2, []workspacelocal.Workspace{{ID: "alpha", Root: root1}})
	if _, err := bad.Run(context.Background(), pwdCommand()); err == nil || errCode(err) != workspace.CodeConflict {
		t.Fatalf("根不一致应 ECONFLICT,实际 %v", err)
	}
}

// 12. 托管模式未就绪:有 seam 无 provider 且无独立根 → EUNIMPLEMENTED。
func TestManagedNotReady(t *testing.T) {
	chain := workspace.NewChain()
	s, err := shelllocal.New(shelllocal.Options{Tmp: t.TempDir(), WorkspaceChain: chain})
	if err != nil {
		t.Fatalf("创建托管 shell-local 失败: %v", err)
	}
	if _, err := s.Run(context.Background(), pwdCommand()); err == nil || !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("provider 未就绪应 EUNIMPLEMENTED,实际 %v", err)
	}
}
