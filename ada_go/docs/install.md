# M5 安装与签名(install)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §13.4(签名与安装)、§13.2(信任等级)。

## 1. 范围

```
platform/install/     L2 基础设施:清单校验、Artifact 摘要、Ed25519 验签、原子安装、本机索引(index.json)
platform/host/cli     verify / install / list 命令
```

本刀已包含**校验、原子安装、本机索引、卸载/升级/回滚、离线/在线仓库、CLI 安装同意**;
不包含桌面确认框、官方 keyring 与分布式安装锁(见 §6)。

## 2. 签名格式

签名覆盖 Artifact 摘要,而不是目录(运行的是 Artifact):

```
原文 = "ada-plugin-v2\n" + <plugin.json 原文> + "\n" + <artifact digest>
签名 = ed25519(privateKey, 原文)
```

`plugin.sig.json`:

```json
{ "keyId": "official-2026", "algo": "ed25519", "signature": "<base64>" }
```

- 只接受 `ed25519`(缺省 algo 视为 ed25519;其它算法拒绝)。
- 密钥来自调用方提供的 key ring(`keyId → {public, trust}`);未知 keyId → 拒绝。
- **篡改清单或 Artifact 任一项都会验签失败**(摘要与清单原文都在原文里)。

## 3. 信任等级

| 情况 | 信任 |
|---|---|
| ts + 有效签名 | 密钥声明的 trust(`verified` / `official`;`builtin` 应只出现在随二进制编译的插件) |
| ts + 未签名(未要求签名) | `third-party` |
| ts + 未签名(要求签名) | 拒绝 |
| go 插件 | `builtin`(Go 代码编译进宿主,没有 Artifact 可签) |

- Go 插件目录里出现 `plugin.sig.json` → 拒绝(签名只针对 Artifact)。
- `trust` 值来自密钥文件,不来自清单自述。

## 4. 安装语义

`install.Install(src, destRoot, opts)`:

1. `VerifyDir`:清单 → Artifact 摘要 → 验签;
2. 在 `destRoot` 下建临时目录`.install-*`;
3. 复制目录:拒绝符号链接,限制文件数(默认 1000)与总字节(默认 16 MiB);
4. `rename` 到 `<destRoot>/<id>@<version>/`;
5. 目标已存在 → 拒绝覆盖(更新必须走新版本目录);
6. 任一步失败 → 删除临时目录,既有目录不受影响。

## 5. CLI

```powershell
go run ./cmd/ada verify plugins/tool/git-helper
# 插件: git-helper@0.1.0(ts)
# 摘要: sha256:...
# 信任: third-party
# 已签名: false

go run ./cmd/ada verify <目录> -keys keys.json -require-signature
go run ./cmd/ada install <目录> -dest <安装根> -keys keys.json -require-signature -yes
go run ./cmd/ada list -dest <安装根>
go run ./cmd/ada uninstall un-demo -dest <安装根>
go run ./cmd/ada upgrade <新版本目录> -dest <安装根> [-yes] [-keep]
go run ./cmd/ada rollback <插件 id> -dest <安装根>
go run ./cmd/ada update <插件 id> -repo <仓库目录> |-repo-url <索引 URL> -dest <安装根> [-yes]
go run ./cmd/ada repo <仓库索引 URL> [-keys 密钥文件] [-require-repo-signature]
```

`keys.json` 格式:

```json
{ "official-2026": { "public": "<base64>", "trust": "official" } }
```

退出码:0 成功 / 1 校验或安装失败 / 2 用法错误(含未确认安装)。
`uninstall` 删除该 id 的**全部版本**并清理索引;只有索引登记、且位于安装根之下的目录才会被删除。
`upgrade` 校验新版本 semver **高于**已装版本 → 安装新版本 → 删除旧版本目录,索引收敛为单条最新记录;拒绝降级与同版本重复。
`-keep` 保留旧版本作为回滚点;`rollback` 删除当前最新版本并把索引回退到次新版本(需要先 `-keep` 升级过)。
`repo` 通过 HTTP 读取**在线仓库索引**(`repo.json`,上限 1 MiB/20s),可选验签同目录 `repo.sig.json`;只读索引,**不下载插件**。
`-repo-url` 让 `update` 走**在线仓库**:索引条目可带 `source`(artifact zip URL),下载→安全解包(拒绝路径逃逸/符号链接,16 MiB/1000 文件上限)→校验摘要→`Upgrade`。`repo.json` 可用 `repo.sig.json` 签名(Ed25519,消息 `ada-repo-v2\n<索引原文>`);`-require-repo-signature` 要求仓库必须带签名。`repo.json` 列出 `{id,version,dir,digest}`,按 semver 选最高版本、校验摘要后复用 `Upgrade`;已是最新/降级/摘要不匹配/目录逃逸都拒绝。
`-yes` 是**安装同意**:插件声明了 `caps` 时,没有 `-yes` 会先展示 id/版本/信任/能力并拒绝安装(退出码 2)。

## 6. 偏差与边界

1. **本机索引/卸载/升级/回滚/离线/在线更新 + 安装锁已实现**:同一 `destRoot` 上的 platform/install/uninstall/upgrade/rollback 由 `.install.lock` 串行化(锁被占用即失败);锁超过 `InstallLockTTL`(10 分钟)视为崩溃残留并回收。artifact 不单独签名,靠索引签名+digest。
2. **安装同意只有 CLI 级**:`-yes` 让 `ada install` 在展示能力后继续;桌面确认框/逐项授权未实现,`-yes` 是一次性整体同意。
3. **没有 `$ADA_HOME/keys` 与官方 keyring**:key ring 由调用方显式提供。
4. **安装 trust 已进入 caps 授权表**:`install.LoadDir` 按 `index.json` 选出每个 id 的最新版本,
   并用索引 trust 覆盖清单自述;CLI/server 识别安装根目录后交给
   `builtins.ApplyAutoGrants` 灌进 Broker。直接绕开安装根、从普通磁盘目录加载仍按
   `third-party` 处理。
5. **摘要只覆盖 Artifact 文件内容**:目录级元数据(权限、空目录)不参与签名。
6. **安装锁是进程级文件锁**:`.install.lock` + `InstallLockTTL` 能挡住同一 `destRoot` 的并发安装,但不是分布式锁;TTL 误回收仍可能。
7. **Windows 长路径/保留名**未处理:安装路径长度与保留名未显式校验。

## 7. 测试矩阵

| 场景 | 用例 |
|---|---|
| 合法签名 → verified,摘要一致 | `install.TestSignedVerified` |
| 清单篡改 → 验签失败 | `install.TestTamperedManifest` |
| 错误密钥 / 未知 keyId | `install.TestUnknownKeyAndBadSignature` |
| 未签名 third-party;要求签名则拒绝 | `install.TestUnsigned` |
| Go 插件 builtin,不接受 Artifact 签名 | `install.TestGoPluginBuiltin` |
| 原子安装、拒绝覆盖 | `install.TestInstallAtomic` |
| 符号链接拒绝 | `install.TestRejectsSymlink`(平台不支持时跳过) |
| 文件数/总大小上限 | `install.TestInstallLimits` |
| CLI:verify 能力清单、install 同意(-yes) | `platform/host/cli.TestVerifyCaps`、`platform/host/cli.TestInstallConsent` |
| 索引写入与 List / 空索引 / 索引损坏 | `install.TestIndexWrittenAndListed`、`TestListEmpty`、`TestListCorrupt` |
| CLI list:安装后条目、空列表 | `platform/host/cli.TestList`、`platform/host/cli.TestListEmpty` |
| 卸载:删除全部版本 + 清理索引 / 未知 id / 索引越界拒绝 | `install.TestUninstall`、`TestUninstallUnknown`、`TestUninstallRejectsOutside` |
| CLI uninstall:卸载后列表为空 / 未知 id | `platform/host/cli.TestUninstall`、`platform/host/cli.TestUninstallUnknown` |
| 升级:清理旧版本 + 索引只剩最新 | `install.TestUpgrade` |
| 升级拒绝:未安装/同版本/降级 | `install.TestUpgradeRejects` |
| CLI upgrade:旧目录清理 + list 最新 | `platform/host/cli.TestUpgrade` |
| KeepPrevious 保留旧版本 / Rollback 回退 / 单版本拒绝 | `install.TestUpgradeKeepPrevious`、`TestRollback`、`TestRollbackNoPrevious` |
| CLI upgrade -keep + rollback / 单版本回滚失败 | `platform/host/cli.TestRollback`、`platform/host/cli.TestRollbackNoPrevious` |
| 仓库 Latest 选版 | `install.TestRepoLatest` |
| UpdateFromRepo 更新并清理旧版 | `install.TestUpdateFromRepo` |
| 已是最新/摘要不匹配/目录逃逸 | `install.TestUpdateFromRepoRejects` |
| CLI update 成功/已是最新 | `platform/host/cli.TestUpdateFromRepo` |
| 仓库签名:往返/缺签名/篡改/固定 keyring | `install.TestRepoSignature`、`TestUpdateFromSignedRepo` |
| CLI update -require-repo-signature 缺签名拒绝 | `platform/host/cli.TestUpdateRequiresRepoSignature` |
| 在线索引:未签名可读/require 缺签名拒绝 | `install.TestFetchRepoIndex` |
| 在线索引验签:通过/篡改拒绝 | `install.TestFetchRepoIndexSigned` |
| CLI repo:读取/require 缺签名 | `platform/host/cli.TestRepo` |
| 下载+解包+校验 | `install.TestFetchArtifactExtract` |
| zip slip/非法 scheme 拒绝 | `install.TestArtifactRejects` |
| 在线更新(索引→artifact→Upgrade) | `install.TestUpdateFromRemote` |
| CLI update -repo-url 在线更新 | `platform/host/cli.TestUpdateRemote` |
| 安装锁:占用拒绝/结束后清理 | `install.TestInstallLock` |
| 过期锁回收 | `install.TestInstallLockStale` |
| 安装根按索引 trust 加载 | `install.TestLoadDirUsesInstallTrust` |

## 8. 下一步

1. `$ADA_HOME/keys` 与 keyring;签名发布工具(生成 `plugin.sig.json`)。
2. 分布式安装锁与 Windows 长路径。
3. 桌面确认框/逐项授权(替代 CLI 的 `-yes` 整体同意)。
4. 安装根目录的 `plan`/`inspect` 展示 trust 来源与 caps 自检。

## 9. 验证命令

```powershell
go test -race -count=1 ./install
go run ./cmd/ada verify plugins/tool/git-helper
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```
