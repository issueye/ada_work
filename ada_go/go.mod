module github.com/issueye/ada_go_v2

go 1.27

require (
	github.com/MoeForge/moejs v0.1.0-alpha.11
	github.com/issueye/ada_jsrt v0.0.0
	golang.org/x/sys v0.0.0-20220715151400-c0bba94af5f8
)

require github.com/evanw/esbuild v0.28.2 // indirect

// 执行层(compile/jsrt)已抽到同级目录的独立库 ada_jsrt。
// 本地开发用 replace;ada_jsrt 发布后改成真实版本号并删除这一行,CI 才能只 checkout 本仓库。
replace github.com/issueye/ada_jsrt => ../ada_jsrt
