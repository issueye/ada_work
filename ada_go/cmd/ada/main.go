// 命令 ada 是装配工具入口:实现位于 host/cli(平台映射与输出)。
package main

import (
	"fmt"
	"os"

	"github.com/issueye/ada_go_v2/platform/denv"
	"github.com/issueye/ada_go_v2/platform/host/cli"
)

func main() {
	// .env 放本机开发/试用要用的环境变量(API key、endpoint 等),不必写进 preset 或 shell。
	// 真实环境变量优先;文件解析失败直接退出——拼错的键被静默忽略比启动失败更难排查。
	// ADA_ENV_FILE 指定别的文件时只读它(CI 或测试用临时环境)。
	os.Exit(run())
}

func run() int {
	paths := []string{".env"}
	if p := os.Getenv("ADA_ENV_FILE"); p != "" {
		paths = []string{p}
	}
	n, err := denv.Apply(paths...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ada:", err)
		return 2
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "ada: 已从环境变量文件加载 %d 项\n", n)
	}
	return cli.Run(os.Args[1:], os.Stdout, os.Stderr)
}
