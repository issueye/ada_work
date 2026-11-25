# plugin-helper:看插件、生成插件、改 preset(TS 实现)

`plugins/tool/plugin-helper` 属于 **tool 插件**,用 **TypeScript** 实现:给模型一组
「管理已有插件 + 生成新插件骨架」的工具。它只做**文件与清单层面**的事:

- 读写全部走 `ada:fs`(fs seam 的装饰器链),因此 `fs-sandbox` / `workspace-guard`
  仍然决定能不能写、写到哪;
- **不做装载与编译校验**——那是 `ada check` / 装配期(`ada plan`)的职责,工具里不重复实现;
- **只生成 TS 插件**。TS 是运行期能从目录装载的唯一形态(生成即可装配);Go 插件要注册进
  `platform/host/builtins` 并重新编译,不在本工具范围。`plugin_list` / `plugin_show`
  仍可**只读**看 Go 插件的清单,但会标注「仅体检」。

发布 preset(`agent-release` / `agent-release-http`)默认启用它:它只改工作区文件与 preset 数据,
写入受 fs 策略约束。注意 `plugin_preset` 的改动是**进程级**的——要让某个机器人用上新 preset 里的
插件,需要重启它的进程(装配发生在进程启动时),已经跑着的那一轮不受影响。

## 工具

| 工具 | 只读 | 作用 |
| --- | --- | --- |
| `plugin_list` | ✅ | 从根目录(缺省 `/workspace/plugins`)往下两层找 `plugin.json`,输出 id/kind/版本/依赖/目录 |
| `plugin_show` | ✅ | 读一个插件的清单,并做静态体检:目录名与 id 是否一致、入口文件是否存在、依赖是否为空 |
| `plugin_scaffold` | | 生成新的 **TS** 插件骨架(清单 + `plugin.ts` + README) |
| `plugin_preset` | | 改 preset:`add`/`remove` 插件目录,`enable`/`disable` 插件 id |

### `plugin_scaffold`

| 参数 | 说明 |
| --- | --- |
| `id` | 必填,小写 kebab-case;与目录名一致 |
| `dir` | 缺省 `plugins/<id>`(相对 `/workspace`) |
| `name` / `summary` | 展示名与一句话说明,写进清单、注释与 README |
| `tool` | 示例工具名;给了就自动 `inject: {required:["tools"]}` |
| `kind` | 只接受 `ts`(传 `go` 会被 schema 拒绝:Go 需要 builtins 注册与重编) |
| `overwrite` | 目录已有 `plugin.json` 时是否覆盖(缺省 false) |

生成文件:

```
<dir>/plugin.json     清单(id/name/version/kind=ts/entry=plugin.ts/inject)
<dir>/plugin.ts       apply + 可选示例工具(ESM,无模块级状态)
<dir>/README.md       下一步与约定
```

生成的 TS 插件**生成即可用**:把 `<dir>` 加进 preset 的 `plugins`,再 `ada plan` 就能装配。
`platform/bridge/tool_plugin_helper_test.go` 验证的就是这条链路(生成 → 装载 → 调它自己的工具)。

### `plugin_preset`

| 参数 | 说明 |
| --- | --- |
| `preset` | preset 文件路径(相对 `/workspace` 或绝对虚拟路径) |
| `action` | `add` / `remove` / `enable` / `disable` |
| `id` | `enable`/`disable` 用;与清单 id 一致 |
| `dir` | `add`/`remove` 用;缺省 `plugins/<id>`。**按原样写进 preset**——preset 里的路径是相对 preset 文件的真实路径,不是 `/workspace` 虚拟路径 |

改完是合法 JSON(2 空格缩进 + 结尾换行);重复动作不会重复写条目。

## 典型流程

```text
plugin_scaffold id=weather-helper tool=weather_now summary="查询当前天气"
plugin_preset   preset=presets/agent-release.preset.json action=add dir=plugins/weather-helper
ada plan --preset presets/agent-release.preset.json     # 在终端里验证装配
```

## 安全边界与已知限制

- **写入受 fs 策略约束**:默认装配里 `fs-local` 把根夹在工作区;有 `fs-sandbox`/只读工作区时
  工具会被装饰器拒绝,这是设计如此,不是 bug。
- 生成的文件不写 secret、不写绝对宿主路径;模板注释要求「secret 按名走 `ada:secrets`」。
- **只有 TS**:Go 插件不是运行期从目录装载的,必须先注册 builtins 再重编;本工具不生成 Go。
- 不做 `plugin_remove`(删除插件目录是破坏性操作,留给宿主/CLI);`plugin_preset remove` 只改数据。
- `plugin_list` 只扫两层目录,深层嵌套(如 `plugins/group/sub/id`)需要显式传根目录;
  `node_modules` 与 `.` 开头的目录会跳过。
- 静态体检**不做编译**:TS 语法错误、服务缺失要等 `ada check` / `ada plan`。

## 测试

```powershell
go test ./platform/bridge -run 'TestPluginHelper'
go test ./platform/host/cli -run 'PluginHelper'
```

覆盖:空目录列表、生成三种文件、清单字段、重复生成被拒、**kind=go 被 schema 拒绝**、
体检报告、**生成的插件真的能被 loader 装载并调用它自己的工具**、preset 的 add/enable/disable。
