// Package cli 是 host/cli 适配器:把 ada 的命令实现从 main 中分离出来,
// 以 io.Writer 注入输出,便于测试与嵌入其它宿主。
//
// 它只做平台映射(参数解析、退出码、文本输出),业务仍在内核/插件/基础设施里。
package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/issueye/ada_go_v2/platform/assemble"
	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/install"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/platform/preset"
	agentloop "github.com/issueye/ada_go_v2/plugins/core/agent-loop"
	controlacp "github.com/issueye/ada_go_v2/plugins/provider/control-acp"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	seamcontrol "github.com/issueye/ada_go_v2/plugins/seam/control"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamsession "github.com/issueye/ada_go_v2/plugins/seam/session"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
	seamworkspace "github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// Run 执行一条命令并返回退出码:0 成功 / 1 计划或装配失败 / 2 用法错误。
func Run(args []string, out, errOut io.Writer) int {
	return RunWithIO(args, os.Stdin, out, errOut)
}

// RunWithIO 与 Run 相同,但显式注入标准输入。
//
// 只有流式命令(acp)会读 in;其余命令忽略它。单独暴露是为了让测试
// 不必替换 os.Stdin。
func RunWithIO(args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)
		return 2
	}
	switch args[0] {
	case "plan":
		return cmdPlan(args[1:], out, errOut)
	case "up":
		return cmdUp(args[1:], out, errOut)
	case "inspect":
		return cmdInspect(args[1:], out, errOut)
	case "agent":
		return cmdAgent(args[1:], out, errOut)
	case "acp":
		return cmdACP(args[1:], in, out, errOut)
	case "verify":
		return cmdVerify(args[1:], out, errOut)
	case "install":
		return cmdInstall(args[1:], out, errOut)
	case "list":
		return cmdList(args[1:], out, errOut)
	case "uninstall":
		return cmdUninstall(args[1:], out, errOut)
	case "upgrade":
		return cmdUpgrade(args[1:], out, errOut)
	case "rollback":
		return cmdRollback(args[1:], out, errOut)
	case "caps":
		return cmdCaps(args[1:], out, errOut)
	case "repo":
		return cmdRepo(args[1:], out, errOut)
	case "check":
		return cmdCheck(args[1:], out, errOut)
	case "serve-repo":
		return cmdServeRepo(args[1:], out, errOut)
	case "serve":
		return cmdServe(args[1:], out, errOut)
	case "update":
		return cmdUpdate(args[1:], out, errOut)
	default:
		usage(errOut)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `用法:
  ada plan    <插件目录|--preset 文件> [-json]
  ada up      <插件目录|--preset 文件>
  ada inspect <插件目录|--preset 文件> [-json] [-chain]
  ada agent   <插件目录|--preset 文件> -message <文本> [-workspace <id>] [-json]
  ada acp     <插件目录|--preset 文件>
  ada verify  <插件目录> [-keys 密钥文件] [-require-signature]
  ada install <插件目录> -dest <安装根> [-keys 密钥文件] [-require-signature] [-yes]
  ada list    -dest <安装根>
  ada uninstall <插件 id> -dest <安装根>
  ada upgrade <插件目录> -dest <安装根> [-keys 密钥文件] [-require-signature] [-yes] [-keep]
  ada rollback <插件 id> -dest <安装根>
  ada caps    <插件目录|--preset 文件> [-json]
  ada repo    <仓库索引 URL> [-keys 密钥文件] [-require-repo-signature] [-json]
  ada check   <插件目录|--preset 文件> [-json]
  ada serve-repo -root <目录> -addr <地址> [-token <token>]
  ada serve   -root <目录> -addr <地址> [-token <token>] [-rate-limit N]
  ada update  <插件 id> -repo <仓库目录> |-repo-url <索引 URL> -dest <安装根> [-keys 密钥文件] [-require-signature] [-require-repo-signature] [-yes]

acp 在 stdin/stdout 上说 JSON-RPC 2.0(ACP);stdout 只输出协议消息,日志一律走 stderr。

退出码:0=成功,1=计划不可执行/装配或校验失败,2=用法错误。
`)
}

type target struct {
	dir       string
	preset    string
	workspace string
	json      bool
	chain     bool
	bad       string
}

func parseTarget(args []string) target {
	var t target
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-json":
			t.json = true
		case a == "-chain" || a == "--chain":
			t.chain = true
		case a == "--preset" || a == "-preset":
			if i+1 >= len(args) {
				t.bad = "--preset 缺少文件参数"
				return t
			}
			t.preset = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			t.bad = "未知参数 " + a
			return t
		default:
			if t.dir == "" {
				t.dir = a
			} else {
				t.bad = "多余的参数 " + a
				return t
			}
		}
	}
	if t.dir != "" && t.preset != "" {
		t.bad = "目录与 --preset 不能同时给出"
	}
	return t
}

func loadTarget(t target) ([]*plugin.Manifest, error) {
	switch {
	case t.preset != "":
		p, err := preset.Load(t.preset)
		if err != nil {
			return nil, err
		}
		return p.Expand()
	case t.dir != "":
		// 安装根目录要先按 index.json 的 trust 加载;否则 TS 插件会被降为 third-party。
		if _, err := os.Stat(filepath.Join(t.dir, install.IndexFile)); err == nil {
			return install.LoadDir(t.dir)
		}
		return plugin.LoadDir(t.dir)
	default:
		return nil, fmt.Errorf("需要给出插件目录或 --preset 文件")
	}
}

// mountAll 是 up 与 inspect 共用的挂载逻辑:失败时停掉半装的 app。
func mountAll(manifests []*plugin.Manifest, report *assemble.Report, out io.Writer) (*kernel.App, *assemble.UpReport, error) {
	if err := builtins.ApplyAutoGrants(manifests); err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelWarn}))
	app := kernel.New(kernel.Options{Logger: logger})
	lookup := loader.Lookup(loader.Options{
		// 内置 Go 插件注册表:CLI 也能挂安全栈;第三方 Go 插件仍需宿主编译时注册。
		Go: builtins.Plugins(),
		Logf: func(format string, args ...any) {
			fmt.Fprintf(out, "  [js] %s\n", fmt.Sprintf(format, args...))
		},
	})
	up, err := assemble.Up(app, manifests, report, lookup)
	if err != nil {
		_ = app.Stop(context.Background())
		return nil, nil, err
	}
	return app, up, nil
}

func cmdPlan(args []string, out, errOut io.Writer) int {
	t := parseTarget(args)
	if t.bad != "" {
		fmt.Fprintln(errOut, t.bad)
		usage(errOut)
		return 2
	}
	manifests, err := loadTarget(t)
	if err != nil {
		fmt.Fprintln(errOut, "加载失败:", err)
		return 1
	}
	report := assemble.Plan(manifests)

	if t.json {
		payload := struct {
			OK       bool               `json:"ok"`
			Plugins  []string           `json:"plugins"`
			Order    []string           `json:"order"`
			Warnings []string           `json:"warnings,omitempty"`
			Problems []assemble.Problem `json:"problems,omitempty"`
		}{OK: report.OK(), Order: report.Order, Warnings: report.Warnings, Problems: report.Problems}
		for _, m := range manifests {
			payload.Plugins = append(payload.Plugins, m.String())
		}
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintln(errOut, "序列化失败:", err)
			return 1
		}
		fmt.Fprintln(out, string(b))
		if !report.OK() {
			return 1
		}
		return 0
	}

	names := make([]string, 0, len(manifests))
	for _, m := range manifests {
		names = append(names, m.String())
	}
	fmt.Fprintf(out, "清单(%d): %s\n", len(names), strings.Join(names, ", "))
	order := strings.Join(report.Order, " → ")
	if order == "" {
		order = "(空)"
	}
	fmt.Fprintln(out, "顺序:", order)
	for _, w := range report.Warnings {
		fmt.Fprintln(out, "警告:", w)
	}
	for _, p := range report.Problems {
		fmt.Fprintln(out, "问题:", p.String())
	}
	if !report.OK() {
		fmt.Fprintln(out, "结果: 不可装配")
		return 1
	}
	fmt.Fprintln(out, "结果: 可装配")
	return 0
}

func cmdUp(args []string, out, errOut io.Writer) int {
	t := parseTarget(args)
	if t.bad != "" {
		fmt.Fprintln(errOut, t.bad)
		usage(errOut)
		return 2
	}
	manifests, err := loadTarget(t)
	if err != nil {
		fmt.Fprintln(errOut, "加载失败:", err)
		return 1
	}
	report := assemble.Plan(manifests)
	if !report.OK() {
		for _, w := range report.Warnings {
			fmt.Fprintln(out, "警告:", w)
		}
		for _, p := range report.Problems {
			fmt.Fprintln(out, "问题:", p.String())
		}
		fmt.Fprintln(out, "结果: 不可装配")
		return 1
	}

	app, up, err := mountAll(manifests, report, out)
	if err != nil {
		fmt.Fprintln(errOut, "装配失败:", err)
		return 1
	}
	ctx := context.Background()

	fmt.Fprintln(out, "装配完成:", strings.Join(up.Order, " → "))
	snap := app.Inspect()
	for _, f := range snap.Fibers {
		fmt.Fprintf(out, "  fiber   %-12s %s\n", f.Plugin, f.State)
	}
	for _, s := range snap.Services {
		fmt.Fprintf(out, "  service %-12s provider=%s dependents=%d\n", s.Name, s.Provider, len(s.Dependents))
	}

	if err := app.Stop(ctx); err != nil {
		fmt.Fprintln(errOut, "停止时出错:", err)
		return 1
	}
	fmt.Fprintln(out, "已停止。")
	return 0
}

type jsonFiber struct {
	ID     string `json:"id"`
	Plugin string `json:"plugin"`
	State  string `json:"state"`
	Epoch  uint64 `json:"epoch"`
	Parent string `json:"parent,omitempty"`
}

type jsonService struct {
	Name       string   `json:"name"`
	Version    int      `json:"version"`
	Kind       string   `json:"kind"`
	Provider   string   `json:"provider"`
	Dependents []string `json:"dependents,omitempty"`
}

type jsonEffect struct {
	Fiber  string `json:"fiber"`
	Plugin string `json:"plugin"`
	Label  string `json:"label"`
}

// parseAgentArgs 解析 `ada agent`;target 参数与 plan/up 保持一致。
func parseAgentArgs(args []string) (target, string, string) {
	var t target
	var message, bad string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-json":
			t.json = true
		case a == "-message" || a == "--message":
			if i+1 >= len(args) {
				return t, message, "-message 缺少文本参数"
			}
			message = args[i+1]
			i++
		case a == "--preset" || a == "-preset":
			if i+1 >= len(args) {
				return t, message, "--preset 缺少文件参数"
			}
			t.preset = args[i+1]
			i++
		case a == "-workspace" || a == "--workspace":
			if i+1 >= len(args) {
				return t, message, "--workspace 缺少工作区 id"
			}
			t.workspace = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			return t, message, "未知参数 " + a
		default:
			if t.dir == "" {
				t.dir = a
			} else {
				return t, message, "多余的参数 " + a
			}
		}
	}
	if t.dir != "" && t.preset != "" {
		bad = "目录与 --preset 不能同时给出"
	}
	if message == "" {
		bad = "-message 不能为空"
	}
	return t, message, bad
}

func cmdAgent(args []string, out, errOut io.Writer) int {
	t, message, bad := parseAgentArgs(args)
	if bad != "" {
		fmt.Fprintln(errOut, bad)
		usage(errOut)
		return 2
	}
	manifests, err := loadTarget(t)
	if err != nil {
		fmt.Fprintln(errOut, "加载失败:", err)
		return 1
	}
	report := assemble.Plan(manifests)
	if !report.OK() {
		for _, p := range report.Problems {
			fmt.Fprintln(out, "问题:", p.String())
		}
		fmt.Fprintln(out, "结果: 不可装配")
		return 1
	}
	app, _, err := mountAll(manifests, report, out)
	if err != nil {
		fmt.Fprintln(errOut, "装配失败:", err)
		return 1
	}
	defer func() { _ = app.Stop(context.Background()) }()

	raw, err := app.Root().Service("agent")
	if err != nil {
		fmt.Fprintln(errOut, "没有 agent 服务:", err)
		return 1
	}
	ag, ok := raw.(*agentloop.Agent)
	if !ok {
		fmt.Fprintln(errOut, "agent 服务类型不正确")
		return 1
	}
	ctx := context.Background()
	meta := seamsession.Meta{Title: "cli"}
	if t.workspace != "" {
		// 只接工作区 ID,不接路径:根由 preset 配置,命令行不绕过宿主策略。
		chain, err := kernel.Service[*seamworkspace.Chain](app.Root(), seamworkspace.ServiceName)
		if err != nil {
			fmt.Fprintln(errOut, "没有 workspace 服务:", err)
			return 1
		}
		svc, err := chain.Get()
		if err != nil {
			fmt.Fprintln(errOut, "没有 workspace provider:", err)
			return 1
		}
		if _, err := svc.Get(ctx, seamworkspace.ID(t.workspace)); err != nil {
			fmt.Fprintln(errOut, "工作区不存在:", err)
			return 1
		}
		meta.Workspace = t.workspace
	}
	sess, err := ag.Open(ctx, meta)
	if err != nil {
		fmt.Fprintln(errOut, "打开会话失败:", err)
		return 1
	}
	res, err := ag.Run(ctx, sess, message)
	if err != nil {
		fmt.Fprintln(errOut, "agent 运行失败:", err)
		return 1
	}
	if t.json {
		payload := struct {
			OK        bool        `json:"ok"`
			TurnID    string      `json:"turnId"`
			Text      string      `json:"text"`
			Steps     int         `json:"steps"`
			ToolCalls int         `json:"toolCalls"`
			Usage     interface{} `json:"usage"`
		}{OK: true, TurnID: res.TurnID, Text: res.Text, Steps: res.Steps, ToolCalls: res.ToolCalls, Usage: res.Usage}
		b, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	if res.Text == "" {
		fmt.Fprintln(out, "(空回复)")
	} else {
		fmt.Fprintln(out, res.Text)
	}
	return 0
}

// cmdACP 在 stdin/stdout 上跑 ACP,直到对端关闭输入。
//
// 它只做宿主映射:装配 preset、取 control 服务、把连接交给 control-acp。
// stdout 只放协议消息,日志/警告/装配输出全部走 stderr,避免污染 JSON-RPC。
func cmdACP(args []string, in io.Reader, out, errOut io.Writer) int {
	t := parseTarget(args)
	if t.bad != "" {
		fmt.Fprintln(errOut, t.bad)
		usage(errOut)
		return 2
	}
	manifests, err := loadTarget(t)
	if err != nil {
		fmt.Fprintln(errOut, "加载失败:", err)
		return 1
	}
	report := assemble.Plan(manifests)
	if !report.OK() {
		for _, p := range report.Problems {
			fmt.Fprintln(errOut, "问题:", p.String())
		}
		fmt.Fprintln(errOut, "结果: 不可装配")
		return 1
	}
	app, _, err := mountAll(manifests, report, errOut)
	if err != nil {
		fmt.Fprintln(errOut, "装配失败:", err)
		return 1
	}
	defer func() { _ = app.Stop(context.Background()) }()

	raw, err := app.Root().Service(seamcontrol.ServiceName)
	if err != nil {
		fmt.Fprintln(errOut, "没有 control 服务(需要在 preset 里启用 agent-control):", err)
		return 1
	}
	inv, ok := raw.(seamcontrol.Invoker)
	if !ok {
		fmt.Fprintln(errOut, "control 服务类型不正确")
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := jsonrpc2.New(ctx, &stdioRWC{in: in, out: out})
	ep := controlacp.New(conn, seamcontrol.New(inv))
	defer func() { _ = ep.Close() }()
	ep.BindUpdates(app.Root())
	// 有 approval 服务时把权限交给 ACP client;没有就保持「未实现」,不静默放行。
	if _, ok := app.Root().TryService("approval"); ok {
		if _, err := ep.BindApproval(app.Root()); err != nil {
			fmt.Fprintln(errOut, "注册 ACP 审批人失败:", err)
			return 1
		}
	}
	<-conn.Done()
	return 0
}

// stdioRWC 把 stdin/stdout 合成双向流;Close 不关闭真实 stdio,
// 所有权仍属于宿主进程。
type stdioRWC struct {
	in  io.Reader
	out io.Writer
}

func (s *stdioRWC) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s *stdioRWC) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *stdioRWC) Close() error                { return nil }

func cmdInspect(args []string, out, errOut io.Writer) int {
	t := parseTarget(args)
	if t.bad != "" {
		fmt.Fprintln(errOut, t.bad)
		usage(errOut)
		return 2
	}
	manifests, err := loadTarget(t)
	if err != nil {
		fmt.Fprintln(errOut, "加载失败:", err)
		return 1
	}
	report := assemble.Plan(manifests)
	if !report.OK() {
		for _, w := range report.Warnings {
			fmt.Fprintln(out, "警告:", w)
		}
		for _, p := range report.Problems {
			fmt.Fprintln(out, "问题:", p.String())
		}
		fmt.Fprintln(out, "结果: 不可装配")
		return 1
	}

	app, up, err := mountAll(manifests, report, out)
	if err != nil {
		fmt.Fprintln(errOut, "装配失败:", err)
		return 1
	}
	defer func() { _ = app.Stop(context.Background()) }()
	snap := app.Inspect()

	if t.json {
		payload := struct {
			Order    []string         `json:"order"`
			Fibers   []jsonFiber      `json:"fibers"`
			Services []jsonService    `json:"services"`
			Effects  []jsonEffect     `json:"effects"`
			Events   []string         `json:"events"`
			Chains   map[string][]int `json:"chains,omitempty"`
		}{Order: up.Order, Events: snap.Events}
		if t.chain {
			payload.Chains = inspectChains(app)
		}
		for _, f := range snap.Fibers {
			payload.Fibers = append(payload.Fibers, jsonFiber{ID: f.ID, Plugin: f.Plugin, State: f.State, Epoch: f.Epoch, Parent: f.Parent})
		}
		for _, s := range snap.Services {
			payload.Services = append(payload.Services, jsonService{Name: s.Name, Version: s.Version, Kind: s.Kind, Provider: s.Provider, Dependents: s.Dependents})
		}
		for _, e := range snap.Effects {
			payload.Effects = append(payload.Effects, jsonEffect{Fiber: e.Fiber, Plugin: e.Plugin, Label: e.Label})
		}
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintln(errOut, "序列化失败:", err)
			return 1
		}
		fmt.Fprintln(out, string(b))
		return 0
	}

	fmt.Fprintln(out, "顺序:", strings.Join(up.Order, " → "))
	fmt.Fprintln(out, "fibers:")
	for _, f := range snap.Fibers {
		parent := f.Parent
		if parent == "" {
			parent = "-"
		}
		fmt.Fprintf(out, "  %-12s %-10s epoch=%-8d parent=%s\n", f.Plugin, f.State, f.Epoch, parent)
	}
	fmt.Fprintln(out, "services:")
	for _, s := range snap.Services {
		fmt.Fprintf(out, "  %-12s v%d provider=%s dependents=%d\n", s.Name, s.Version, s.Provider, len(s.Dependents))
	}
	fmt.Fprintln(out, "effects:")
	for _, e := range snap.Effects {
		fmt.Fprintf(out, "  %-12s %s\n", e.Plugin, e.Label)
	}
	fmt.Fprintln(out, "events:", strings.Join(snap.Events, ", "))
	if t.chain {
		fmt.Fprintln(out, "chains:")
		chains := inspectChains(app)
		for _, name := range []string{"fs", "shell", "workspace"} {
			p, ok := chains[name]
			if !ok {
				continue
			}
			fmt.Fprintf(out, "  %-6s priorities=%v\n", name, p)
		}
	}
	return 0
}

// inspectChains 读取 fs/shell/workspace 三个 chain 的装饰器优先级(外层到内层)。
//
// net 链已随 ADR 0012 移除:网络不再有受控 seam,Node 网络模块直连标准库。
func inspectChains(app *kernel.App) map[string][]int {
	out := map[string][]int{}
	if v, ok := app.Root().TryService("fs"); ok {
		if c, ok := v.(*seamfs.Chain); ok {
			out["fs"] = c.Applied()
		}
	}
	if v, ok := app.Root().TryService("shell"); ok {
		if c, ok := v.(*seamshell.Chain); ok {
			out["shell"] = c.Applied()
		}
	}
	if v, ok := app.Root().TryService(seamworkspace.ServiceName); ok {
		if c, ok := v.(*seamworkspace.Chain); ok {
			out["workspace"] = c.Applied()
		}
	}
	return out
}

type keyFileEntry struct {
	Public string `json:"public"`
	Trust  string `json:"trust"`
}

func loadKeyRing(path string) (map[string]install.Key, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]keyFileEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]install.Key, len(raw))
	for id, entry := range raw {
		pub, err := base64.StdEncoding.DecodeString(entry.Public)
		if err != nil {
			return nil, fmt.Errorf("密钥 %q 不是合法 base64: %w", id, err)
		}
		trust, err := install.TrustFromKey(entry.Trust)
		if err != nil {
			return nil, err
		}
		out[id] = install.Key{Public: ed25519.PublicKey(pub), Trust: trust}
	}
	return out, nil
}

func parseVerifyArgs(args []string) (dir, keys string, require bool, bad string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-keys":
			if i+1 >= len(args) {
				return "", "", false, "-keys 缺少文件参数"
			}
			keys = args[i+1]
			i++
		case a == "-require-signature":
			require = true
		case strings.HasPrefix(a, "-"):
			return "", "", false, "未知参数 " + a
		default:
			if dir != "" {
				return "", "", false, "多余的参数 " + a
			}
			dir = a
		}
	}
	if dir == "" {
		return "", "", false, "需要给出插件目录"
	}
	return dir, keys, require, ""
}

func cmdVerify(args []string, out, errOut io.Writer) int {
	dir, keysPath, require, bad := parseVerifyArgs(args)
	if bad != "" {
		fmt.Fprintln(errOut, bad)
		usage(errOut)
		return 2
	}
	keys, err := loadKeyRing(keysPath)
	if err != nil {
		fmt.Fprintln(errOut, "加载密钥失败:", err)
		return 1
	}
	info, err := install.VerifyDir(dir, install.Options{Keys: keys, RequireSignature: require})
	if err != nil {
		fmt.Fprintln(errOut, "校验失败:", err)
		return 1
	}
	capList := strings.Join(info.Manifest.Caps, ", ")
	if capList == "" {
		capList = "(无)"
	}
	fmt.Fprintf(out, "插件: %s@%s(%s)\n摘要: %s\n信任: %s\n已签名: %v\n能力: %s\n",
		info.ID, info.Version, info.Kind, info.Digest, info.Trust, info.Signed, capList)
	return 0
}

// parseInstallArgs 解析 install 参数;-dest 是 install 专属,不能走 verify 的参数解析。
func cmdUninstall(args []string, out, errOut io.Writer) int {
	id := ""
	dest := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-dest":
			if i+1 >= len(args) {
				fmt.Fprintln(errOut, "-dest 缺少目录参数")
				usage(errOut)
				return 2
			}
			dest = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			fmt.Fprintln(errOut, "未知参数 "+a)
			usage(errOut)
			return 2
		default:
			if id != "" {
				fmt.Fprintln(errOut, "多余的参数 "+a)
				usage(errOut)
				return 2
			}
			id = a
		}
	}
	if id == "" || dest == "" {
		fmt.Fprintln(errOut, "需要插件 id 与 -dest 安装根目录")
		usage(errOut)
		return 2
	}
	removed, err := install.Uninstall(dest, id)
	if err != nil {
		fmt.Fprintln(errOut, "卸载失败:", err)
		return 1
	}
	fmt.Fprintf(out, "已卸载: %s(删除 %d 个版本)\n", id, removed)
	return 0
}

// consent 是安装/升级共用的同意检查:声明能力的插件需要 -yes。
func consent(info *install.Info, yes bool, out, errOut io.Writer) bool {
	if len(info.Manifest.Caps) == 0 || yes {
		return true
	}
	fmt.Fprintf(out, "插件: %s@%s(信任 %s,已签名 %v)\n能力: %s\n",
		info.ID, info.Version, info.Trust, info.Signed, strings.Join(info.Manifest.Caps, ", "))
	fmt.Fprintln(errOut, "该插件声明了能力,需要 -yes 确认")
	return false
}

func cmdUpgrade(args []string, out, errOut io.Writer) int {
	dir, dest, keysPath, require, yes, keep, bad := parseInstallArgs(args)
	if bad != "" {
		fmt.Fprintln(errOut, bad)
		usage(errOut)
		return 2
	}
	keys, err := loadKeyRing(keysPath)
	if err != nil {
		fmt.Fprintln(errOut, "加载密钥失败:", err)
		return 1
	}
	preview, err := install.VerifyDir(dir, install.Options{Keys: keys, RequireSignature: require})
	if err != nil {
		fmt.Fprintln(errOut, "校验失败:", err)
		return 1
	}
	if !consent(preview, yes, out, errOut) {
		return 2
	}
	info, err := install.Upgrade(dir, dest, install.Options{Keys: keys, RequireSignature: require, KeepPrevious: keep})
	if err != nil {
		fmt.Fprintln(errOut, "升级失败:", err)
		return 1
	}
	fmt.Fprintf(out, "已升级: %s@%s → %s\n", info.ID, info.Version, info.Dir)
	return 0
}

func cmdCaps(args []string, out, errOut io.Writer) int {
	t := parseTarget(args)
	if t.bad != "" {
		fmt.Fprintln(errOut, t.bad)
		usage(errOut)
		return 2
	}
	manifests, err := loadTarget(t)
	if err != nil {
		fmt.Fprintln(errOut, "加载失败:", err)
		return 1
	}
	broker := caps.NewBroker(nil)
	grantErr := assemble.GrantManifests(broker, manifests)

	type capRow struct {
		ID      string   `json:"id"`
		Version string   `json:"version"`
		Trust   string   `json:"trust"`
		Caps    []string `json:"caps"`
	}
	rows := make([]capRow, 0, len(manifests))
	for _, m := range manifests {
		trust := m.Trust
		if trust == "" {
			trust = string(caps.TrustThirdParty)
		}
		rows = append(rows, capRow{ID: m.ID, Version: m.Version, Trust: trust, Caps: m.Caps})
	}

	if t.json {
		payload := struct {
			OK      bool     `json:"ok"`
			Error   string   `json:"error,omitempty"`
			Plugins []capRow `json:"plugins"`
		}{OK: grantErr == nil, Plugins: rows}
		if grantErr != nil {
			payload.Error = grantErr.Error()
		}
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintln(errOut, "序列化失败:", err)
			return 1
		}
		fmt.Fprintln(out, string(b))
		if grantErr != nil {
			return 1
		}
		return 0
	}

	for _, row := range rows {
		capsList := strings.Join(row.Caps, ", ")
		if capsList == "" {
			capsList = "(无)"
		}
		fmt.Fprintf(out, "  %s@%s trust=%s caps=%s\n", row.ID, row.Version, row.Trust, capsList)
	}
	if grantErr != nil {
		fmt.Fprintln(out, "结果: 不可授权")
		fmt.Fprintln(errOut, "授权失败:", grantErr)
		return 1
	}
	fmt.Fprintln(out, "结果: 可授权")
	return 0
}

// cmdRepo 在线读取仓库索引(可选验签),列出可用版本。
func cmdRepo(args []string, out, errOut io.Writer) int {
	url := ""
	keysPath := ""
	requireRepo := false
	asJSON := false
	bad := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-keys":
			if i+1 >= len(args) {
				bad = "-keys 缺少文件参数"
				break
			}
			keysPath = args[i+1]
			i++
		case a == "-require-repo-signature":
			requireRepo = true
		case a == "-json":
			asJSON = true
		case strings.HasPrefix(a, "-"):
			bad = "未知参数 " + a
		default:
			if url != "" {
				bad = "多余的参数 " + a
			} else {
				url = a
			}
		}
		if bad != "" {
			break
		}
	}
	if bad != "" {
		fmt.Fprintln(errOut, bad)
		usage(errOut)
		return 2
	}
	if url == "" {
		fmt.Fprintln(errOut, "需要仓库索引 URL")
		usage(errOut)
		return 2
	}
	keys, err := loadKeyRing(keysPath)
	if err != nil {
		fmt.Fprintln(errOut, "加载密钥失败:", err)
		return 1
	}
	entries, err := install.FetchRepoIndex(context.Background(), url, keys, requireRepo)
	if err != nil {
		fmt.Fprintln(errOut, "读取仓库失败:", err)
		return 1
	}
	if asJSON {
		b, err := json.MarshalIndent(map[string]any{"ok": true, "entries": entries}, "", "  ")
		if err != nil {
			fmt.Fprintln(errOut, "序列化失败:", err)
			return 1
		}
		fmt.Fprintln(out, string(b))
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "仓库: (空)")
		return 0
	}
	fmt.Fprintf(out, "仓库插件(%d):\n", len(entries))
	for _, e := range entries {
		fmt.Fprintf(out, "  %s@%s dir=%s\n", e.ID, e.Version, e.Dir)
	}
	return 0
}

func cmdUpdate(args []string, out, errOut io.Writer) int {
	id := ""
	repo := ""
	repoURL := ""
	dest := ""
	keysPath := ""
	require := false
	requireRepo := false
	yes := false
	bad := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-repo":
			if i+1 >= len(args) {
				bad = "-repo 缺少目录参数"
				break
			}
			repo = args[i+1]
			i++
		case a == "-repo-url":
			if i+1 >= len(args) {
				bad = "-repo-url 缺少 URL 参数"
				break
			}
			repoURL = args[i+1]
			i++
		case a == "-dest":
			if i+1 >= len(args) {
				bad = "-dest 缺少目录参数"
				break
			}
			dest = args[i+1]
			i++
		case a == "-keys":
			if i+1 >= len(args) {
				bad = "-keys 缺少文件参数"
				break
			}
			keysPath = args[i+1]
			i++
		case a == "-require-signature":
			require = true
		case a == "-require-repo-signature":
			requireRepo = true
		case a == "-yes":
			yes = true
		case strings.HasPrefix(a, "-"):
			bad = "未知参数 " + a
		default:
			if id != "" {
				bad = "多余的参数 " + a
			} else {
				id = a
			}
		}
		if bad != "" {
			break
		}
	}
	if bad != "" {
		fmt.Fprintln(errOut, bad)
		usage(errOut)
		return 2
	}
	if id == "" || (repo == "" && repoURL == "") || dest == "" || (repo != "" && repoURL != "") {
		fmt.Fprintln(errOut, "需要插件 id、且 -repo 与 -repo-url 二选一、-dest 安装根")
		usage(errOut)
		return 2
	}
	if repoURL != "" {
		return runRemoteUpdate(id, repoURL, dest, keysPath, require, requireRepo, yes, out, errOut)
	}
	keys, err := loadKeyRing(keysPath)
	if err != nil {
		fmt.Fprintln(errOut, "加载密钥失败:", err)
		return 1
	}
	opts := install.Options{Keys: keys, RequireSignature: require, RepoKeys: keys, RequireRepoSignature: requireRepo}
	preview, err := install.PreviewRepoUpdate(repo, id, opts)
	if err != nil {
		fmt.Fprintln(errOut, "校验仓库版本失败:", err)
		return 1
	}
	if !consent(preview, yes, out, errOut) {
		return 2
	}
	info, err := install.UpdateFromRepo(repo, id, dest, opts)
	if err != nil {
		fmt.Fprintln(errOut, "更新失败:", err)
		return 1
	}
	fmt.Fprintf(out, "已更新: %s@%s → %s\n", info.ID, info.Version, info.Dir)
	return 0
}

// runRemoteUpdate 从在线仓库更新:先预览/同意,再下载 artifact 安装。
func runRemoteUpdate(id, repoURL, dest, keysPath string, require, requireRepo, yes bool, out, errOut io.Writer) int {
	keys, err := loadKeyRing(keysPath)
	if err != nil {
		fmt.Fprintln(errOut, "加载密钥失败:", err)
		return 1
	}
	opts := install.Options{
		Keys: keys, RequireSignature: require,
		RepoKeys: keys, RequireRepoSignature: requireRepo,
	}
	preview, err := install.PreviewRemoteUpdate(context.Background(), repoURL, id, opts)
	if err != nil {
		fmt.Fprintln(errOut, "校验在线版本失败:", err)
		return 1
	}
	if !consent(preview, yes, out, errOut) {
		return 2
	}
	info, err := install.UpdateFromRemote(context.Background(), repoURL, id, dest, opts)
	if err != nil {
		fmt.Fprintln(errOut, "更新失败:", err)
		return 1
	}
	fmt.Fprintf(out, "已更新: %s@%s → %s\n", info.ID, info.Version, info.Dir)
	return 0
}

func cmdRollback(args []string, out, errOut io.Writer) int {
	id := ""
	dest := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-dest":
			if i+1 >= len(args) {
				fmt.Fprintln(errOut, "-dest 缺少目录参数")
				usage(errOut)
				return 2
			}
			dest = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			fmt.Fprintln(errOut, "未知参数 "+a)
			usage(errOut)
			return 2
		default:
			if id != "" {
				fmt.Fprintln(errOut, "多余的参数 "+a)
				usage(errOut)
				return 2
			}
			id = a
		}
	}
	if id == "" || dest == "" {
		fmt.Fprintln(errOut, "需要插件 id 与 -dest 安装根目录")
		usage(errOut)
		return 2
	}
	version, err := install.Rollback(dest, id)
	if err != nil {
		fmt.Fprintln(errOut, "回滚失败:", err)
		return 1
	}
	fmt.Fprintf(out, "已回滚: %s → %s\n", id, version)
	return 0
}

// cmdList 列出安装根目录的索引。
func cmdList(args []string, out, errOut io.Writer) int {
	dest := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-dest":
			if i+1 >= len(args) {
				fmt.Fprintln(errOut, "-dest 缺少目录参数")
				usage(errOut)
				return 2
			}
			dest = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			fmt.Fprintln(errOut, "未知参数 "+a)
			usage(errOut)
			return 2
		default:
			fmt.Fprintln(errOut, "多余的参数 "+a)
			usage(errOut)
			return 2
		}
	}
	if dest == "" {
		fmt.Fprintln(errOut, "缺少 -dest 安装根目录")
		usage(errOut)
		return 2
	}
	entries, err := install.List(dest)
	if err != nil {
		fmt.Fprintln(errOut, "读取索引失败:", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "已安装插件: (空)")
		return 0
	}
	fmt.Fprintf(out, "已安装插件(%d):\n", len(entries))
	for _, e := range entries {
		capsList := strings.Join(e.Caps, ", ")
		if capsList == "" {
			capsList = "(无)"
		}
		fmt.Fprintf(out, "  %s@%s trust=%s signed=%v caps=%s\n", e.ID, e.Version, e.Trust, e.Signed, capsList)
	}
	return 0
}

func parseInstallArgs(args []string) (dir, dest, keys string, require, yes, keep bool, bad string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-dest":
			if i+1 >= len(args) {
				return "", "", "", false, false, false, "-dest 缺少目录参数"
			}
			dest = args[i+1]
			i++
		case a == "-keys":
			if i+1 >= len(args) {
				return "", "", "", false, false, false, "-keys 缺少文件参数"
			}
			keys = args[i+1]
			i++
		case a == "-require-signature":
			require = true
		case a == "-yes":
			yes = true
		case a == "-keep":
			keep = true
		case strings.HasPrefix(a, "-"):
			return "", "", "", false, false, false, "未知参数 " + a
		default:
			if dir != "" {
				return "", "", "", false, false, false, "多余的参数 " + a
			}
			dir = a
		}
	}
	if dir == "" {
		return "", "", "", false, false, false, "需要给出插件目录"
	}
	if dest == "" {
		return "", "", "", false, false, false, "缺少 -dest 安装根目录"
	}
	return dir, dest, keys, require, yes, keep, ""
}

func cmdInstall(args []string, out, errOut io.Writer) int {
	dir, dest, keysPath, require, yes, keep, bad := parseInstallArgs(args)
	if bad != "" {
		fmt.Fprintln(errOut, bad)
		usage(errOut)
		return 2
	}
	keys, err := loadKeyRing(keysPath)
	if err != nil {
		fmt.Fprintln(errOut, "加载密钥失败:", err)
		return 1
	}
	preview, err := install.VerifyDir(dir, install.Options{Keys: keys, RequireSignature: require})
	if err != nil {
		fmt.Fprintln(errOut, "校验失败:", err)
		return 1
	}
	if keep {
		fmt.Fprintln(errOut, "-keep 只用于 upgrade")
		return 2
	}
	if !consent(preview, yes, out, errOut) {
		return 2
	}
	info, err := install.Install(dir, dest, install.Options{Keys: keys, RequireSignature: require})
	if err != nil {
		fmt.Fprintln(errOut, "安装失败:", err)
		return 1
	}
	fmt.Fprintf(out, "已安装: %s@%s → %s(信任 %s,已签名 %v)\n", info.ID, info.Version, info.Dir, info.Trust, info.Signed)
	return 0
}
