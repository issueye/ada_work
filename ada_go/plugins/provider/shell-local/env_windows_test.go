package shelllocal

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

// TestManagedEnvWindowsNameCase 覆盖 Windows 环境变量名大小写不敏感。
//
// 系统环境块里常见写法是 "Path"/"SystemRoot"/"Pathext",而受管白名单按上表写
// "PATH"/"SYSTEMROOT"/"PATHEXT"。按字面比较会把 PATH 整条丢掉,子进程里所有
// 外部命令都会变成「不是内部或外部命令」。
func TestManagedEnvWindowsNameCase(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 环境变量名大小写不敏感;其它平台按字面比较")
	}
	env := newManagedEnv(nil, []string{
		`Path=C:\Windows\System32`,
		`SystemRoot=C:\Windows`,
		`Pathext=.COM;.EXE`,
	})
	for _, name := range []string{"PATH", "Path", "SYSTEMROOT", "SystemRoot", "PATHEXT"} {
		if _, ok := env.Get(name); !ok {
			t.Fatalf("受管环境应按大小写不敏感查找 %q", name)
		}
	}
	if got, _ := env.Get("PATH"); got != `C:\Windows\System32` {
		t.Fatalf("PATH 值不正确: %q", got)
	}
	if _, ok := env.Snapshot()["PATH"]; !ok {
		t.Fatalf("快照应包含归一化后的 PATH,实际 %v", env.Snapshot())
	}
}

// TestManagedEnvWindowsPathFallback 覆盖 PATH/PATHEXT 完全缺失时的系统默认值。
//
// 桌面宿主可能只继承了 USERPROFILE/TEMP 这类变量;没有 PATH 时连 where/findstr
// 都找不到,agent 会把「命令不存在」误判成仓库或环境问题。
func TestManagedEnvWindowsPathFallback(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("只有 Windows 需要补系统默认 PATH")
	}
	env := newManagedEnv(nil, []string{`USERPROFILE=C:\Users\x`, `TEMP=C:\Temp`})
	path, ok := env.Get("PATH")
	if !ok || !strings.Contains(strings.ToLower(path), "system32") {
		t.Fatalf("PATH 缺失时应补系统目录,实际 %q", path)
	}
	if _, ok := env.Get("PATHEXT"); !ok {
		t.Fatalf("PATHEXT 缺失时应补默认扩展名")
	}
}

// TestManagedEnvWindowsPathKept 确认显式提供的 PATH 不会被默认值改写。
func TestManagedEnvWindowsPathKept(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("只有 Windows 需要补系统默认 PATH")
	}
	env := newManagedEnv(nil, []string{`PATH=D:\tools;D:\sdk`})
	if got, _ := env.Get("PATH"); got != `D:\tools;D:\sdk` {
		t.Fatalf("已有 PATH 不应被改写: %q", got)
	}
}

// TestRunSeesMixedCasePath 是端到端回归:进程环境里只有混合大小写的 "Path"
// (资源管理器/快捷方式启动的常见形态),run_shell 执行的 cmd 仍应看到 PATH。
func TestRunSeesMixedCasePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("只覆盖 Windows 环境变量名大小写")
	}
	path, ok := os.LookupEnv("PATH")
	if !ok || path == "" {
		t.Skip("当前测试进程没有 PATH,无法解析 cmd")
	}
	// 清掉原 PATH 再以 "Path" 写回,模拟系统环境块的常见大小写。
	if err := os.Unsetenv("PATH"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("Path", path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Unsetenv("Path")
		_ = os.Setenv("PATH", path)
	})

	s, err := New(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("创建 shell-local 失败: %v", err)
	}
	res, err := s.Run(context.Background(), seamshell.Command{
		Program: "cmd", Args: []string{"/c", "echo", "%PATH%"},
	})
	if err != nil {
		t.Fatalf("执行 cmd 失败: %v", err)
	}
	out := strings.TrimSpace(string(res.Stdout))
	if strings.Contains(out, "%PATH%") {
		t.Fatalf("PATH 没有进入子进程环境(%%PATH%% 未展开): %q", out)
	}
	if out == "" {
		t.Fatalf("PATH 展开为空,子进程可能拿到了空环境")
	}
}
