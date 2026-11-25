# 工作区插件设计（seam/workspace）

> 状态：**v1 已实现（W1–W9，2026-11-21）**。契约、provider、工具、策略、fs/shell 托管模式、
> 会话绑定、CLI 与示例都已落地；多根挂载、远程工作区、prompt 动态段仍按 §2.2 延期。
> 归属：能力按三段式拆成 `seam/workspace` + `provider/workspace-local` + `policy/workspace-guard`；
> 模型工具放 `workspace-helper`；会话绑定只改 `core/agent-loop` 的上下文传播，不新增 core 插件。

### 实现文件

| 切片 | 文件 | 验证 |
|---|---|---|
| W1 | [`plugins/seam/workspace`](../plugins/seam/workspace/workspace.go) | Chain/绑定/错误码契约测试 |
| W2 | [`plugins/provider/workspace-local`](../plugins/provider/workspace-local/workspace_local.go) | 多根/越界/只读/符号链接/切换测试 |
| W3 | [`plugins/tool/workspace-helper`](../plugins/tool/workspace-helper/workspace.go) | info/list、绑定、无 provider 测试 |
| W4 | [`plugins/provider/fs-local`](../plugins/provider/fs-local/fs_local.go) | 独立/托管/不一致/未就绪测试 |
| W5 | [`plugins/provider/shell-local`](../plugins/provider/shell-local/shell_local.go) | 默认 cwd/绑定/只读/不一致测试 |
| W6 | [`plugins/core/agent-loop`](../plugins/core/agent-loop/agent_loop.go) | 绑定传播到工具 ctx |
| W7 | [`plugins/policy/workspace-guard`](../plugins/policy/workspace-guard/workspace_guard.go) | 前缀/glob/读/执行/事件测试 |
| W8 | [`examples/workspace`](../examples/workspace/workspace.preset.json)、`ada agent --workspace` | CLI 端到端与未知 ID 拒绝 |
| W9 | `ada_jsrt/jsrt`(同级独立库)、[`platform/bridge`](../platform/bridge/plugin.go) | 原生 fs/shell 继承工具 ctx(`TestJSNativeFSInheritsCallBinding`) |

## 1. 要解决的问题

当前「工作区」只是一个散落的配置项：

- [`fs-local`](../plugins/provider/fs-local/fs_local.go) 与 [`shell-local`](../plugins/provider/shell-local/shell_local.go) 各自配置一个 `workspace` 根，两处必须手工保持一致；
- [`session.Meta`](../plugins/seam/session/session.go) 已经有 `Workspace` 字段，但没有任何组件消费它；
- `/workspace` 被工具、sandbox、脚本插件硬编码，换根只能改 preset 后重启；
- 没有统一的地方回答「当前工作区是谁、只读与否、哪些路径应忽略」；
- 多会话同进程时，无法表达「A 会话在 repo、B 会话在 docs」。

结果是：**同一事实有多个来源**。一处改根、另一处忘改，就会出现 fs 能读、shell 却在旧目录里跑；session 记录了工作区，但工具完全不看它。

## 2. 目标与非目标

### 2.1 目标

1. **一个事实来源**：工作区根、只读标记、忽略规则由 `workspace` 服务统一拥有。
2. **会话绑定**：会话创建时把工作区 ID 写进 `session.Meta.Workspace`，工具调用按该绑定解析路径。
3. **fs / shell 同一解析**：两者都通过 `workspace.ResolveHost` 得到宿主根，不再各自维护。
4. **可替换实现**：本地目录只是默认实现；将来容器、远程、内存工作区可以换 provider。
5. **模型不可切换根**：工作区由宿主/用户选择，模型只能查询；避免一条 prompt 就把文件面换走。
6. **兼容旧 preset**：没有 workspace 栈时，`fs-local` / `shell-local` 仍按自己的 `workspace` 配置独立工作。

### 2.2 非目标（本次明确不做）

- 不做多根挂载（`/workspace` 之外的 `/docs`、`/src`）；v1 只有 `/workspace`。
- 不做模型驱动的 `workspace_use` / `workspace_switch` 工具。
- 不做 OS 级沙箱；`shell-local` 的 cwd 限制不等于进程隔离（见 ADR 0001）。
- 不做远程工作区（SSH / 容器 / 云端）。
- 不做 git worktree 管理、文件监听、索引。
- 不改 `seam/fs`、`seam/shell` 的公开方法签名；只在 provider 内部消费 workspace。
- 不把工作区根绝对路径写进工具结果、事件、日志或 prompt。

## 3. 归属判定

按 [AGENTS.md](../AGENTS.md) §3 决策树：

| 问题 | 判定 |
|---|---|
| 它定义宿主资源「工作区根」的接口吗？ | 是 → `plugins/seam/workspace`（只有类型/接口/错误/事件） |
| 它实现一个本地目录工作区吗？ | 是 → `plugins/provider/workspace-local` |
| 它确定性地拒绝某次写/执行吗？ | 是 → `plugins/policy/workspace-guard`（装饰器） |
| 它给模型提供查询工具吗？ | 是 → `plugins/tool/workspace-helper` |
| 会话如何带上工作区？ | 已有 `session.Meta.Workspace`；`core/agent-loop` 把它转成调用上下文 |

**为什么不是 `core/workspace`**：工作区是宿主持有的资源边界，不是 agent 循环的一部分。把它放 core 会让 fs/shell 反过来依赖 core，破坏「能力 = seam」。**为什么不是只加一个 preset 字段**：那样仍然没有会话绑定，也没有单一解析路径。

## 4. 结构

```text
seam/workspace（服务 workspace + Chain）
        ▲
        │ Install / Decorate
        │
provider/workspace-local ──Info/ResolveHost──▶ fs-local / shell-local
        ▲
        │ 可选策略
policy/workspace-guard（忽略规则、只读之外的拒绝）
        │
workspace-helper（workspace_info / workspace_list，只读）

session.Meta.Workspace ──agent-loop 上下文──▶ resolver 选择绑定
```

依赖方向：

```text
seam/workspace   ✗→ provider/policy/core/tool
provider/workspace-local ✓→ seam/workspace + kernel
policy/workspace-guard  ✓→ seam/workspace
workspace-helper        ✓→ seam/workspace + seam/tools
fs-local / shell-local  ✓→ seam/workspace（可选，仅解析）
agent-loop              ✓→ seam/workspace（只做 ctx 绑定，不访问实现）
```

## 5. 契约

### 5.1 类型

```go
package workspace

// ID 是工作区稳定标识，只允许 kebab-case；出现在 session.Meta 与工具结果里。
type ID string

// Op 是解析用途。同一路径在读写下可能有不同授权。
type Op string

const (
    OpRead   Op = "read"
    OpWrite  Op = "write"
    OpEdit   Op = "edit"
    OpList   Op = "list"
    OpSearch Op = "search"
    OpExec   Op = "exec"
)

func (o Op) Mutating() bool { return o == OpWrite || o == OpEdit || o == OpExec }

// Info 是插件可见的工作区描述。Host 根不在其中，避免绝对路径越过 seam。
type Info struct {
    ID       ID       `json:"id"`
    Name     string   `json:"name"`
    Mount    string   `json:"mount"` // v1 固定 "/workspace"
    ReadOnly bool     `json:"readOnly,omitempty"`
    Ignore   []string `json:"ignore,omitempty"`
}

// Target 是解析结果。Host/Root 只允许 fs/shell provider 内部使用。
type Target struct {
    Workspace ID     `json:"workspace"`
    Virtual   string `json:"virtual"` // 规范化后的虚拟路径
    Rel       string `json:"rel"`     // 工作区相对路径，slash 分隔
    Host      string `json:"-"`       // 绝不可序列化、不可进事件/日志/工具结果
    Root      string `json:"-"`       // 工作区根，同样只给 fs/shell provider
}

// Binding 由会话元信息派生，随调用上下文传播；不是可变注册表。
type Binding struct {
    SessionID string
    Workspace ID
}
```

`Info.Mount` 目前恒为 `/workspace`，保留字段是为了将来多根挂载时不改结构；v1 的 provider 必须拒绝其他 mount。

### 5.2 服务接口

```go
type Service interface {
    Current(ctx context.Context) (Info, error)
    Get(ctx context.Context, id ID) (Info, error)
    List(ctx context.Context) ([]Info, error)

    // ResolveHost 只给 fs/shell provider 用；它会同时完成路径与授权检查。
    ResolveHost(ctx context.Context, op Op, p string) (Target, error)

    // Switch 只给宿主/用户调用，不暴露成模型工具；已有会话的绑定不变。
    Switch(ctx context.Context, id ID) (Info, error)
}
```

解析规则（provider 必须实现）：

1. `p` 必须以 `/workspace` 开头；`/workspace` 本身表示根目录。
2. `path.Clean` 后拒绝 `..`、NUL、反斜杠；非 `/workspace` 根返回 `EDENIED`。
3. 绑定工作区：先读 `workspace.BindingFrom(ctx)`；没有绑定时用 `Current`。
4. 绑定指向未知 ID 时**不静默回退**，返回 `ENOTFOUND`（fail-closed）。
5. `ReadOnly` 工作区拒绝 `OpWrite` / `OpEdit` / `OpExec`，但允许读、列目录、搜索。
6. 符号链接：解析最深已存在祖先，最终宿主路径必须仍在工作区根内；否则 `EDENIED`。
7. `Target.Rel` 用 slash，根目录为 `"."`；`Target.Host` 必须是绝对路径且已去符号链接。
8. `ResolveHost` 不得缓存跨调用的结果：`Switch` 后新调用必须看到新根。

### 5.3 Chain

与 `seam/fs` 相同的装饰器形态，策略必须能确定性拒绝：

```go
type Chain struct { /* base + decorations */ }

func (c *Chain) Install(base Service) (kernel.Dispose, error)
func (c *Chain) Decorate(fn func(Service) Service, priority int) (kernel.Dispose, error)
func (c *Chain) Get() (Service, error) // 无 provider → EUNIMPLEMENTED
func (c *Chain) Applied() []int       // inspect --chain 用
```

`Decorate` 的优先级语义与 `seam/fs` 一致：高优先级在外层，先判；同优先级后注册者在外层。

### 5.4 上下文绑定

不引入新的 session 注册表，也不在 provider 里存 `sessionID → workspace` 映射：

```go
func WithBinding(ctx context.Context, b Binding) context.Context
func BindingFrom(ctx context.Context) (Binding, bool)
```

- `core/agent-loop` 在每轮开始时读 `sess.Meta().Workspace`，非空则写进 ctx。
- `fs-local` / `shell-local` 只把 ctx 原样传给 `ResolveHost`。
- 绑定随 `session.Meta` 持久化（[`seam/session`](../plugins/seam/session/session.go) 已有字段），因此 resume / fork 不丢。
- 没有绑定（旧 preset / 未指定）时，provider 用 `Current`。

### 5.5 错误码

复用 `seamerr`，新增两个工作区语义码：

| 码 | 含义 |
|---|---|
| `EUNIMPLEMENTED` | seam 在位但没有 workspace provider |
| `ENOWORKSPACE` | 有 provider 但没有任何已配置工作区 |
| `ENOTFOUND` | 绑定/请求的工作区 ID 不存在 |
| `EINVALID` | 配置非法（重复 ID、根为空、mount 非 `/workspace`） |
| `EDENIED` | 路径越界、只读工作区写/执行、策略拒绝；`Details.source = workspace\|policy` |
| `ECONFLICT` | `fs-local` / `shell-local` 自己的根与 workspace 根不一致 |

### 5.6 事件

| 事件 | 类型 | payload | 说明 |
|---|---|---|---|
| `workspace/changed` | emit | `{"previous":"repo","current":"docs"}` | 仅 `Switch` 成功且实际变化时发；已有会话不受影响 |
| `workspace/denied` | emit | `{"op":"write","workspace":"repo","path":"/workspace/.git/config","source":"policy"}` | 可选，由 `workspace-guard` 发；不是门禁 |

绑定变化不新增会话事件：它已经是 `session.Meta.Workspace`，重复写事件会造成两个事实来源。若将来允许会话中途切换，再按 `plugin:workspace/bound` 追加会话事件并单独评审。

## 6. provider/workspace-local 语义

### 6.1 配置

```json
{
  "id": "workspace-local",
  "version": "0.1.0",
  "kind": "go",
  "inject": {"required": ["workspace"]},
  "config": {
    "active": "repo",
    "workspaces": [
      {
        "id": "repo",
        "name": "ada_go_v2",
        "root": ".",
        "readOnly": false,
        "ignore": [".git", "bin", "dist"]
      },
      {
        "id": "docs",
        "name": "文档只读",
        "root": "docs",
        "readOnly": true
      }
    ]
  }
}
```

| 字段 | 必填 | 语义 |
|---|---|---|
| `workspaces[].id` | 是 | kebab-case，全局唯一 |
| `workspaces[].name` | 否 | 展示名，缺省用 id |
| `workspaces[].root` | 是 | 绝对路径，或以进程 cwd 为基准的相对路径；必须是已存在目录 |
| `workspaces[].readOnly` | 否 | 默认 false；true 时拒绝写/编辑/执行 |
| `workspaces[].ignore` | 否 | 交给 `workspace-guard` 的候选模式；provider 本身不据此拒绝 |
| `active` | 否 | 默认工作区；缺省取 ID 字典序最小者，保证确定性 |

装配期校验：根存在且为目录、ID 唯一、mount 固定、`active` 必须存在。任一失败 → `EINVALID`，不半启动。

### 6.2 Current / Switch

- `Current` 返回当前 Info（不含根路径）。
- `Switch` 在互斥锁内替换 current 并广播 `workspace/changed`；同一时刻只有一个 current。
- 已开会话靠 `BindingFrom(ctx)` 固定在自己的工作区；`Switch` 只影响没有绑定的新调用。
- 不允许把 `Switch` 注册成模型工具：模型改根等于绕过宿主选择。

### 6.3 路径安全

provider 自己完成根内校验，不复用 fs-local 的内部函数（provider 之间不得互相 import）：

- 根在装配期 `filepath.Abs` + `EvalSymlinks`；
- 相对段拼接后检查 `within(root, host)`；
- 目标已存在时 `EvalSymlinks` 后复核；目标不存在时解析最深已存在祖先，再拼回缺失段；
- Windows 额外覆盖：盘符、UNC、大小写不敏感、ADS、保留名；测试矩阵见 §13；
- 打开的原子性与 TOCTOU 仍由 fs-local 的打开路径负责；workspace 只保证「交给 fs 的宿主路径已经过根内校验」。

## 7. fs-local / shell-local 集成

### 7.1 兼容模式（默认）

`fs-local` / `shell-local` 的清单加**可选依赖**：

```json
"inject": {"required": ["fs"], "optional": ["workspace"]}
```

Apply 时：

1. `ctx.TryService("workspace")` 拿到 `*workspace.Chain`；拿不到 → 完全按旧行为，用自己的 `workspace` 配置。
2. 拿到 Chain 但 `Get()` 返回 `EUNIMPLEMENTED` → 仍按旧行为（seam 在位、无 provider）。
3. `Get()` 成功 → **托管模式**：
   - `/workspace` 的解析改为 `chain.Get().ResolveHost(ctx, op, p)`；
   - 自己不接受 `/workspace` 的根配置与 provider 不一致：provider 已就绪时在装配期比较，provider 晚于本插件安装时在首次解析再次比较，不同则 `ECONFLICT`；
   - 配置为空则不比较（由 workspace 提供根）。
4. `/tmp` 仍由 fs-local / shell-local 自己的 `tmp` 配置拥有，workspace 不接管。

**为什么用 `TryService` 而不是强制依赖**：强制依赖会让所有旧 preset 在升级后 pending；`optional` 让旧 preset 原样工作。**为什么不一致要报错而不是以一边为准**：两个根同时配置却不同，是配置错误；静默选择任意一边都会产生「读 A 写 B」。

### 7.2 fs-local

- `Read` / `Stat` / `List` / `Search` → `OpRead` / `OpList` / `OpSearch`；
- `Write` → `OpWrite`；`Edit` → `OpEdit`；
- 托管模式下不再做本地的 `/workspace` 前缀切换，直接把 `Target.Host` 当作打开目标；
- 原有 `fs-sandbox`、`fs-redact`、`caps-gate` 装饰器位置不变，仍在 fs 链外层。

### 7.3 shell-local

- `Command.Dir` 为空 → `/workspace`；
- 非空 → 必须是 `/workspace` 前缀，经 `ResolveHost(ctx, OpExec, dir)` 得到宿主 cwd；
- 只读工作区拒绝 `OpExec`（因为命令可能写文件）；
- 环境、白名单、超时、输出上限、进程树终止全部沿用 `shell-local` 现有逻辑。

**边界**：cwd 只限制相对路径，命令仍可用绝对路径访问根外。这必须由 `shell-guard` 的允许列表、caps 与将来的 OS 沙箱承担，workspace 不宣称自己是执行沙箱。

## 8. 会话绑定（core/agent-loop）

只改一处：每轮开始把会话元信息变成上下文绑定。

```go
// Agent.Run 入口附近；sess 已由 Agent.Open 创建。
if id := sess.Meta().Workspace; id != "" {
    ctx = workspace.WithBinding(ctx, workspace.Binding{
        SessionID: sess.ID(),
        Workspace: workspace.ID(id),
    })
}
```

- `Agent.Open` 不替宿主选工作区：`meta.Workspace` 为空就不绑定，由 provider 的 `Current` 决定。
- 宿主（CLI / server / desktop）在创建会话时写入 `Meta.Workspace`；值必须是已配置的 ID。
- fork 继承 `Meta.Workspace`（session provider 已复制 Meta）。
- 不存会话→工作区映射表，避免 load/switch 时产生第二份状态。

### 8.1 JS 边界（已修复）

脚本插件通过 `ada:fs` / `ada:shell` 调原生回调时，`jsrt` 现在会把**当前 worker 任务的调用 ctx**保存为 `callCtx`，原生 fs/shell 全部使用它：

- `Runtime.enqueue` 在 worker 执行每个任务前设置 `callCtx`，任务结束后清空；
- `nativeShell` / `nativeFSRead` / `nativeFSWrite` / `nativeFSEdit` / `nativeFSSearch` / `nativeFSList` 改用 `currentContext()`，不再退回 `context.Background()`；
- bridge 的 `ctx.tool` handler 把 `tctx` 一路传到 JS handler，因此脚本工具与 Go 工具看到同一个 `workspace.Binding`；
- 取消、超时也随调用 ctx 传播到 shell/fs。

边界仍在：`ctx.on` / waterfall 的 JS 回调使用事件自身的 ctx，事件 ctx 不一定带 Binding；它们会落到 `Current`。这是事件上下文的性质，不是工具调用的缺口。

回归用例：`platform/bridge.TestJSNativeFSInheritsCallBinding`（绑定 `beta` 时脚本 `ada:fs` 必须读 `beta` 根，而不是 `alpha`）。

### 8.2 真实端点验证（2026-11-21）

使用 `examples/workspace/real.preset.json`（OpenAI 兼容端点 + `ADA_LLM_API_KEY`）实测：

| 场景 | 结果 |
|---|---|
| `--workspace docs` + `workspace_info` / `read_file a.txt` | 绑定 `docs`，读到 `hello` |
| `--workspace docs` + `write_file blocked.txt` | `EDENIED [source=workspace] 工作区 docs 只读,拒绝 write`，文件未创建 |
| `--workspace repo` + `write_file` / `read_file real-test.txt` | 写入并读回成功，文件落在 repo 根，docs 无副本 |
| `--workspace repo` + `write_file .git/blocked.txt` | `EDENIED [source=policy]`，文件未创建 |
| `workspace_list` | 返回 `docs`(readOnly=true) 与 `repo` |
| `workspace_info` 原样 JSON | 只有 id/name/mount/ignore/readOnly，无宿主绝对路径 |
| `cmd_run` + shell-local 托管 | 默认 cwd 为 active repo 根；`--workspace docs` 时正确返回 `EDENIED`(只读拒绝 exec)；可写 `docsw` 绑定时 cwd 为 docs 路径 |

## 9. 工具

`plugins/tool/workspace-helper`，只读，不提供切换：

| 工具 | 参数 | 返回 |
|---|---|---|
| `workspace_info` | 无 | 当前会话绑定（若有）与 `Current`；`{id,name,mount,readOnly,ignore}`，**无宿主路径** |
| `workspace_list` | 无 | 所有已配置工作区的 ID/名称/只读标记，按 ID 排序 |

- 工具 Handler 从 ctx 的 `BindingFrom` 读取绑定；没有绑定时报告 `Current`。
- 两个工具都声明 `ReadOnly: true`，不触发审批。
- 不提供 `workspace_use`；工作区选择是宿主能力，不是模型能力。

## 10. policy/workspace-guard

可选的装饰器，挂在工作区链外层：

```json
{
  "id": "workspace-guard",
  "version": "0.1.0",
  "kind": "go",
  "inject": {"required": ["workspace"]},
  "config": {
    "denyWrite": [".git", "bin", "dist"],
    "denyRead": [".env", ".git"],
    "denyExec": [".git"]
  }
}
```

语义：

- 只对 `ResolveHost` 生效；匹配对象是 `Target.Rel`（slash 分隔），不是宿主绝对路径。
- 模式语法 v1 只支持 `path.Match` 的单段 glob 与固定前缀；固定前缀按路径段边界匹配（`.git` 命中 `.git/config`，不命中 `.gitignore`）。**不支持 gitignore 的 `!` 取反、`**` 跨段、目录继承规则**；需要完整 gitignore 时先扩契约，不在 provider 里偷偷实现。
- 拒绝返回 `EDENIED`，`Details.source = "policy"`，并可选发 `workspace/denied`。
- `workspace-guard` 是纵深防御，不是沙箱；真正的写边界仍由 `fs-sandbox` + caps + OS 沙箱承担。
- 未挂 policy 时，provider 仍执行路径越界与 `readOnly`；`ignore` 字段只是元数据。

## 11. 安全不变量

1. **根选择只来自宿主**：没有模型可调用的切换工具。
2. **绝对路径不出 seam**：`Info` 无根字段；`Target.Host` / `Target.Root` 带 `json:"-"`；工具/事件/日志禁止打印。
3. **绑定不静默回退**：会话绑定指向未知工作区时 `ENOTFOUND`，不落到 `Current`。
4. **路径先授权再打开**：`ResolveHost` 同时做越界、只读、策略检查；fs-local 不再自行猜测根。
5. **能力位不因工作区而放宽**：`fs.read` / `fs.write` / `shell.exec` 的 caps 仍在原处；workspace 只是目标选择。
6. **拒绝可审计**：`EDENIED` 带 `source`；`workspace/changed` 可被 audit-log 记录。
7. **不替代沙箱**：`readOnly` 与 ignore 都是策略层；OS 级隔离仍按 ADR 0001 延期。

## 12. 兼容矩阵

| workspace seam | provider | fs-local `workspace` | 行为 |
|---|---|---|---|
| 无 | 无 | 必填 | 旧行为，独立根 |
| 有 | 无 | 必填 | 旧行为；`ResolveHost` 不会被调用 |
| 有 | 有 | 空 | 托管，根来自 workspace |
| 有 | 有 | 与 workspace 根相同 | 托管，装配期校验通过 |
| 有 | 有 | 与 workspace 根不同 | `ECONFLICT`，装配失败 |
| 有 | 有 | 空，但 `/tmp` 配置存在 | `/workspace` 托管，`/tmp` 仍本地 |

旧 preset 不需要改；`fs-local` / `shell-local` 声明了可选依赖 `workspace`，所以旧 preset 的 `ada plan` 会多一条「可选依赖未提供」警告，这是 P6「依赖永不静默」的预期行为。新 preset 若要用工作区能力，加 `seam-workspace` → `workspace-local` →（可选）`workspace-guard` → `workspace-helper`。

## 13. 测试矩阵

| # | 场景 | 期望 |
|---|---|---|
| 1 | provider 配置空 / 根不存在 / 根不是目录 | `EINVALID`，装配失败 |
| 2 | 重复 ID、`active` 不存在、mount 非 `/workspace` | `EINVALID` |
| 3 | `Current` 缺省选择 | 按 ID 字典序，不依赖配置数组顺序 |
| 4 | `Switch` 成功 / 同 ID | 发 `workspace/changed` / 不发 |
| 5 | 绑定存在 | `ResolveHost` 用绑定，不受 `Switch` 影响 |
| 6 | 绑定未知 ID | `ENOTFOUND`，不回退 |
| 7 | `..`、NUL、反斜杠、非 `/workspace` 根 | `EDENIED` |
| 8 | 符号链接指向根外（含不存在目标的父目录链接） | `EDENIED` |
| 9 | 只读工作区读/写/编辑/执行 | 读放行，其余 `EDENIED` |
| 10 | `workspace-guard` 命中 write/read/exec 模式 | 对应 `EDENIED`，`source=policy` |
| 11 | fs-local 托管读写 / 独立模式回归 | 文件落到正确根；旧测试全绿 |
| 12 | fs-local 根不一致 | `ECONFLICT` |
| 13 | shell-local 默认 dir / 显式 dir / 越界 | 正确 cwd / `EDENIED` |
| 14 | agent-loop 绑定传播 | `sess.Meta.Workspace` 非空时 fs 解析到该根 |
| 15 | 无绑定旧 preset | 仍用 fs-local 自己的根 |
| 16 | 工具输出 / Info JSON / 事件 | 不含宿主绝对路径 |
| 17 | 并发 `Switch` + 多会话解析 | `-race` 通过，单次调用线性化 |
| 18 | Windows 大小写、UNC、ADS、保留名 | 拒绝或规范化后仍在根内 |
| 19 | 脚本插件在多会话下 | 按 §8.1 落到 `Current`（预期行为，不是回归） |
| 20 | 安装/清单 | `ada check`、`ada plan`、`ada inspect --chain --preset examples/workspace/workspace.preset.json` 通过 |

## 14. 实施顺序与预算

| 切片 | 内容 | 预算（行） | 状态 |
|---|---|---|---|
| W1 | `seam/workspace` 类型 + Chain + 上下文 helper + 契约测试 | 276 / 400 | ✅ 已完成 |
| W2 | `provider/workspace-local` 多根解析 + 路径安全 + 测试 | ~270 / 800 | ✅ 已完成 |
| W3 | `workspace-helper` 两个只读工具 + 测试 | ~135 / 800 | ✅ 已完成 |
| W4 | `fs-local` 托管模式 + 兼容矩阵测试 | 总 ≤ 800 | ✅ 已完成 |
| W5 | `shell-local` 托管模式 + 测试 | 总 ≤ 800 | ✅ 已完成 |
| W6 | `agent-loop` 绑定传播 + 测试 | 少量 | ✅ 已完成 |
| W7 | `policy/workspace-guard` + 拒绝矩阵 | ~125 / 800 | ✅ 已完成 |
| W8 | CLI `--workspace <id>`、示例 preset、文档 | host 1443 / 1500 | ✅ 已完成 |
| W9 | jsrt/bridge 原生 fs/shell 继承工具 ctx | 少量 | ✅ 已完成 |

每个切片独立可回滚：W1/W2 不改变任何旧 preset 行为；W4/W5 只在 workspace provider 存在时走新路径。W8 增加 `examples/workspace/` 示例，不把 workspace 栈塞进默认 `examples/agent`，避免一次改动影响既有 golden。

门禁：每刀跑 `go test -race ./...`、`pwsh scripts/guard.ps1`、`ada inspect --chain --preset examples/workspace/workspace.preset.json` 快照；预算超限走 ADR。

## 15. 备选方案与取舍

| 方案 | 为什么不选 |
|---|---|
| 只加 `workspace` 配置字段，不做插件 | 仍旧两处配置、无会话绑定、无统一解析 |
| `core/workspace` 单插件 | fs/shell 要依赖 core，层级反转；且无 provider 可替换点 |
| 新增 `fs-workspace` provider 包装 fs-local | provider 不能 import provider；会复制 fs 实现 |
| 在 `ReadRequest` 里加根字段 | 每个调用点都要改，宿主路径进入请求结构，破坏虚拟路径不变量 |
| 现在就做多根挂载 | 要改 sandbox、caps、所有工具与脚本模块；与 v1 目标无关，先单根验证模型 |
| 让模型调 `workspace_use` 切根 | 一条注入指令即可把文件面切到任意配置根；根选择应属于宿主 |

## 16. 开放问题

1. `/tmp` 是否也应随工作区切换？本设计维持「应用级 /tmp」，避免切根导致临时文件消失。
2. ignore 是否需要完整 gitignore 语义？v1 明确只做简单 glob，完整语义需要单独契约。
3. prompt 是否需要动态工作区段？`seam/prompts` 的 Section 是静态 Body；按会话/切换动态渲染需要扩展 seam，另开切片。
4. 会话中途切换工作区：本设计禁止；若将来允许，必须写 `plugin:workspace/bound` 会话事件，并处理 fork/resume。
5. 远程/容器工作区：`ResolveHost` 返回「宿主路径」的假设会被打破，需要按 provider 能力抽象为句柄。
6. ~~CLI 的 `--workspace` 接 ID 还是路径？~~ 已决定只接 ID；路径在 preset 配置里定义，避免命令行绕过 host 策略。

## 17. 与现有文档的关系

- 虚拟路径与 fs/shell 三道闸仍以 [seam-api-security.md](seam-api-security.md) 为准；本文只增加「根从哪来」。
- `session.Meta.Workspace` 的持久化语义见 [seam-session.md](seam-session.md)；本文不新增会话事件。
- 工作区不是沙箱：OS 级隔离仍见 [ADR 0001](adr/0001-deferred-m5-scope.md)。

