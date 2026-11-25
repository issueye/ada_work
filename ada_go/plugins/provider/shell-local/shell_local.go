// Package shelllocal 是 shell seam 的本地实现:无 shell 直接 exec、受管环境、
// 超时终止、输出上限、流式回调。
//
// 进程树终止:POSIX 用进程组;Windows 用 Job Object(见 process_windows.go),
// 完整 Job Object 支持列入 docs/seam-shell.md 的边界清单。
package shelllocal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// ProviderName 是 provider 名与插件 id。
const ProviderName = "shell-local"

// Options 配置本地执行。
type Options struct {
	// Workspace 是独立模式的 /workspace 根;托管模式下可为空。
	Workspace string
	// Tmp 是 /tmp 根;托管模式且 Workspace 为空时必须显式给出。
	Tmp string
	// WorkspaceChain 非 nil 时启用托管模式,优先用 workspace 服务解析 /workspace。
	WorkspaceChain *workspace.Chain
	AllowedEnv     []string
	AllowShell     bool
	ShellProgram   string
	MaxOutput      int64
}

// Shell 是本地实现。
type Shell struct {
	workspace    string
	tm           string
	env          *managedEnv
	allowShell   bool
	shellProgram string
	maxOutput    int64
	wschain      *workspace.Chain
}

// New 创建实现。
func New(opts Options) (*Shell, error) {
	if opts.Workspace == "" && opts.WorkspaceChain == nil {
		return nil, seamshell.Invalid("shell-local: Workspace 不能为空(或启用 workspace 托管模式)")
	}
	var ws string
	var err error
	if opts.Workspace != "" {
		ws, err = filepath.Abs(opts.Workspace)
		if err != nil {
			return nil, seamshell.Invalid(fmt.Sprintf("shell-local: Workspace 非法: %v", err))
		}
	}
	tm := opts.Tmp
	if tm == "" {
		if ws == "" {
			return nil, seamshell.Invalid("shell-local: 托管模式且 Workspace 为空时必须配置 tmp")
		}
		tm = filepath.Join(ws, ".ada-tmp")
	}
	if tm, err = filepath.Abs(tm); err != nil {
		return nil, seamshell.Invalid(fmt.Sprintf("shell-local: Tmp 非法: %v", err))
	}
	maxOutput := opts.MaxOutput
	if maxOutput <= 0 {
		maxOutput = seamshell.MaxOutputBytes
	}
	return &Shell{
		workspace: ws, tm: tm, wschain: opts.WorkspaceChain,
		env:          newManagedEnv(opts.AllowedEnv, os.Environ()),
		allowShell:   opts.AllowShell,
		shellProgram: opts.ShellProgram,
		maxOutput:    maxOutput,
	}, nil
}

// Name 返回 provider 名。
func (s *Shell) Name() string { return ProviderName }

// Env 返回受管环境注册表。
func (s *Shell) Env() seamshell.Env { return s.env }

// Run 执行一条命令,收集全部输出。
func (s *Shell) Run(ctx context.Context, cmd seamshell.Command) (seamshell.Result, error) {
	return s.exec(ctx, cmd, nil)
}

// Stream 边跑边推增量;onChunk 同步调用,慢消费者会形成回压。
func (s *Shell) Stream(ctx context.Context, cmd seamshell.Command, onChunk func(seamshell.Chunk) error) (seamshell.Result, error) {
	if onChunk == nil {
		return seamshell.Result{}, seamshell.Invalid("shell-local: Stream 需要 onChunk")
	}
	return s.exec(ctx, cmd, onChunk)
}

func (s *Shell) exec(ctx context.Context, cmd seamshell.Command, onChunk func(seamshell.Chunk) error) (seamshell.Result, error) {
	if err := ctx.Err(); err != nil {
		return seamshell.Result{}, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "执行前已取消", err)
	}
	if err := validateProgram(cmd.Program); err != nil {
		return seamshell.Result{}, err
	}
	timeout := cmd.Timeout
	if timeout == 0 {
		timeout = seamshell.DefaultTimeout
	}
	if timeout < 0 || timeout > seamshell.MaxTimeout {
		return seamshell.Result{}, seamshell.Invalid(fmt.Sprintf("shell-local: Timeout 非法(%s)", timeout))
	}
	dir, err := s.resolveDir(ctx, cmd.Dir)
	if err != nil {
		return seamshell.Result{}, err
	}
	env, err := s.buildEnv(cmd.Env)
	if err != nil {
		return seamshell.Result{}, err
	}

	program := cmd.Program
	args := append([]string(nil), cmd.Args...)
	if cmd.Shell {
		if !s.allowShell {
			return seamshell.Result{}, seamshell.Denied("sandbox", "shell 模式未启用(默认拒绝)")
		}
		if s.shellProgram == "" {
			return seamshell.Result{}, seamshell.Invalid("shell-local: ShellProgram 未配置")
		}
		line := strings.Join(append([]string{cmd.Program}, cmd.Args...), " ")
		program = s.shellProgram
		args = append(append([]string(nil), cmd.ShellArgs...), line)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(runCtx, program, args...)
	command.Dir = dir
	command.Env = env
	if len(cmd.Stdin) > 0 {
		command.Stdin = bytes.NewReader(cmd.Stdin)
	}
	configure(command)
	// 进程退出/超时后,子进程可能仍持有 stdout/stderr 管道;WaitDelay 让 Wait 不被无限拖住。
	command.WaitDelay = 2 * time.Second

	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		return seamshell.Result{}, err
	}
	stderrPipe, err := command.StderrPipe()
	if err != nil {
		return seamshell.Result{}, err
	}
	start := time.Now()
	if err := command.Start(); err != nil {
		return seamshell.Result{}, err
	}
	if err := afterStart(command); err != nil {
		terminate(command)
		return seamshell.Result{}, err
	}

	outCap := newCapture("stdout", s.maxOutput, onChunk, cancel)
	errCap := newCapture("stderr", s.maxOutput, onChunk, cancel)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(outCap, stdoutPipe) }()
	go func() { defer wg.Done(); _, _ = io.Copy(errCap, stderrPipe) }()

	// 超时/取消时终止进程(组);CommandContext 只杀父进程,这里补上整棵树。
	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			terminate(command)
		case <-done:
		}
	}()

	waitErr := command.Wait()
	close(done)
	wg.Wait()
	cleanup(command)
	duration := time.Since(start)

	chunkErr := outCap.chunkError()
	if chunkErr == nil {
		chunkErr = errCap.chunkError()
	}

	result := seamshell.Result{
		Stdout:    append([]byte(nil), outCap.bytes()...),
		Stderr:    append([]byte(nil), errCap.bytes()...),
		Truncated: outCap.overLimit() || errCap.overLimit(),
		Duration:  duration,
		Facts:     seamshell.Facts{Enforced: false, Backend: "none"},
	}
	if ps := command.ProcessState; ps != nil {
		result.ExitCode = ps.ExitCode()
	} else {
		result.ExitCode = -1
	}

	if result.Truncated {
		// 输出超限:命令被终止,结果照常返回(截断可见)。
		return result, nil
	}
	if chunkErr != nil {
		return result, seamerr.Wrap(seamshell.CodeInvalid, ProviderName, "流式回调失败", chunkErr)
	}
	if waitErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return result, seamshell.Timeout(fmt.Sprintf("命令超时(%s)", timeout), waitErr)
		}
		if runCtx.Err() == context.Canceled {
			return result, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "命令被取消", waitErr)
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil // 非零退出不是错误,退出码就是结果
		}
		return result, waitErr
	}
	return result, nil
}

func validateProgram(program string) error {
	if program == "" {
		return seamshell.Invalid("shell-local: Program 不能为空")
	}
	if strings.ContainsAny(program, " \t\r\n\x00") {
		return seamshell.Invalid(fmt.Sprintf("shell-local: Program 不能包含空白: %q", program))
	}
	return nil
}

func (s *Shell) resolveDir(ctx context.Context, p seamshell.Path) (string, error) {
	if p == "" {
		p = workspace.MountRoot
	}
	v := string(p)
	if strings.ContainsRune(v, 0) || strings.Contains(v, `\`) {
		return "", seamshell.Invalid("shell-local: Dir 含非法字符")
	}
	if !strings.HasPrefix(v, "/") {
		return "", seamshell.Invalid("shell-local: Dir 必须是虚拟路径")
	}
	clean := path.Clean(v)

	// 托管模式只接管 /workspace;/tmp 仍是本 provider 自己的目录。
	if s.wschain != nil && (clean == workspace.MountRoot || strings.HasPrefix(clean, workspace.MountRoot+"/")) {
		svc, gerr := s.wschain.Get()
		if gerr != nil {
			if s.workspace == "" {
				return "", gerr
			}
			// 已配置独立根:provider 未就绪时保持旧行为。
		} else {
			target, rerr := svc.ResolveHost(ctx, workspace.OpExec, clean)
			if rerr != nil {
				return "", rerr
			}
			if s.workspace != "" && !sameDirectory(s.workspace, target.Root) {
				return "", seamerr.New(workspace.CodeConflict, ProviderName, "shell-local: 配置的 workspace 与 workspace 服务根不一致", false)
			}
			info, ierr := os.Stat(target.Host)
			if ierr != nil {
				return "", ierr
			}
			if !info.IsDir() {
				return "", seamshell.Invalid(fmt.Sprintf("shell-local: Dir 不是目录: %s", p))
			}
			return target.Host, nil
		}
	}

	var root, rel string
	switch {
	case clean == "/workspace" || strings.HasPrefix(clean, "/workspace/"):
		root, rel = s.workspace, strings.TrimPrefix(clean, "/workspace")
	case clean == "/tmp" || strings.HasPrefix(clean, "/tmp/"):
		root, rel = s.tm, strings.TrimPrefix(clean, "/tmp")
	default:
		return "", seamshell.Denied("sandbox", fmt.Sprintf("不支持的虚拟根: %s", clean))
	}
	if root == "" {
		return "", seamerr.Unimplemented(ProviderName, "workspace 服务未就绪")
	}
	host := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	resolvedRoot := root
	if v, err := filepath.EvalSymlinks(root); err == nil {
		resolvedRoot = v
	}
	target, err := filepath.EvalSymlinks(host)
	if err != nil {
		if os.IsNotExist(err) {
			return "", &seamerr.Error{Code: seamshell.CodeNotFound, Provider: ProviderName, Message: fmt.Sprintf("工作目录不存在: %s", p)}
		}
		return "", err
	}
	if !withinDir(resolvedRoot, target) {
		return "", seamshell.Denied("sandbox", fmt.Sprintf("工作目录符号链接指向根外: %s", p))
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", seamshell.Invalid(fmt.Sprintf("shell-local: Dir 不是目录: %s", p))
	}
	return target, nil
}

// sameDirectory 用 os.SameFile 比较两个目录,避免大小写/符号链接导致的误判。
func sameDirectory(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// withinDir 判断 target 是否在 root 内(两者都应是已解析的真实路径)。
func withinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// buildEnv 只允许受管变量与 ADA_* 前缀的变量。
func (s *Shell) buildEnv(extra map[string]string) ([]string, error) {
	values := s.env.Snapshot()
	if len(extra) > 0 {
		for k, v := range extra {
			if !s.env.allowed(k) {
				return nil, seamshell.Invalid(fmt.Sprintf("shell-local: 环境变量 %q 不在白名单", k))
			}
			// 注入键也走归一化,避免同一变量以两种大小写同时进入子进程。
			values[envNameCanonical(k)] = v
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values[k])
	}
	return out, nil
}

// managedEnv 是受管环境注册表。
//
// values/names 的键在 Windows 上统一为大写:环境变量名在 Windows 不区分大小写,
// 且系统环境块常见写法是 "Path"/"SystemRoot"。按字面比较会把 PATH 整条丢掉,
// 子进程里所有外部命令都会变成「不是内部或外部命令」。
type managedEnv struct {
	values map[string]string
	names  map[string]bool
}

var defaultEnvNames = []string{"PATH", "PATHEXT", "SYSTEMROOT", "HOME", "TEMP", "TMP", "USERPROFILE", "LANG", "LC_ALL"}

// envNameCanonical 返回用于比较的环境变量名。
//
// 只影响比较键;子进程收到的仍是 "KEY=value"(Windows 不分大小写)。
func envNameCanonical(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func newManagedEnv(allowed []string, environ []string) *managedEnv {
	names := map[string]bool{}
	for _, n := range defaultEnvNames {
		names[envNameCanonical(n)] = true
	}
	for _, n := range allowed {
		if n = strings.TrimSpace(n); n != "" {
			names[envNameCanonical(n)] = true
		}
	}
	values := map[string]string{}
	for _, kv := range environ {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		ck := envNameCanonical(key)
		if names[ck] || strings.HasPrefix(ck, "ADA_") {
			values[ck] = val
			names[ck] = true
		}
	}
	env := &managedEnv{values: values, names: names}
	env.ensureWindowsDefaults()
	return env
}

// ensureWindowsDefaults 在 PATH/PATHEXT 缺失或为空时补系统默认值。
//
// 桌面宿主从快捷方式或裁剪过环境的启动器拉起 runtime 时,父进程可能没有 PATH;
// 这时连 where/findstr/curl 都找不到,agent 会把「命令不存在」误判成环境问题。
// 只在缺失时补,不覆盖显式 PATH(环境净化仍是策略默认值)。
func (e *managedEnv) ensureWindowsDefaults() {
	if runtime.GOOS != "windows" {
		return
	}
	if strings.TrimSpace(e.values["PATH"]) == "" {
		root := strings.TrimSpace(e.values["SYSTEMROOT"])
		if root == "" {
			// os.Getenv 在 Windows 上走 GetEnvironmentVariable,不区分大小写。
			root = strings.TrimSpace(os.Getenv("SystemRoot"))
		}
		if root == "" {
			root = `C:\Windows`
		}
		e.values["PATH"] = strings.Join([]string{
			filepath.Join(root, "System32"),
			root,
			filepath.Join(root, "System32", "Wbem"),
			filepath.Join(root, "System32", "WindowsPowerShell", "v1.0"),
		}, ";")
		e.names["PATH"] = true
	}
	if strings.TrimSpace(e.values["PATHEXT"]) == "" {
		e.values["PATHEXT"] = ".COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC"
		e.names["PATHEXT"] = true
	}
}

func (e *managedEnv) allowed(name string) bool {
	key := envNameCanonical(name)
	return e.names[key] || strings.HasPrefix(key, "ADA_")
}

func (e *managedEnv) Get(name string) (string, bool) {
	v, ok := e.values[envNameCanonical(name)]
	return v, ok
}

func (e *managedEnv) Snapshot() map[string]string {
	out := make(map[string]string, len(e.values))
	for k, v := range e.values {
		out[k] = v
	}
	return out
}

// capture 收集输出(有上限),同时按块回调。
type capture struct {
	stream  string
	limit   int64
	onChunk func(seamshell.Chunk) error
	onLimit func()

	mu         sync.Mutex
	buf        []byte
	over       bool
	seq        int
	cancelOnce sync.Once
	chunkErr   error
}

func newCapture(stream string, limit int64, onChunk func(seamshell.Chunk) error, onLimit func()) *capture {
	return &capture{stream: stream, limit: limit, onChunk: onChunk, onLimit: onLimit}
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	if !c.over {
		remaining := c.limit - int64(len(c.buf))
		if remaining <= 0 {
			c.over = true
		} else if int64(len(p)) > remaining {
			c.buf = append(c.buf, p[:remaining]...)
			c.over = true
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	over := c.over
	err := c.chunkErr
	c.mu.Unlock()

	if over {
		c.cancelOnce.Do(c.onLimit)
	}
	if c.onChunk != nil && len(p) > 0 && err == nil {
		text := p
		if int64(len(text)) > seamshell.MaxChunkBytes {
			text = text[:seamshell.MaxChunkBytes]
		}
		c.mu.Lock()
		c.seq++
		seq := c.seq
		c.mu.Unlock()
		if cerr := c.onChunk(seamshell.Chunk{Stream: c.stream, Text: string(text), Seq: seq}); cerr != nil {
			c.mu.Lock()
			c.chunkErr = cerr
			c.mu.Unlock()
			// 回压失败:终止命令,让等待方尽快返回。
			c.cancelOnce.Do(c.onLimit)
		}
	}
	return len(p), nil
}

func (c *capture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf
}

func (c *capture) overLimit() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.over
}

func (c *capture) chunkError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chunkErr
}

// Plugin 返回 shell-local 插件:inject shell seam,把本地实现安装进 Chain。
//
// workspace 是可选依赖:旧 preset 没有 workspace 栈时完全按自己的根工作。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamshell.Chain](ctx, "shell")
		if err != nil {
			return nil, err
		}
		var wschain *workspace.Chain
		if v, ok := ctx.TryService(workspace.ServiceName); ok {
			if ch, ok := v.(*workspace.Chain); ok {
				wschain = ch
			}
		}
		workspaceCfg := cfg.String("workspace")
		if wschain != nil && workspaceCfg != "" {
			if svc, gerr := wschain.Get(); gerr == nil {
				target, rerr := svc.ResolveHost(ctx.Context(), workspace.OpExec, workspace.MountRoot)
				if rerr != nil {
					return nil, rerr
				}
				abs, aerr := filepath.Abs(workspaceCfg)
				if aerr != nil || !sameDirectory(abs, target.Root) {
					return nil, seamerr.New(workspace.CodeConflict, ProviderName, "shell-local: 配置的 workspace 与 workspace 服务根不一致", false)
				}
			}
		}
		impl, err := New(Options{
			Workspace:      workspaceCfg,
			Tmp:            cfg.String("tmp"),
			WorkspaceChain: wschain,
			AllowedEnv:     cfg.Strings("env"),
			AllowShell:     cfg.Bool("allowShell"),
			ShellProgram:   cfg.String("shellProgram"),
		})
		if err != nil {
			return nil, err
		}
		return chain.Install(impl)
	}).WithInject("shell").WithOptional(workspace.ServiceName)
}
