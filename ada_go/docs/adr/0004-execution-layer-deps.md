# ADR 0004:执行层引入 esbuild 与 moejs

- 状态:已接受
- 日期:2026-11-19
- 复议日期:2027-01-19(moejs 仍是 alpha;每次升级前复议)
- 触发条款:AGENTS.md §12.5(引入新的第三方依赖)

## 背景

M1 只交付了内核。M2 要让 **TS 插件在进程内跑起来,且不需要 Node**,这要求两件能力:

1. **把 TS 降级成 moejs 能吃的 JS**。moejs 的 `CompileTS` 只做类型擦除,遇到有运行时语义的语法(`enum`、带值 `namespace`、构造器参数属性、装饰器、`import x = require`、TSX)直接 `SyntaxError`。
2. **在进程内执行 JS**。引入 Node 会推翻「单二进制、无 Node」这条硬约束(C7/C8 与 AGENTS §1)。

设计文档 §10–§11 已经选定 esbuild(Go API)与 moejs,并做了 5 个 spike(S1/S2/S6/S8/S9)。本 ADR 只决定「是否把它们作为真实依赖引入仓库」。

## 决策

- 引入 **`github.com/evanw/esbuild v0.28.2`**:只使用 `api.Transform`(逐文件、内存态、纯 Go),不使用 `Build`、不解析路径、不落盘。
- 引入 **`github.com/MoeForge/moejs v0.1.0-alpha.11`**:在 `jsrt` 里单点适配,对外只暴露 JSON 边界。
- 两个依赖都**只能被 `compile/`、`jsrt/`、`bridge/` 与宿主引用**;`kernel/` 的依赖禁令不变(AGENTS §5.1),由 `scripts/guard.ps1` 的 `go list -deps ./kernel` 强制。
- 版本**钉死**;升级必须跑执行层全量测试并复议本 ADR(moejs 仍是 alpha)。

## 替代方案

1. **纯手写 TypeScript 擦除**:enum/装饰器/参数属性都需要真正的降级,自己写等于重写 esbuild 的一部分,且难以与 TS 语义保持一致。否决。
2. **外部 Node 进程做编译/执行**:破坏「无 Node、单二进制」的硬约束(C8),并且跨进程 JSON 边界会让 P8「数据面留在 Go」失效。否决。
3. **只用 moejs 的 `CompileTS`**:S1 已证明会在 `enum` + 参数属性上失败,而这正是 TS 插件最常见的写法。否决。
4. **只用 esbuild 的 `Build`(含打包)**:`Build` 需要文件系统与路径解析,与我们「模块解析归宿主、编译按文件内存态」的分离相悖(design §10.1)。部分采用:仅 `Transform`。

## 影响面

- 二进制体积:esbuild ≈ +7 MB(strip)、moejs ≈ +12 MB(strip),两者同进程 ≈ 16–22 MB(design §15.1 实测)。发布物可按 build tag 裁剪 esbuild(design §15.3),M2 先不做裁剪。
- 新增 `compile/`、`jsrt/`、`bridge/` 三个 L2 包,`go.mod` 出现首批第三方依赖。
- 供应链:两个依赖都需在 `go.sum` 中固定;CI 使用 `-mod=readonly` 语义(默认行为即可)。
- 许可证:esbuild(MIT)、moejs 的 LICENSE 需随发布物附带(design §13,留到 M5 打包时处理)。

## 回滚方式

删掉 `compile/`、`jsrt/`、`bridge/` 三个包与 `go.mod` 中两行 require,`go mod tidy` 即可完全回滚;内核与门禁不依赖它们,回滚不影响 M1 的任何结论。已编译的 Artifact 是纯 JSON/JS,可保留。

## 复议日期

2027-01-19。若 moejs 发布稳定版或改为可替换接口,重新评估是否仍钉 alpha.11。
