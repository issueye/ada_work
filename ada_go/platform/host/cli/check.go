package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"

	"github.com/issueye/ada_go_v2/platform/assemble"
	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/host/registry"
	"github.com/issueye/ada_go_v2/platform/host/server"
	"github.com/issueye/ada_go_v2/platform/install"
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

// compileDir 逐个插件目录跑 VerifyDir(清单/摘要/TS 编译)。
func compileDir(dir string) (int, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, "读取目录失败: " + err.Error(), err
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "plugin.json")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if _, err := install.VerifyDir(filepath.Join(dir, e.Name()), install.Options{}); err != nil {
			return count, fmt.Sprintf("插件 %s 编译/校验失败: %v", e.Name(), err), err
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

func cmdServeRepo(args []string, out, errOut io.Writer) int {
	root := ""
	addr := "127.0.0.1:8080"
	token := ""
	certFile := ""
	keyFile := ""
	rateLimit := 0
	bad := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-root":
			if i+1 >= len(args) {
				bad = "-root 缺少目录参数"
				break
			}
			root = args[i+1]
			i++
		case a == "-addr":
			if i+1 >= len(args) {
				bad = "-addr 缺少地址参数"
				break
			}
			addr = args[i+1]
			i++
		case a == "-token":
			if i+1 >= len(args) {
				bad = "-token 缺少值"
				break
			}
			token = args[i+1]
			i++
		case a == "-rate-limit":
			if i+1 >= len(args) {
				bad = "-rate-limit 缺少数值"
				break
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				bad = "-rate-limit 必须是整数"
			} else {
				rateLimit = n
			}
			i++
		case a == "-cert":
			if i+1 >= len(args) {
				bad = "-cert 缺少文件"
				break
			}
			certFile = args[i+1]
			i++
		case a == "-key":
			if i+1 >= len(args) {
				bad = "-key 缺少文件"
				break
			}
			keyFile = args[i+1]
			i++
		default:
			bad = "未知参数 " + a
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
	if root == "" {
		fmt.Fprintln(errOut, "需要 -root 仓库目录")
		usage(errOut)
		return 2
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(errOut, "监听失败:", err)
		return 1
	}
	fmt.Fprintf(out, "仓库服务: http://%s\n", ln.Addr().String())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := registry.Serve(ctx, registry.Options{
		Root: root, Token: token, RateLimit: rateLimit,
		CertFile: certFile, KeyFile: keyFile,
	}, ln); err != nil {
		fmt.Fprintln(errOut, "服务失败:", err)
		return 1
	}
	fmt.Fprintln(out, "已停止。")
	return 0
}

func cmdServe(args []string, out, errOut io.Writer) int {
	root := ""
	addr := "127.0.0.1:8080"
	token := ""
	rateLimit := 0
	certFile := ""
	keyFile := ""
	bad := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-root":
			if i+1 >= len(args) {
				bad = "-root 缺少目录参数"
				break
			}
			root = args[i+1]
			i++
		case a == "-addr":
			if i+1 >= len(args) {
				bad = "-addr 缺少地址参数"
				break
			}
			addr = args[i+1]
			i++
		case a == "-token":
			if i+1 >= len(args) {
				bad = "-token 缺少值"
				break
			}
			token = args[i+1]
			i++
		case a == "-rate-limit":
			if i+1 >= len(args) {
				bad = "-rate-limit 缺少数值"
				break
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				bad = "-rate-limit 必须是整数"
			} else {
				rateLimit = n
			}
			i++
		case a == "-cert":
			if i+1 >= len(args) {
				bad = "-cert 缺少文件"
				break
			}
			certFile = args[i+1]
			i++
		case a == "-key":
			if i+1 >= len(args) {
				bad = "-key 缺少文件"
				break
			}
			keyFile = args[i+1]
			i++
		case a == "-rate-limit":
			if i+1 >= len(args) {
				bad = "-rate-limit 缺少数值"
				break
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				bad = "-rate-limit 必须是整数"
			} else {
				rateLimit = n
			}
			i++
		default:
			bad = "未知参数 " + a
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
	if root == "" {
		fmt.Fprintln(errOut, "需要 -root 根目录")
		usage(errOut)
		return 2
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(errOut, "监听失败:", err)
		return 1
	}
	fmt.Fprintf(out, "HTTP 服务: http://%s\n", ln.Addr().String())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := server.Serve(ctx, server.Options{
		Root: root, Token: token, RateLimit: rateLimit,
		CertFile: certFile, KeyFile: keyFile,
	}, ln); err != nil {
		fmt.Fprintln(errOut, "服务失败:", err)
		return 1
	}
	fmt.Fprintln(out, "已停止。")
	return 0
}
