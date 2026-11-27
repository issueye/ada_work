# M3 装配层

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §9.2(清单 v2)、§12.1(装配编排)与 §12.4(组合)。
> 本页描述**已实现**的部分与**已知边界**;install / 签名 / capability / profile 都不在这一刀里。

## 1. 范围

M3 要回答的是「装哪些插件、按什么顺序、参数怎么覆盖、装不上怎么办」。当前交付:

```
preset.json ──preset.Load──▶ 多目录 + disabled + config patch
                                   │
plugin.json ──plugin.LoadDir──▶ []*Manifest(校验过的清单)
                                   │
                                   ▼
                         assemble.Plan ──▶ Report(顺序/问题/警告)
                                   │
                                   ▼
              loader.Lookup ◀── kind ──▶ go: 宿主注册表 / ts: 编译或 Artifact
                                   │
                                   ▼
                         assemble.Up ──▶ 挂载 + 失败整体回滚
                                   │
                                   ▼
                    ada plan / ada up / ada inspect ──▶ 计划 / 挂载 / 诊断快照
```

**不在这一刀里**:插件安装与更新、签名校验、capability 授权(这些在 M5 文档);
bundle/profile 已在 `platform/bundle` 落地(见 [m4-composition.md](m4-composition.md))。

## 2. 清单(`platform/plugin/`)

字段是 design §9.2 的子集:

| 字段 | 必填 | 语义 |
|---|---|---|
| `id` | ✅ | kebab-case,全局唯一 |
| `version` | ✅ | semver(`0.1.0`、`1.2.3-beta.1`) |
| `kind` | ✅ | `go` 或 `ts` |
| `entry` / `artifact` | ts 必填其一 | 源码入口 / 编译产物路径 |
| `inject.required` | | 必需依赖;无人提供 → 计划失败 |
| `inject.optional` | | 可选依赖;无人提供 → 只告警 |
| `provide` | | 本插件提供的服务名 |
| `config` | | 传给插件的配置对象 |
| `disabled` | | 为 true 时计划排除并告警 |

校验规则(全部有测试):

- **未知字段直接报错**(`DisallowUnknownFields`):清单是契约,拼错的字段静默忽略会让作者以为它生效了;
- `id` 必须 kebab-case,`version` 必须 semver,`kind` 只能是 `go`/`ts`;
- `provide`/`inject.*` 不允许空名与重复名;
- 同一服务不能既 `provide` 又 `require`(自依赖,装配期必然成环);
- 同一服务不能同时出现在 required 与 optional 里。

错误统一为 `*plugin.Error`,带 `EPLUGINIO` / `EPLUGINMANIFEST` 码。

## 3. 计划(`assemble.Plan`)

纯函数:同一批清单永远得到同一个顺序。

规则按优先级:

1. **重复 id** → 阻断;
2. **同一服务多个提供者** → 阻断(否则「谁生效取决于注册顺序」);
3. **必需依赖无人提供** → 阻断,该插件不进入顺序;
4. **级联**:提供者自己装不起来 → 依赖它的消费者也阻断(否则会永久 pending);
5. **可选依赖无人提供** → 只告警;
6. **成环** → 阻断,并点名环路径(`a → b → a`);
7. 拓扑排序,同级按 id 排序 → 确定性顺序。

```go
type Report struct {
    Order    []string  // 拓扑顺序(有问题时只含已确定的部分)
    Problems []Problem // kind / plugin / service / detail
    Warnings []string
}
func (r *Report) OK() bool
func (r *Report) Err() error // *assemble.Error,码 EASSEMBLEPLAN
```

## 4. 组合(`platform/preset/`)

preset 是**数据不是代码**(AGENTS §2.2 的组合数据类):产品差异靠换 preset 表达,不允许 `if product == ...`。

```json
{
  "name": "base",
  "plugins": ["../../plugins"],
  "enable": ["agent-loop", "fs-local", "llm-replay"],
  "disable": ["llm-openai"],
  "patch": {"agent-loop": {"model": "replay"}}
}
```

| 字段 | 语义 |
|---|---|
| `name` | kebab-case,用于报告 |
| `plugins` | 插件目录列表;相对路径按 **preset 文件所在目录**解析。目录可以是单插件目录、插件目录集合,或**一层分类目录**(`plugins/` 下的 `seam`/`provider`/...) |
| `enable` | **候选白名单**:只保留名单里的插件,其余不进入装配;不写则不收口(目录里的全部插件都是候选) |
| `disable` | 在候选集合里置为 disabled(计划告警并排除);允许与 `enable` 重叠,表示「收下但默认不启用」 |
| `patch` | 每个插件的 config **深合并**覆盖;对象递归、其余值直接替换 |

约束与语义:

- patch **只能覆盖 `config`**:id/kind/inject/provide 属于插件身份与依赖图,不是参数,不能被 preset 改;
- `disable`/`patch`/`enable` 指向不存在的插件 → **报错**(静默忽略会让作者以为配置生效了);
- 同一 id 出现在多个目录 → 展开时报错;
- 深合并的理由:patch 只写「与默认不同的那个键」,强制整体替换会逼作者复制全部默认值,默认值一改就漂移;
- **为什么要 `enable`**:内置插件已经统一到 `plugins/` 一个目录里(preset 直接写 `../../plugins`),
  `enable` 把「这个产品要哪几个」表达成数据;`enable` 与 `disable` 重叠时,`disable` 的那几个
  仍在清单里但默认关着——宿主运行期可以打开备选实现(例如 `ada-rpc-agent` 切换 LLM 供应商)。

## 5. 装载(`loader.Lookup`)

按 `kind` 选路,把清单变成可挂载的 `kernel.Plugin`:

| kind | 来源 | 说明 |
|---|---|---|
| `go` | 宿主注册表 `map[id]func() kernel.Plugin` | Go 插件必须编译进宿主;CLI 里没有注册表,会得到明确错误 |
| `ts` + `artifact` | `compile.ParseArtifact` | 校验摘要后交给 `bridge.Plugin` |
| `ts` + `entry` | `loader.CollectSources` + `compile.Build` | 现场扫描目录(跳过 `node_modules` 与隐藏目录)并编译 |

装载层不做依赖排序(那是 `assemble.Plan`),也不做事务与回滚(那是 `assemble.Up`);它返回的函数类型与 `assemble.Lookup` 一致,所以装配层不需要知道 `compile`/`bridge` 的存在。

## 6. 装配(`assemble.Up`)

```go
up, err := assemble.Up(app, manifests, plan, lookup)
```

- 按计划顺序挂载,每个插件等待 `Ready`(上限 10s);
- **事务语义**:任一插件挂载/就绪失败 → 已挂载的插件**按逆序卸载**(与内核 effect 栈一致);
- 失败返回 `*assemble.UpError`:插件名、原因、回滚数量与回滚错误(码 `EASSEMBLEUP`);
- 计划本身不通过时直接拒绝执行(不会「装一半再报错」)。

## 7. CLI: `ada plan` / `ada up` / `ada inspect`
## 7. CLI: `ada plan` / `ada up`

```powershell
go run ./cmd/ada plan examples/plugins                 # 纯目录:顺序 + 可选依赖告警
go run ./cmd/ada plan --preset examples/presets/base.preset.json
go run ./cmd/ada plan --preset examples/presets/base.preset.json -json
go run ./cmd/ada up      --preset examples/git-ts/git.preset.json
go run ./cmd/ada inspect --preset examples/git-ts/git.preset.json      # 挂载并输出诊断快照
go run ./cmd/ada inspect --preset examples/git-ts/git.preset.json -json
```

`--preset` 的实际输出:

```
清单(3): greeter@0.1.0(go), presenter@0.1.0(go), upper@0.1.0(go)
顺序: greeter → upper
警告: 插件 presenter 已被 disabled,计划中排除
结果: 可装配
```

`ada up --preset examples/git-ts/git.preset.json` 会真的挂载、打印 fiber/服务,再停止触发清理。

退出码:`0` 成功 / `1` 不可装配或装配失败 / `2` 用法错误。

## 8. 测试矩阵

| 场景 | 用例 |
|---|---|
| 清单解析与字段校验 / 未知字段 / 目录扫描 | `plugin.TestParseValidManifest`、`plugin.TestValidateRules`、`plugin.TestRejectsUnknownField`、`plugin.TestLoadDir` |
| 拓扑顺序 + 可选依赖告警 | `assemble.TestPlanOrderIsDeterministic` |
| 缺提供者 / 重复提供者 / 成环 / 级联阻断 / disabled / 重复 id | `assemble.TestPlan*` |
| 按序挂载并真的提供服务 | `assemble.TestUpMountsInPlanOrder` |
| 失败逆序回滚 / 坏计划拒绝 / 缺 Lookup | `assemble.TestUpRollsBackOnFailure`、`assemble.TestUpRejectsBadPlan`、`assemble.TestUpWithoutLookup` |
| preset:展开 + disabled + config 深合并 | `preset.TestLoadExpandAppliesDisableAndPatch` |
| preset:enable 白名单收口 / 缺失目标报错 | `preset.TestExpandEnableAllowlist` |
| preset:相对路径按文件目录解析 | `preset.TestRelativeToPresetDir` |
| preset:未知 disable/patch 目标报错 | `preset.TestUnknownTargetsFail` |
| preset:自身校验 / 跨目录重复 id | `preset.TestPresetValidation`、`preset.TestDuplicateIDAcrossDirs` |
| go 插件按注册表装载(含缺注册报错) | `loader.TestGoLookup` |
| ts 源码/Artifact 模式装载并挂载 | `loader.TestTSSourceLookup`、`loader.TestTSArtifactLookup` |
| 清单信息不足/未知 kind / 扫描排除规则 | `loader.TestTSLookupErrors`、`loader.TestCollectSources` |
| Inspect 能看到 fiber/服务/effect 标签 | `kernel.TestInspectShowsEffects` |
| 延迟激活:startup only / Ensure 幂等 / 错误 | `assemble.TestUpLazyMountsStartupOnly`、`TestEnsureMounts`、`TestEnsureErrors` |
| startup 失败逆序回滚 | `assemble.TestUpLazyStartupFailureRollsBack` |
| activation 校验 | `plugin.TestActivation` |
| 服务触发挂载/未知服务/幂等 | `assemble.TestEnsureForService` |
| EnsureAll 按 id 排序挂载 | `assemble.TestEnsureAll` |
| TriggerOnCron 参数校验/注册冲突/Dispose | `assemble.TestTriggerOnCronValidation`、`TestTriggerOnCronRegister` |
| 并发 Ensure 只挂载一次(-race) | `assemble.TestDeferredConcurrentEnsure` |
| 并发 TriggerOnCron 只成功一次 | `assemble.TestDeferredConcurrentTrigger` |
| TriggerOnEvent 触发/取消/参数 | `assemble.TestTriggerOnEvent` |
| 清单信息不足/未知 kind / 扫描排除规则 | `loader.TestTSLookupErrors`、`loader.TestCollectSources` |

## 9. 已知边界与下一步

1. **install / 签名 / capability** 已由 M5 落地(见 [install.md](install.md)、[caps.md](caps.md));`ada plan/up` 仍只读本地目录。
2. **bundle / profile 已在 `platform/bundle` 落地**:preset 仍是单层;多级组合见 [m4-composition.md](m4-composition.md)。
3. **版本约束缺失**:`inject` 只按服务名匹配,没有「期望 provider ≥ v2」;内核对 `MinVersion` 已有支持,plan 还没用上。
4. **`ada inspect` 不含 JS 侧 effect**:bridge 的 effect 由 Apply disposer 执行,不进入内核快照(见 m2-execution §2.3)。
5. **Go 插件无法从 CLI 动态加载**:Go 插件必须编译进宿主,CLI 对 ts 插件完整可用。
6. **隔离槽**:`Isolate` 语义还没进入清单(`isolate: { shell: true }`)。
7. **`ada up` 仍是全量编译**:kernel 的 `Fiber.Reload` 已支持 fiber 级热重载,但没有宿主接线。
8. **触发 helper 已做**:`EnsureForService`/`TriggerOnCron`/`TriggerOnEvent`;真正的事件源(onPage/命令面板)仍由宿主产生。

## 10. 验证命令

```powershell
go test -race -count=1 ./...                 # 含 plugin / assemble / loader / preset
go run ./cmd/ada plan examples/plugins       # 人可读计划
go run ./cmd/ada plan --preset examples/presets/base.preset.json
go run ./cmd/ada up --preset examples/git-ts/git.preset.json      # 真的挂载 TS 插件
go run ./cmd/ada inspect --preset examples/git-ts/git.preset.json # 诊断快照(fiber/服务/effect/事件)
pwsh scripts/guard.ps1                       # 红线门禁
```
