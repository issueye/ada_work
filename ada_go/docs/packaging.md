# 编译与发布打包

> 脚本:[`scripts/package.ps1`](../scripts/package.ps1)、[`scripts/build.ps1`](../scripts/build.ps1)、
> 内置清单导出工具:[`scripts/tools/builtins-dump`](../scripts/tools/builtins-dump/main.go)。
> 目标:发布包里的 `plugins/` 与二进制内置插件**同源**,preset 不引用源码开发目录。

## 1. 为什么需要单独的打包脚本

仓库里的内置插件只有一处:源码目录 **`plugins/<分类>/<目录>`**。
Go 插件的 `plugin.json` 由 `builtins-dump -source plugins` 从 `builtins.Plugins()` 生成
(`catalog.TestCatalogManifestsMatchRegistry` 守着,清单与注册表不可能靠手抄漂移);
TS 插件的 `plugin.ts` 与 `plugin.json` 本来就在同一目录。
发布包里的清单则从编译进宿主的注册表**重新导出**,不复制仓库里任何手写清单。

`package.ps1` 的做法:

1. 编译全部入口到 `dist/<name>/bin/`;
2. 运行 `builtins-dump -out plugins`,**从编译进宿主的 `builtins.Plugins()` 枚举所有 Go 插件**,
   用 `kernel.Plugin` 的 `Provide()`/`Inject()` 自动生成 `plugins/<id>/plugin.json`;
3. 从同一个 `plugins/` 目录按清单自动发现 `kind=ts` 的插件,复制源码与清单
   (工具类 `text`/`shell`/`memory`/`http`/`plugin`/`skill`/`git`/`task`/`command-helper`
   都在 `plugins/tool/`,新增 TS 插件不需要改打包脚本);
4. 生成只引用包内 `../plugins`、`workspace/`、`data/` 的 preset;示例/工具插件不在 allowlist 里,随包但默认禁用;
5. 用打包后的二进制执行 `ada plan` 与 `ada check plugins` 自检(同平台时);
6. 打 zip。

因此发布包与仓库只有一份插件来源:仓库里改插件目录,`-source` 重生成清单即可;
发布包启用 TS 插件时由包内 esbuild 现场编译,不需要开发目录。

## 2. 用法

```powershell
pwsh scripts/package.ps1                                  # 当前平台,版本 dev-<commit>
pwsh scripts/package.ps1 -Version 0.1.0                   # 指定版本
pwsh scripts/package.ps1 -Version 0.1.0 -GOOS linux -GOARCH amd64
pwsh scripts/package.ps1 -SkipBuild -NoZip                 # 复用已有 bin,只出目录
pwsh scripts/package.ps1 -StripDebug                       # 发布 strip
pwsh scripts/package.ps1 -LLMEndpoint http://127.0.0.1:3050/v1 -LLMModel tokenharbor/deepseek-v4.1-flash:free
```

`-LLMEndpoint` + `-LLMModel` 同时给出时,额外生成
`presets/agent-real-release.preset.json`;否则只生成回放 preset。

## 3. 包结构

```text
dist/ada-<version>-<goos>-<goarch>/
  bin/
    ada[.exe]
    ada-rpc-agent[.exe]
    ada-demo[.exe]
    ada-ts-demo[.exe]
  plugins/
    <id>/plugin.json            # 由 builtins 自动导出的全部 Go 内置插件
    text-helper/                # TS 实现:plugin.json + plugin.ts
    shell-helper/               # TS 实现:plugin.json + plugin.ts
    memory-helper/              # TS 实现:长期记忆(默认启用)
    git-helper/                 # 示例脚本插件(examples/git-ts),随包但默认禁用
    task-helper/                # 示例脚本插件(examples/task-ts)
    command-helper/             # 示例脚本插件(examples/command-ts)
  presets/
    agent-release.preset.json
    workspace-release.preset.json
    acp-release.preset.json
    agent-real-release.preset.json   # 仅当给了 LLMEndpoint/LLMModel
  data/
    agent-replay.jsonl
    workspace-replay.jsonl
  workspace/
    a.txt
  docs/
    a.txt                       # 只读示例工作区
  package.json                  # version/commit/goos/goarch/plugins/binaries
  .ada-base                     # 装配基目录锚点:preset 里 data/workspace 相对路径的根
  README.txt
```

在包根目录运行:

```powershell
bin/ada.exe plan  --preset presets/agent-release.preset.json
bin/ada.exe agent --preset presets/agent-release.preset.json -message "读取 a.txt 并总结"
bin/ada.exe agent --preset presets/workspace-release.preset.json --workspace docs -message "查询工作区并读取 a.txt"
```

`acp-release` 在 `agent-release` 基础上额外启用 `agent-control` / `seam-acp` / `seam-approval` / `acp-helper`,
用于 `ada acp --preset presets/acp-release.preset.json`(ACP 模式;`ada acp` 需要 `agent-control` 提供 control 服务)。

## 4. preset 的 allowlist

`plugins/` 是唯一的插件目录,共 **52 个 Go 内置插件 + 9 个内置 TS 插件**
(`text`/`shell`/`memory`/`http`/`plugin`/`skill`/`git`/`task`/`command-helper`);发布
preset 只启用栈内插件,其余全部写入 `disable`(不在 allowlist 里的插件):

- `agent-release`: agent-loop、plan、session-memory/stats、system-prompt、seam-llm + llm-replay、
  seam-fs + fs-local、seam-tools、compaction-basic、text/memory/task/tool-helper(TS/Go),
  以及 shell 栈(seam-shell + shell-local + shell-guard + seam-sandbox + sandbox-none +
  shell-helper、git-helper、command-helper);`system-prompt.tools` 会附上记忆、计划
  (`plan_set`/`plan_update`/`plan_show`)与 `tool_find`/`tool_exec` 的使用规则;
- `workspace-release`: 再增加 seam/workspace + workspace-local + workspace-guard + workspace-helper,
  不启用 shell 栈;
- `agent-real-release`(可选): 用 `llm-openai` 替换 `llm-replay`,同样带 shell 栈。

未启用的插件仍以清单形式随包发布,方便后续在 preset 中启用;启用时必须补上它自己的配置。
保持默认关闭的只有四类:**互斥实现**(`llm-openai` 对 `llm-replay`、`session-jsonl` 对
`session-memory`)、**审批人**(`approval-cli`/`approval-deny`)、**宿主传输**(`rpc-*` 与
`seam-rpc`,由 `ada-rpc-agent` 自己装配)、**守卫/策略**(`fs-sandbox`、`caps-gate`、
`approval-gate`、`audit-log`、`*-redact`/`*-observe`、`llm-retry`、`tool-timeout`、
`secrets-guard`、`workspace-guard` 等——它们会改变默认行为,是否开启属于产品口径)。

## 5. 自检

同平台打包时,脚本会进入包目录执行:

```powershell
bin/ada plan  --preset presets/agent-release.preset.json
bin/ada plan  --preset presets/workspace-release.preset.json
bin/ada plan  --preset presets/acp-release.preset.json
bin/ada check plugins
```

前两条校验发布 preset 不依赖开发目录;`check plugins` 扫描整个插件目录并现场编译
随包 TS 插件(含三个示例脚本插件),能发现漏拷源码、清单/entry 写错一类的问题。
任一失败即中止打包。交叉编译到其它 GOOS/GOARCH 时跳过运行自检,但仍校验二进制与清单存在。

## 6. 边界

1. **Go 清单自动导出,TS 插件按清单复制**:`builtins-dump` 只导出 Go 插件的清单;仓库 `plugins/`
   下 `kind=ts` 的插件(工具类 `text`/`shell`/`memory`/`http`/`plugin`/`skill`/`git`/`task`/`command-helper`)
   由脚本按清单发现并复制源码与清单。它们默认「随包但不启用」,启用时要补自己的配置
   (如 `shell-helper` 的 `program`、`task-helper` 依赖的 `fs` 栈);其它来源的 TS 插件仍需在
   发布项目里提供源码/Artifact 与清单。
2. **preset 的资源路径按装配基目录解析**:`llm-replay`、`fs-local.workspace` 等配置是相对装配基目录的,
   包根由脚本写入的 `.ada-base` 锚点标记。`ada` CLI(含 `ada acp`)与 `ada-rpc-agent` 都从 preset
   目录向上找 `go.mod` 或 `.ada-base` 并切到该目录再装配——所以宿主可以把 agent 进程启动在用户的
   工作区目录里,而 preset 仍写 `data/`、`workspace/` 这类包根相对路径。
3. **没有真实端点自动探测**:真实 preset 的 endpoint/model 只来自参数,不在脚本里猜。
4. **zip 只是本地产物**:未做签名/公证/安装器;发布签名仍走 `install` 的密钥体系。
5. **交叉编译不能在本机执行自检**:这是有意跳过,不代表包已验证目标平台的运行时行为。
