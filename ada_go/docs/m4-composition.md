# M4 组合层(bundle / profile)

> 契约来源:[plugin-kernel-design.md](plugin-kernel-design.md) §12.4。

## 1. 范围

```
platform/bundle/  L2 组合数据:bundle(插件条目)+ profile(bundle 列表 + 按 id 补丁)
```

与 M3 的 `platform/preset/` 的分工:

| | preset | bundle / profile |
|---|---|---|
| 输入 | 插件目录列表 | bundle 条目(id + 插件目录 + config + disabled) |
| config 覆盖 | **深合并** | **整体替换**(§12.4 规则 1) |
| 顺序 | 依赖图 | 依赖图;`insertBefore/After` 只告警并忽略 |
| 叠加 | 单层 | profile 在 bundle 之上按 id 打补丁 |

## 2. 数据模型

`ada.bundle.json`:

```json
{
  "name": "base",
  "version": "0.1.0",
  "entries": [
    { "id": "greeter", "plugin": "./plugins/greeter", "config": { "prefix": "你好" } },
    { "id": "presenter", "plugin": "./plugins/presenter", "disabled": true }
  ]
}
```

`my.profile.json`:

```json
{
  "name": "mine",
  "bundles": ["base.bundle.json"],
  "patch": [
    { "id": "greeter", "config": { "prefix": "Hello" } },
    { "id": "presenter", "disabled": false, "insertBefore": ["greeter"] }
  ]
}
```

规则:

- `entry.plugin` 是插件目录(相对 bundle 文件或绝对路径);相对路径不能逃逸 bundle 目录;
- `entry.id` 必须与插件清单 id 一致(暂不支持别名);
- 同一个 id 在多个 bundle 里重复 → **报错**;
- `entry.config` / `patch.config` **整体替换**清单里的 config(不是深合并);
- `patch.disabled` 可覆盖 bundle 的 disabled(`*bool` 区分「没写」与 `false`);
- `when`(entry/patch 都可写)是受限表达式(复用 `platform/expr/` 包);为假时 entry 跳过/patch 不生效;
- `insertBefore/After` 只产生 warning,不影响顺序(顺序由依赖图决定);
- `patch.id` 指向不存在的插件 → **报错**(静默忽略会让作者以为配置生效了)。

## 3. API

```go
b, err := bundle.Load("base.bundle.json")
p, err := bundle.LoadProfile("my.profile.json")
manifests, warnings, err := p.Expand()
```

- `Parse` / `ParseProfile`:严格 JSON(未知字段报错,`when` 语法解析期校验);
- `ExpandWithEnv(env map[string]any)`:表达式环境由调用方组装(约定命名空间 `env/os/profile/features`;缺失路径视为 null);
- `Expand`:加载 bundle → 应用 entry config/disabled → 应用 profile patch → 返回清单与警告;
- 返回值可直接交给 `assemble.Plan` / `assemble.Up`。

## 4. 偏差与边界

1. **`when` 已支持,但环境由调用方提供**:没有自动绑定 `env/os/profile/features`;`disabled` 仍是静态布尔(如需动态禁用请用 `when` 跳过 entry)。
2. **`engine` 已校验**:`ExpandWithOptions` 传入宿主版本做 semver 约束匹配(`HostVersion` 为空时跳过);清单 `engines` 同样在展开期校验。
3. **不支持 id 别名**:entry id 必须等于清单 id。
4. **没有 bundle 仓库/在线拉取**:`plugin` 只接受本地路径(相对或绝对)。
5. **没有 profile 继承链**:当前是「bundles + patch」单层;设计里的 profile 继承/覆盖优先级未实现。
6. **未校验 `isolate`/`intercept`**:entry 上的这两个字段还没有进入数据模型。
7. **组合与签名/信任未联动**:没有把 install 的 trust 写进组合结果。

## 5. 测试矩阵

| 场景 | 用例 |
|---|---|
| bundle 展开:config 整体替换 + disabled | `bundle.TestBundleExpand` |
| profile 补丁:替换/覆盖/insert 告警 | `bundle.TestProfilePatch` |
| 未知 patch id、跨 bundle 重复 id | `bundle.TestProfileErrors` |
| entry id 与清单不一致 | `bundle.TestEntryIDMismatch` |
| bundle 解析校验(未知字段/空/重复/逃逸) | `bundle.TestParseErrors` |
| profile 解析校验 | `bundle.TestProfileValidation` |
| 多 bundle 合并 | `bundle.TestMultipleBundles` |
| entry `when` 为真装配/为假跳过 | `bundle.TestEntryWhen` |
| patch `when` 为假不生效 | `bundle.TestPatchWhen` |
| 非法 `when` 解析期拒绝 | `bundle.TestWhenInvalid` |
| engine/engines 校验与宿主版本 | `bundle.TestEngineChecks`、`bundle.TestEngineInvalid` |

## 6. 下一步

1. 把 flow 的受限表达式抽成公共包,让 `disabled`/`when` 支持表达式(只读命名空间 `env/os/profile/features`)。
2. `engine`/`version` 校验与宿主版本比较。
3. entry 的 `isolate`/`intercept` 展开成内核标签。
4. profile 继承链与优先级;bundle 仓库/在线拉取与签名联动。

## 7. 验证命令

```powershell
go test -race -count=1 ./bundle
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```