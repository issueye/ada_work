package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"

	"github.com/issueye/ada_go_v2/platform/assemble"
	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/host/registry"
	"github.com/issueye/ada_go_v2/platform/host/server"
	"github.com/issueye/ada_go_v2/platform/install"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

// checkResult 是 `ada check` 的结构化结果。
type checkResult struct {
	OK         bool     `json:"ok"`
	Plugins    int      `json:"plugins"`
	PlanOK     bool     `json:"planOk"`
	PlanErrs   []string `json:"planErrors,omitempty"`
	CapsOK     bool     `json:"capsOk"`
	CapsErr    string   `json:"capsError,omitempty"`
	CompileOK  bool     `json:"compileOk"`
	Compiled   []string `json:"compiled,omitempty"`
	CompileErr string   `json:"compileError,omitempty"`
	Problems   []string `json:"problems,omitempty"`
}

// compileDir 逐个插件跑 VerifyDir(清单/摘要/TS 编译)。
//
// 目录发现与 preset 走同一条规则(单插件目录 / 插件集合 / 一层分类目录),
// 所以 `ada check plugins` 能一键校验整个内置插件目录。
func compileDir(dir string) (int, string, error) {
	manifests, err := plugin.LoadDir(dir)
	if err != nil {
		return 0, "读取目录失败: " + err.Error(), err
	}
	count := 0
	for _, m := range manifests {
		if _, err := install.VerifyDir(m.Dir, install.Options{}); err != nil {
			return count, fmt.Sprintf("插件 %s 编译/校验失败: %v", m.ID, err), err
		}
		count++
	}
	return count, "", nil
}

func cmdCheck(args []string, out, errOut io.Writer) int {
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
	res := checkResult{Plugins: len(manifests), PlanOK: true, CapsOK: true, CompileOK: true}
	if t.dir == "" {
		// preset 目标:目录未知,跳过逐插件编译。
		res.Compiled = nil
	} else {
		count, cErr, _ := compileDir(t.dir)
		res.Compiled = make([]string, 0)
		if cErr != "" {
			res.CompileOK = false
			res.CompileErr = cErr
			res.Problems = append(res.Problems, cErr)
		}
		_ = count
	}

	report := assemble.Plan(manifests)
	if !report.OK() {
		res.PlanOK = false
		for _, p := range report.Problems {
			res.PlanErrs = append(res.PlanErrs, p.String())
		}
	}
	broker := caps.NewBroker(nil)
	if err := assemble.GrantManifests(broker, manifests); err != nil {
		res.CapsOK = false
		res.CapsErr = err.Error()
		res.Problems = append(res.Problems, "能力: "+err.Error())
	}
	res.OK = res.PlanOK && res.CapsOK && res.CompileOK

	if t.json {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			fmt.Fprintln(errOut, "序列化失败:", err)
			return 1
		}
		fmt.Fprintln(out, string(b))
		if !res.OK {
			return 1
		}
		return 0
	}

	fmt.Fprintf(out, "清单: OK(%d 个插件)\n", res.Plugins)
	if res.PlanOK {
		fmt.Fprintf(out, "计划: OK(顺序 %v)\n", report.Order)
	} else {
		fmt.Fprintf(out, "计划: 不可执行\n")
		for _, p := range res.PlanErrs {
			fmt.Fprintln(out, "  -", p)
		}
	}
	if res.CapsOK {
		fmt.Fprintln(out, "能力: OK")
	} else {
		fmt.Fprintln(out, "能力: 失败 -", res.CapsErr)
	}
	if res.CompileOK {
		if t.dir == "" {
			fmt.Fprintln(out, "编译: 跳过(preset 目标)")
		} else {
			fmt.Fprintln(out, "编译: OK")
		}
	} else {
		fmt.Fprintln(out, "编译: 失败 -", res.CompileErr)
	}
	if res.OK {
		fmt.Fprintln(out, "结果: 通过")
		return 0
	}
	fmt.Fprintln(out, "结果: 失败")
	return 1
}

// serveFlags 是 serve / serve-repo 共用的命令行参数。
type serveFlags struct {
	root      string
	addr      string
	token     string
	certFile  string
	keyFile   string
	rateLimit int
}

// parseServeFlags 解析共用参数;地址缺省是 127.0.0.1:8080。
//
// 两个 serve 子命令的开关集完全相同(仓库服务多一个可选 -rate-limit 的默认值),
// 因此共用一份解析:分成两份曾让 cmdServe 里出现重复分支。
func parseServeFlags(args []string) (serveFlags, string) {
	f := serveFlags{addr: "127.0.0.1:8080"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if i+1 >= len(args) {
			return f, a + " 缺少参数"
		}
		switch a {
		case "-root":
			f.root = args[i+1]
		case "-addr":
			f.addr = args[i+1]
		case "-token":
			f.token = args[i+1]
		case "-cert":
			f.certFile = args[i+1]
		case "-key":
			f.keyFile = args[i+1]
		case "-rate-limit":
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return f, "-rate-limit 必须是整数"
			}
			f.rateLimit = n
		default:
			return f, "未知参数 " + a
		}
		i++
	}
	if f.root == "" {
		return f, "需要 -root 根目录"
	}
	return f, ""
}

// serveCommand 是 serve / serve-repo 的共同实现:解析参数、监听、交给适配器。
//
// 两个适配器的 Options 是各自包里的类型(字段同形但不通用),所以由调用方把
// flags 折成自己的 Options;这里只共享参数、监听与输出。
func serveCommand(args []string, out, errOut io.Writer, label string,
	serve func(context.Context, serveFlags, net.Listener) error) int {
	f, bad := parseServeFlags(args)
	if bad != "" {
		fmt.Fprintln(errOut, bad)
		usage(errOut)
		return 2
	}
	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		fmt.Fprintln(errOut, "监听失败:", err)
		return 1
	}
	fmt.Fprintf(out, "%s: http://%s\n", label, ln.Addr().String())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := serve(ctx, f, ln); err != nil {
		fmt.Fprintln(errOut, "服务失败:", err)
		return 1
	}
	fmt.Fprintln(out, "已停止。")
	return 0
}

func cmdServeRepo(args []string, out, errOut io.Writer) int {
	return serveCommand(args, out, errOut, "仓库服务",
		func(ctx context.Context, f serveFlags, ln net.Listener) error {
			return registry.Serve(ctx, registry.Options{
				Root: f.root, Token: f.token, RateLimit: f.rateLimit,
				CertFile: f.certFile, KeyFile: f.keyFile,
			}, ln)
		})
}

func cmdServe(args []string, out, errOut io.Writer) int {
	return serveCommand(args, out, errOut, "HTTP 服务",
		func(ctx context.Context, f serveFlags, ln net.Listener) error {
			return server.Serve(ctx, server.Options{
				Root: f.root, Token: f.token, RateLimit: f.rateLimit,
				CertFile: f.certFile, KeyFile: f.keyFile,
			}, ln)
		})
}
