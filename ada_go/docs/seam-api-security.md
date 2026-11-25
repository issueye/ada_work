# ada v2 安全 seam 契约（fs / sandbox / shell）

> 配套：[plugin-kernel-design.md](plugin-kernel-design.md)、[seam-api.md](seam-api.md)（llm/tools/session/prompts）。
> 本文冻结**宿主资源**这条边界：文件、沙箱、执行。它们是 DSH 里 policy 最厚、也最容易出安全洞的一层，必须在写实现前定死。

| 项 | 值 |
|---|---|
| 状态 | 契约草案（v1），待评审冻结 |
| 适用版本 | `apiVersion: 2`、`engines.ada >=2.0.0` |
| 参照 | DSH `dsh-fs` / `dsh-fs-local` / `dsh-fs-sandbox` / `dsh-fs-observation-policy` / `dsh-sandbox-policy` / `dsh-sandbox-local` / `dsh-bash-local` / `dsh-bash-sandbox` / `dsh-shell-env` |

---

## 0. 为什么三个必须一起冻结

文件与执行不是两件独立的事：**同一个沙箱判定必须同时管住 `read/write/edit` 和 `bash/pwsh`**。如果 fs 自己判一条规则、shell 自己判一条，就会出现「写文件被拦、用 shell 绕过」的经典漏洞。

DSH 的做法正是把它们绑在一起：

- `dsh-fs-sandbox`：**Sandbox-enforcing implementation of the filesystem seam** —— 「fences write/edit by the per-call sandbox mode（read-only denies mutation, workspace-write contains it to the workspace + temp roots）while reads pass through」。
- `dsh-bash-sandbox`：**Sandbox-consuming implementation of the bash executor seam** —— 「confines every command via ctx.sandbox, reports denial/enforcement result facts」。
- `dsh-sandbox-policy`：per-call resolver，deployment fallback + 每个 session 的 mode 与 workspace root，「shared by every enforcing capability family」。

所以 ada v2 把三者放进同一张契约：`sandbox` 是判定源，`fs` 与 `shell` 是**消费方**，且都通过同一套装饰器（§3）实施。

---

## 1. 共同模型

### 1.1 三道闸，一次判定

```
① 能力授权（静态，插件级）   caps.Broker：这个插件有没有 fs.write / exec
        │
② 沙箱判定（动态，每次调用）  sandbox.Resolve(op, target, session)
        │  Escalatable 且调用方要求升级
        ├──▶ 审批（approval，fail-closed）→ 升级 mode 或拒绝
        │
③ 装饰器执行（确定性顺序）    sandbox → observation → 实现
        │
        ▼  实际 syscall / 进程
```

- **① 是「插件能不能碰这个能力」**：清单声明 + 安装同意 + 信任等级（见主设计 §13.2）。
- **② 是「这次调用在哪」**：会话模式（read-only / workspace-write / danger-full-access）+ 路径 + 操作类型。
- **③ 是「怎么执行」**：装饰器链，后注册包外层（§3.2），任一环拒绝即终止。

三者都拒绝时错误码统一 `EDENIED`，但 `Details` 要区分来源（`cap` / `sandbox` / `approval`），UI 与审计据此给出不同提示（「去设置里授权」vs「当前会话只读」）。

### 1.2 虚拟路径

宿主绝对路径**不进**插件代码。路径是一等值类型：

```go
type Path string   // 虚拟路径：/workspace/a.txt、/tmp/x、/platform/plugin/<id>/...
```

| 前缀 | 映射 | 读 | 写 | 说明 |
|---|---|---|---|---|
| `/workspace` | 会话工作区根 | ✓ | ✓（仅 workspace-write） | 主要工作面 |
| `/tmp` | 会话私有临时目录 | ✓ | ✓ | 生命周期随会话 |
| `/platform/plugin/<id>` | 已安装插件目录 | ✓ | 仅插件自己（`fs.write.plugin`） | 代码与资产 |
| `/user/...` | 宿主绝对路径（显式映射） | ✓（`fs.read.host`） | ✗ | 兼容「读宿主文件」，默认关闭 |
| `/attachment/<id>` | `seam/storage` 资源 | ✓ | ✗ | 图片/附件 |

解析规则：

- `Resolve(virtual) (host string, err error)` 由 seam 实现；**解析与打开之间必须无 TOCTOU**（`openat`/`O_NOFOLLOW` 逐段，或 Windows 的 `FILE_FLAG_OPEN_REPARSE_POINT`）。
- 虚拟路径先 `path.Clean`；拒绝 `..` 越界、NUL、反斜杠混用。
- 符号链接逐段检查：链接目标必须仍在允许根内（`EvalSymlinks` 之后做前缀复核，**且**打开时用不跟随标志二次确认）。
- Windows 额外：盘符、UNC、8.3 短名、ADS（`a.txt:stream`）、保留名（`CON`/`NUL`）、大小写不敏感导致的越界（`WORKSPACE` vs `workspace`）、路径长度上限。

### 1.3 沙箱模式

| 模式 | fs 读 | fs 写 | shell | 典型场景 |
|---|---|---|---|---|
| `read-only` | 允许（工作区+宿主可读面） | **拒绝** | 只读命令白名单（`ls`/`grep` 等） | 代码审阅、只读分析 |
| `workspace-write` | 允许 | 仅 `/workspace` + `/tmp` | 允许，受同一模式约束 | 日常编码 |
| `danger-full-access` | 允许 | 允许（除插件目录保护） | 允许 | 显式授权的自动化 |

- 模式来自 **session 的 permission preset**（DSH `dsh-permission-presets` 把 sandbox-mode 与 approval-policy 打成一个产品级选择），可在会话内变更；变更必须写 session 事件（可审计、可 replay）。
- `danger-full-access` 仍需 ①能力授权；它放宽的是 ②，不是 ①。

### 1.4 装饰器链：为什么不用事件做强制

seam 提供装饰器注册：

```go
// 由 seam/fs、seam/shell 暴露
Decorate(fn func(FS) FS, opts DecorateOptions) (kernel.Dispose, error)
```

- **强制类策略用装饰器**：沙箱、审批前的预检、只读投影 —— 它们必须**确定性、可排序、同步可拒**。
- **观察类用事件**：`fs/read`、`shell/exited`、`sandbox/denied` —— fire-and-forget，不能阻断。
- 不把 enforcement 放在 waterfall 事件里：事件监听器可能 panic、可能不调 `next`、顺序靠注册顺序，不适合做安全边界。
- 装饰器链的注册顺序决定嵌套：`priority` 高者在外层（先判）；同一 priority 按注册顺序。`ada inspect --chain fs` 打印实际嵌套。

指定的三条链（默认 preset）：

```
fs:      sandbox-fs → observation → fs-local
shell:   sandbox-exec → env-filter → shell-local
storage: redact → storage-local
```

### 1.5 错误码（在 §1 通用码之上）

| 码 | 含义 |
|---|---|
| `EDENIED` | 拒绝；`Details.source = cap\|sandbox\|approval` |
| `ENOENT` | 不存在 |
| `EISDIR` / `ENOTDIR` | 类型不符 |
| `ECONFLICT` | 版本守卫失败（读后未再读就写 / 文件已变） |
| `EBINARY` | 二进制/非文本（读工具拒绝解码） |
| `ETOOLARGE` | 超过大小上限（读/写/输出） |
| `ETIMEOUT` | 命令超时（进程树已杀） |
| `EPROBE` | 沙箱后端功能探测失败（**fail-closed**） |

---

## 2. `seam/sandbox`

### 2.1 Go 接口

```go
package sandbox

type Sandbox interface {
    // Mode 返回某会话当前的模式（来自 permission preset，可被事件覆盖）。
    Mode(ctx context.Context, sessionID string) Mode

    // Resolve 是唯一判定入口：op + target（虚拟路径或命令）+ 会话。
    Resolve(ctx context.Context, req Request) (Decision, error)

    // Backends 列出可用后端及其功能探测结果。
    Backends() []BackendInfo

    // Facts 返回「这次调用实际被施加了什么约束」，供工具结果展示与审计。
    Facts(ctx context.Context, req Request) Facts
}

type Mode string // read-only | workspace-write | danger-full-access

type Op string // fs.read | fs.write | fs.edit | fs.list | shell.exec

type Request struct {
    SessionID string
    Op        Op
    Path      Path      // 文件操作
    Program   string    // 命令操作
    Args      []string
    Write     bool
    Network   bool      // 历史字段:网络已不受 seam 管控(ADR 0012),保留只为兼容 Request 结构
}

type Decision struct {
    Allow      bool
    Mode       Mode
    Reasons    []string  // "sandbox: read-only denies write" 之类，可直接展示
    Escalatable bool     // 是否可通过审批升级（→ approval）
}

type Facts struct {
    Backend   string   // "bwrap" | "landlock" | "seatbelt" | "acl" | "none"
    Mode      Mode
    Roots     []string // 实际允许根
    Enforced  bool     // 后端起效了吗（false = 仅策略层限制，要醒目提示）
}

type BackendInfo struct {
    Name    string
    Probe   error   // nil = 功能探测通过
    Kind    string  // "os-level" | "policy-only"
}
```

### 2.2 后端契约（provider）

| 后端 | 平台 | 实现要点 |
|---|---|---|
| `provider/sandbox-bwrap` | Linux | bubblewrap 绑挂：只暴露允许根；网络开关 |
| `provider/sandbox-landlock` | Linux | Landlock 路径/网络规则 |
| `provider/sandbox-seatbelt` | macOS | `sandbox-exec` profile |
| `provider/sandbox-windows-acl` | Windows | restricted token + ACL 受限目录 |
| `provider/sandbox-none` | 全平台 | 仅策略层（`Enforced=false`，UI 必须提示「未隔离」） |

规则：

1. **功能探测（probe）失败即 fail-closed**：后端声明不可用，装配时该 preset 报错或降级为 `policy-only` 并醒目提示。
2. 后端只负责「执行时约束」，**判定仍由 `Resolve` 统一给出**；后端不可自行放行。
3. `Facts` 必须回填到工具结果（DSH `dsh-bash-sandbox` 的 "reports denial/enforcement result facts" 同款），让模型与用户都知道这次是在什么约束下跑的。

### 2.3 事件

| 事件 | 模式 | 用途 |
|---|---|---|
| `sandbox/request` | Emit | 每次判定的观测（不含阻断） |
| `sandbox/denied` | Emit | 拒绝审计（含 source/cap/sandbox） |
| `sandbox/mode.changed` | Emit | 模式变更（写 session 事件） |

### 2.4 验收（`seamtest.Sandbox`）

| 用例 | 断言 |
|---|---|
| 三模式矩阵 | 3 模式 × 4 操作 = 12 组判定与预期一致 |
| 路径穿越 | `../`、绝对路径、NUL 全部拒绝 |
| 符号链接 | 指向根外的链接拒绝（读与写都要测） |
| 模式变更 | 变更写 session 事件，回放后判定一致 |
| 后端探测 | probe 失败 → `EPROBE` 或 `Enforced=false`，绝不放行 |
| Escalatable | 只读下写操作返回 `Escalatable=true` 且 `source=sandbox` |

---

## 3. `seam/fs`

### 3.1 Go 接口

```go
package fs

type FS interface {
    Stat(ctx context.Context, p Path) (FileInfo, error)
    List(ctx context.Context, req ListRequest) ([]DirEntry, error)

    // Read 有界读：默认上限 1 MiB；文本文件返回内容 + 版本；二进制返回 EBINARY 或摘要。
    Read(ctx context.Context, req ReadRequest) (ReadResult, error)

    // Write 原子写：临时文件 + rename；可带版本守卫（IfVersion）。
    Write(ctx context.Context, req WriteRequest) (WriteResult, error)

    // Edit 精确替换：必须提供读到的 Version（read-before-edit）。
    Edit(ctx context.Context, req EditRequest) (EditResult, error)

    // Search 可选能力（由 provider 声明 Supports）。rg 后端。
    Search(ctx context.Context, req SearchRequest) (SearchResult, error)
}

type ReadRequest struct {
    Path    Path
    Offset  int64  // 字节偏移
    Limit   int64  // 0 = 默认上限
    Format  Format // text | bytes | auto
}

type ReadResult struct {
    Content  string
    Truncated bool
    Version  Version   // 内容哈希 + mtime + size；后续 Write/Edit 的守卫凭证
    Kind     FileKind  // text | binary | image | dir
    MimeType string
}

type WriteRequest struct {
    Path      Path
    Content   []byte
    IfVersion Version  // 零值 = 允许覆盖（仅危险模式）；否则不一致 → ECONFLICT
    CreateDirs bool
}

type EditRequest struct {
    Path      Path
    IfVersion Version
    Old, New  string   // 字面替换，必须唯一命中
    All       bool
}

type ListRequest struct {
    Path      Path
    Recursive bool
    MaxDepth  int
    Limit     int
}
```

### 3.2 不变量

1. **读后写**：`Write/Edit` 必须带 `IfVersion`；策略 `policy/fs-observation` 在没有观测记录时直接拒绝（`ECONFLICT`）。这是 DSH `dsh-fs-observation-policy` 的 "read-before-edit, version-guarded write/edit"。
2. **原子写**：写临时文件 + `rename`；失败不留半文件。
3. **有界**：读默认 1 MiB、写默认 4 MiB、列目录默认 2000 条；超限 `ETOOLARGE`，由调用方显式放宽（工具层再受结果预算约束）。
4. **不跟随越界链接**：见 §1.2。
5. **断言式路径**：`Path` 必须是虚拟路径，宿主路径由 `Resolve` 内部处理；插件无法构造原始 `os.File`。

### 3.3 装饰器（policy 的实现形态）

| 装饰器 | 作用 | 对应 DSH |
|---|---|---|
| `policy/fs-sandbox` | 在调用前 `sandbox.Resolve`，拒绝写/越界；读按模式放行 | `dsh-fs-sandbox` |
| `policy/fs-observation` | 维护「已观测文件 → 版本」表；read-before-edit、写前校验；把观测到的最新内容注入模型上下文 | `dsh-fs-observation-policy` |
| `policy/fs-redact` | 读结果脱敏（密钥/token 正则） | — |

观察策略的上下文注入走 `prompts.Section`，不直接改 `llm.Request`（避免与 §5.3 的「不要监听 assemble」冲突）。

### 3.4 事件

| 事件 | 模式 | 用途 |
|---|---|---|
| `fs/read` | Emit | 遥测：谁在读什么、多大 |
| `fs/write` / `fs/edit` | Emit | 变更审计（含版本、字节数） |
| `fs/observe` | Waterfall | 观察策略的**同步闸**（返回 allow/deny），在装饰器内调用 |

### 3.5 验收（`seamtest.FS`）

| 用例 | 断言 |
|---|---|
| 读后写 | 无 `IfVersion` 的写被拒；版本过期写返回 `ECONFLICT` 且不落盘 |
| 原子性 | 模拟 rename 失败，目标文件保持旧内容 |
| 有界 | 超限读返回 `Truncated`；超限写 `ETOOLARGE` |
| 二进制 | `Kind=binary` 时不返回原文 |
| 穿越 | 见 Sandbox 矩阵，fs 层不得绕过 |
| 编码 | CRLF/LF、UTF-8 BOM、UTF-16 拒绝（`EINVALID` 并提示） |

---

## 4. `seam/shell`

### 4.1 Go 接口

```go
package shell

type Shell interface {
    // Run 执行一条命令；默认无 shell（Program+Args 直接 exec）。
    Run(ctx context.Context, cmd Command) (Result, error)

    // Stream 边跑边推增量（stdout/stderr 交错，带句柄）。
    Stream(ctx context.Context, cmd Command, onChunk func(Chunk) error) (Result, error)

    // Env 是受管环境变量注册表（DSH 的 dsh-shell-env 同款）。
    Env() Env
}

type Command struct {
    Program   string            // 必须是程序路径/名字，不含空格
    Args      []string
    Dir       Path
    Stdin     []byte
    Env       map[string]string // 只允许清单声明的变量 + 受管变量
    Timeout   time.Duration     // 0 = 120s；上限 30m（后台任务另走 jobs seam）
    Shell     bool              // 默认 false；true 必须由 policy 显式允许
    ShellArgs []string
}

type Result struct {
    ExitCode int
    Stdout   []byte            // 截断到 4 MB，Truncated 标记
    Stderr   []byte
    Truncated bool
    Duration time.Duration
    Facts     sandbox.Facts     // 实际约束（必须回填）
}

type Chunk struct {
    Stream string  // stdout | stderr
    Text   string
    Seq    int
}
```

### 4.2 规则

1. **无 shell**：`Shell=false` 时用 `exec.CommandContext(program, args...)`，参数不经 shell 解析；`Shell=true` 需要 `shell.exec.shell` 能力 + policy 放行（默认拒绝，DSH 的 bash 工具也是这套取舍）。
2. **环境净化**：`Env` 只给「清单声明 ∩ 宿主存在」的变量，加受管前缀 `ADA_*`；**密钥永不进 env**（用 `seam/secrets` 按名取）。变量名在 Windows 上按大小写不敏感匹配（系统常见写法是 `Path`/`SystemRoot`）；`PATH`/`PATHEXT` 完全缺失时补系统默认值，否则子进程里的外部命令会全部找不到（见 [seam-shell.md](seam-shell.md) §4）。
3. **进程树**：超时/取消时杀整棵进程树（`Setpgid`/Job Object），不能只杀父进程。
4. **输出**：累计 4 MB 截断；流式模式每 chunk ≤ 64 KiB，且回压（慢消费者阻塞 Loop）。
5. **沙箱事实**：`Result.Facts` 必须来自 `sandbox`，并在工具结果里可见（模型会据此判断「能做什么」）。
6. **后台任务**：超过 30 s 或需要交互的，转 `seam/jobs`（v1 `cap_exec_background` 的经验），shell seam 不做长驻进程。

### 4.3 装饰器

| 装饰器 | 作用 |
|---|---|
| `policy/shell-sandbox` | 每次 `Run/Stream` 前 `sandbox.Resolve(shell.exec, program)`；`Escalatable` 时走审批 |
| `policy/shell-env` | 过滤 env；拒绝 `ADA_*` 之外的注入 |
| `policy/shell-allowlist` | 程序级白名单（可选；默认靠沙箱路径隔离） |

### 4.4 事件

| 事件 | 模式 | 用途 |
|---|---|---|
| `shell/start` | Emit | 命令与参数（脱敏后） |
| `shell/exited` | Emit | 退出码、耗时、截断、沙箱事实 |
| `shell/denied` | Emit | 拒绝审计 |

### 4.5 验收（`seamtest.Shell`）

| 用例 | 断言 |
|---|---|
| 无 shell | `Program="echo; rm -rf"` 作为参数时原样传递，不被解释 |
| Shell 关闭 | `Shell=true` 无能力时 `EDENIED` |
| env 泄漏 | 宿主 `PATH`/`SECRET` 不可见（只白名单 + `ADA_*`） |
| 超时杀树 | 子进程再起子进程，超时后全部消失（用进程快照断言） |
| 输出截断 | 超过 4 MB 时 `Truncated=true` 且命令被终止 |
| 沙箱事实 | `Facts.Enforced` 为 false 时结果里可见（UI 提示未隔离） |

---

## 5. 与 DSH 的对照

| 本方案 | DSH | 差异 |
|---|---|---|
| `seam/sandbox` + `provider/sandbox-*` | `dsh-sandbox-policy` + `dsh-sandbox-local` | DSH 把「策略解析」和「后端」拆成两个包；本文合成一个 seam + 后端注册表，减少一层服务 |
| `policy/fs-sandbox`（装饰器） | `dsh-fs-sandbox`（seam 的 enforcing 实现） | DSH 是「另一份 fs 实现」；本文用装饰器包裹，避免两套路径逻辑漂移 |
| `policy/fs-observation`（装饰器 + `fs/observe`） | `dsh-fs-observation-policy`（event gate，无服务 API） | 一致：观察策略不加服务方法 |
| `seam/shell` + `policy/shell-sandbox` | `dsh-bash-local` + `dsh-bash-sandbox` + `dsh-shell-env` | 一致；本文把 env 注册表放进 seam |
| `seam/fs` + `provider/fs-local` | `dsh-fs-local` + `dsh-fs-ssh` | SSH 后端留作 provider 扩展 |
| preset 的 `sandboxMode` | `dsh-permission-presets` | 一致：模式与审批合成一个产品级选择 |

---

## 6. 待决问题

1. **`fs.Search` 是否独立成 `seam/fs-search`**：DSH 是独立包（`dsh-tool-fs-search`，内置 ripgrep）。本文暂并入 fs 的 `Search` 方法 + `Supports("search")`，代价是 provider 必须同时实现两种职责。
2. **`/user/...` 读宿主绝对路径**是否保留：v1 为兼容 `read_file` 开放过；v2 建议默认关闭，由 `fs.read.host` 能力 + 会话模式双开。
3. **沙箱后端在 Windows 的默认值**：`acl` 后端依赖 restricted token，需要额外进程；是否默认 `policy-only` 并强制 UI 提示。
4. **模式变更的持久化**：写 session 事件即可，还是同时写 settings（跨会话默认）？建议前者，后者由 permission preset 提供默认值。
5. **`shell.Env` 的命名空间**：`ADA_*` 由宿主保留（与 v1 `ADAG_` 冲突时迁移），确认前缀。
6. **`policy/fs-observation` 的上下文注入量**：观测文件内容进提示会吃预算，建议只注入「路径 + 版本 + 摘要」，全文由工具按需读。

---

## 7. 实施顺序（接 seam-api.md 附录 C）

| # | 任务 | 依赖 | 估算 |
|---|---|---|---|
| C8 | `seam/sandbox` + `provider/sandbox-none` + 判定矩阵 | M1/M2 | 2 d |
| C9 | `seam/fs` + `provider/fs-local` + `policy/fs-sandbox` | C8 | 3 d |
| C10 | `policy/fs-observation` + 版本守卫与 `ECONFLICT` | C9 | 2 d |
| C11 | `seam/shell` + `provider/shell-local` + `policy/shell-sandbox`/`shell-env` | C8 | 3 d |
| C12 | 三平台后端（bwrap/landlock、seatbelt、Windows ACL） | C8–C11 | 5–8 d（可分批） |
| C13 | 拒绝矩阵 + 审计 + `ada inspect --chain` | C8–C12 | 2 d |

C13 已落地:`platform/securitytest`(fs/shell 矩阵,跨插件一致性测试)与 `ada inspect --chain`(fs/shell/workspace 装饰器优先级快照);caps 拒绝审计仍待补;net 矩阵随 ADR 0012 移除。

合计 **17–23 人日**；C8 必须先做（判定矩阵是所有后端的共同契约），C12 可以按平台并行。
