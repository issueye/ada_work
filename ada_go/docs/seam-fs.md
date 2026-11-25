# M5 安全(第一刀:seam/fs)

> 契约来源:[seam-api-security.md](seam-api-security.md) §1、§3。本页记录**已实现子集**、偏差与边界。
> 工作区根可由 [workspace-plugin.md](workspace-plugin.md) 托管；本文的 `workspace` 配置只在没有 workspace provider 时生效。

## 1. 范围

```
plugins/seam/fs/                契约:Path / 类型 / FS 接口 / 错误码 / 装饰器 Chain / seam 插件
plugins/provider/fs-local/      实现:虚拟路径 → 根目录、有界读、原子写、版本守卫、精确替换
plugins/policy/fs-sandbox/      策略:按模式(read-only / workspace-write / danger)拒绝写
```

装配关系与 session seam 一致:

```
seam/fs 插件 ──provide "fs"──▶ *fs.Chain
provider/fs-local ──inject "fs"──▶ Chain.Install(实现)
policy/fs-sandbox ──inject "fs"──▶ Chain.Decorate(沙箱包装,priority=100)
消费者 ──inject "fs"──▶ Chain.Get() ──▶ 组装后的 FS(装饰器在外层)
```

## 2. 契约要点

| 类型/方法 | 说明 |
|---|---|
| `Path` | 虚拟路径,只允许 `/workspace/...` 与 `/tmp/...`(本刀实现两个根) |
| `FS.Read` | 有界读:默认 1 MiB;`Truncated` 表示截断;二进制按 text 读 → `EBINARY` |
| `FS.Write` | 原子写(临时文件 + rename);`IfVersion` 版本守卫;默认上限 4 MiB |
| `FS.Edit` | 精确替换;必须带 `IfVersion`(read-before-edit);多处命中需 `All` |
| `FS.List` | 递归受 `MaxDepth` 约束;超过 `Limit`(默认 2000)返回 `ETOOLARGE` |
| `Version` | 内容哈希 + mtime + size;provider 与测试共用 `VersionOf` |
| `Chain.Decorate` | priority 高者在外层(先判);同优先级后注册者在外层 |
| `Chain.Root` | 返回底座实现的宿主根;供 `node:fs` 安装与诊断使用,不进入 `FS` 接口 |
| 错误码 | `EDENIED`/`ENOENT`/`EISDIR`/`ECONFLICT`/`EBINARY`/`ETOOLARGE`/`EINVALID` |

`EDENIED` 的 `Message` 带 `source=sandbox|cap|approval`,便于 UI/审计区分来源。

## 3. 安全不变量(实现侧)

- **越界拒绝**:`..`、绝对宿主路径、反斜杠、NUL、非 `/workspace` `/tmp` 根 → 拒绝(不是「找不到」)。
- **符号链接**:解析目标后必须仍在同一根内;根外链接拒绝(读与写都检查)。
- **读后写**:`Edit` 无 `IfVersion` → `ECONFLICT`;版本不一致 → `ECONFLICT`,且不落盘。
- **原子写**:同目录临时文件 + `rename`;失败时目标文件保持旧内容。
- **有界**:读/写/列目录默认上限;超限是错误,不是静默截断(读写)或截断列表。
- **换行/编码**:内容按字节透传,CRLF 不改写。

## 4. 偏差与已知风险(必须写清楚)

1. **TOCTOU 窗口**:当前是「解析最深已存在祖先(含父目录符号链接),再按真实路径打开」。
   打开时未使用 `O_NOFOLLOW` / 逐段 `openat`(Windows 未用 `FILE_FLAG_OPEN_REPARSE_POINT`),
   因此链接在解析与打开之间被替换的理论窗口仍存在。修复需要平台专用打开标志,
   列入下一刀(不能假装已解决)。父目录符号链接写逃逸已修复并有回归用例
   `TestWriteThroughSymlinkParent`。
2. **版本哈希是有界的**:文件超过 1 MiB 时,版本只由前 1 MiB + mtime + size 组成,
   与契约的「内容哈希」有偏差;足以检测常规编辑,但理论上可构造同前缀碰撞。
3. **只有 `/workspace` 与 `/tmp`**:`/platform/plugin/<id>`、`/user/...`、`/attachment/*` 未实现。
4. **`Search` 已实现(字面量)**:大小写敏感、只搜文本、单文件 ≤1 MiB、默认 limit 200;没有正则/忽略大小写/排除规则。
5. **沙箱判定已接到 `sandbox.Resolve`**:`fs-sandbox` 每次操作先问 sandbox 后端(模式来自后端配置);设计要求的
   「模式变更写 session 事件、回放后判定一致」仍未做。
6. **审批已接入可升级拒绝**:`fs-sandbox` 在 sandbox 判定 `Escalatable` 时调用 `seam/approval`(默认审批人拒绝);
   能力授权由独立的 `policy/caps-gate` 强制,`fs-sandbox` 不重复做 caps。
7. **Windows 特性未覆盖**:盘符/UNC/8.3 短名/ADS/保留名/大小写不敏感越界/路径长度。
8. **redact / observe 已实现**:`policy/fs-redact` 对读结果做正则脱敏;`policy/fs-observe` 每次操作后广播 `fs/observe` 事件(payload 含 op/path/bytes/plugin/error)。「读后写表」的持久化投影仍未实现。

## 5. 测试矩阵

| 场景 | 用例 |
|---|---|
| 装饰器链:未安装 / 组装顺序 / dispose / Applied | `fs.TestChainCompose` |
| 写→读→编辑、版本守卫、read-before-edit | `fslocal.TestWriteReadEdit` |
| `..`/绝对/反斜杠/NUL/未知根拒绝 | `fslocal.TestRejectsEscape` |
| 超限写、有界读、二进制 | `fslocal.TestBoundsAndBinary` |
| 列目录(非递归/递归/上限) | `fslocal.TestList` |
| 符号链接指向根外 | `fslocal.TestSymlinkEscape`(平台不支持时跳过) |
| CRLF 原样返回 | `fslocal.TestPreservesCRLF` |
| 三模式 × 写路径矩阵、拒绝不触底 | `fssandbox.TestModeMatrix`、`fssandbox.TestDeniedDoesNotReachInner` |
| seam+provider+policy 经内核装配 | `fssandbox.TestKernelIntegration` |
| 无 provider 时 `EUNIMPLEMENTED` | `fssandbox.TestKernelWithoutProvider` |
| redact:默认 key 模式命中/无命中不变/非法正则/写透传 | `fsredact.TestRedactsDefault`、`TestNoMatch`、`TestInvalidPattern`、`TestPassthrough` |
| redact 内核集成(AWS key 被脱敏) | `fsredact.TestKernelIntegration` |
| observe:读/写记录、失败记录、skipOps | `fsobserve.TestRecordsReadAndWrite`、`TestRecordsError`、`TestSkipOps` |
| observe 内核集成与「拒绝也记录」 | `fsobserve.TestKernelIntegration`、`TestObserveDeniedCalls` |

## 6. 下一步

1. `sandbox` seam 与 `fs-sandbox` 真正联动(模式来自 session 事件)。
2. `fs-observation`:读后写表 + 写前校验,把版本守卫提升为「观测到才允许写」。
3. `fs-redact`:读结果脱敏(secret/token 正则);模式表与编译在 `platform/redact`,与 `shell-redact` 共用同一份。
4. 平台专用打开标志修复 TOCTOU;Windows 路径特性补齐。
5. `seam/shell`:命令白名单、超时/进程树、输出预算;复用同一装饰器链思路。

## 7. 验证命令

```powershell
go test -race -count=1 ./plugins/...
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```