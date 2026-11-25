# policy/audit-log(审计落盘)

> 配套 [seam-fs.md](seam-fs.md)/[seam-shell.md](seam-shell.md)/[seam-approval.md](seam-approval.md) 的事件契约。

## 1. 范围

```
plugins/policy/audit-log/   把观察/审批事件追加为 JSONL 文件
```

它做「落盘 + 大小轮转 + payload 通用脱敏」:不上报、不阻断。

## 2. 事件

| 事件 | 来源 |
|---|---|
| `fs/observe` | `policy/fs-observe` |
| `shell/observe` | `policy/shell-observe` |
| `approval/requested` | `seam/approval` 插件 |
| `approval/decided` | fs/shell 策略(审批结果) |
| `caps/deny` | `caps.Broker` 拒绝回调(经 `caps-static`/策略 fallback 发事件) |

## 3. 配置与格式

```json
{ "path": "审计文件路径(必填,父目录需存在)", "maxBytes": 0, "keep": 2 }
```

每行:

```json
{"ts":"2026-...Z","event":"fs/observe","payload":{...}}
```

- 追加写,`0600` 权限;`maxBytes>0` 时按大小轮转(`path → path.1 → path.2 ...`),保留 `keep` 个历史文件(默认 2);
- 互斥锁保护多事件并发写;
- 写入失败被忽略(审计不阻断业务),但会丢失记录(见边界)。

## 4. 测试

- `auditlog.TestWrites`:单事件写成一行、payload 完整;
- `auditlog.TestAppendsMultiple`:三类事件追加三行;
- `auditlog.TestMissingPath`:缺 `path` 插件失败;
- `auditlog.TestApprovalDecided`:`approval/decided` 落盘;
- `auditlog.TestCapsDeny`:`caps/deny` 落盘且 URL target 已脱敏;
- `auditlog.TestRedactsPayload`:敏感 key/URL query 通用脱敏;
- `auditlog.TestRotation`:小阈值触发轮转、keep 保留正确;
- `auditlog.TestRotationInvalid`:负 maxBytes 失败。

## 5. 偏差与边界

1. **payload 通用脱敏**:敏感 key(`authorization`/`token`/`secret`/`password` 等)置
   `REDACTED`;URL 类 key 与文本中的 http(s) URL 去掉 userinfo/fragment/query。
   非 URL 的路径/工具名保持原样。
2. **有大小轮转、无按天轮转/压缩**:`maxBytes` 达到后移位并丢弃最旧;没有保留期与压缩。
3. **写入失败静默**:磁盘满/权限错误不会阻断业务,也没有错误上报。
4. **没有 fsync**:崩溃可能丢最后几行。
5. **父目录必须存在**:不自动 MkdirAll。
6. **没有远程上报/标准化 schema**:只是本地 JSONL。

## 6. 下一步

1. 按天轮转与保留期(按大小轮转已实现);
2. `fsync` 策略与失败计数;
3. 标准审计 schema(会话 id/调用链)与远程 sink。

## 7. 验证命令

```powershell
go test -race -count=1 ./plugins/policy/audit-log
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```
