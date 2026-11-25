# platform/host/registry(只读在线仓库服务端)

> 配套 [install.md](install.md)(在线索引与 artifact 下载)与 AGENTS §2.2(宿主适配器)。

## 1. 范围

```
platform/host/registry/   从本地目录提供 repo.json / repo.sig.json / artifacts/*.zip
```

它只做 HTTP 服务端映射与路径约束;仓库内容(索引/签名/zip)由发布流程生成。

## 2. 路由

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/repo.json` | 仓库索引 |
| GET | `/repo.sig.json` | 索引签名(可选) |
| GET | `/artifacts/<name>.zip` | artifact zip |
| GET | `/` | 路由清单 |

- 非 GET → 405;文件不存在 → 404;路径逃逸/绝对路径/反斜杠 → 400;
- `Options.Token` 非空时要求 `Authorization: Bearer <token>`(常量时间比较)。
- `Options.RateLimit` > 0 时固定窗口限流(每秒),超限 429 + `Retry-After`;`Options.Now` 可注入。
- `Options.AllowOrigin` 非空时启用 CORS(OPTIONS 预检 204)。

## 3. 与客户端配对

- 客户端 `ada repo <url>/repo.json` 读索引并验签;
- `ada update ... -repo-url <url>/repo.json` 下载 `source` 指向的 zip;
- 索引条目里的 `source` 必须指向本服务的 `/artifacts/*.zip`。

## 4. 测试

- `registry.TestServe`:索引/签名/artifact 内容;
- `registry.TestAuth`:缺/正确 token;
- `registry.TestResolveRejects`:绝对路径/`..` 拒绝(含 POSIX 风格)。

### 4.1 Serve 与 CLI

- `registry.Serve(ctx, opts, ln)` 在给定 listener 上服务,ctx 取消时 `Shutdown` 并返回 nil;
- `ada serve-repo -root <目录> -addr <地址> [-token] [-rate-limit N] [-cert 证书 -key 私钥]`:监听并打印地址,SIGINT 优雅停止,可 TLS;
- 测试 `TestServeGraceful` 用 `:0` 监听 + 请求 + 取消验证退出。

## 5. 偏差与边界

1. **只读**:没有上传/删除/发布/签名生成(签名由离线工具生成)。
2. **TLS/限流/CORS 可选**:`CertFile/KeyFile` 成对时走 `ServeTLS`;`RateLimit` 为单实例固定窗口;CORS 只设置允许源。
3. **无目录索引**:不列出 artifacts;客户端必须知道文件名。
4. **路径限制是词法 + 符号链接解析**:服务端启动时解析 root 一次,运行中新增符号链接可能绕过(未做连接级校验)。
5. **没有缓存/范围请求**(`http.ServeFile` 自身支持 Range)。

## 6. 下一步

1. ~~`ada serve-repo` 命令~~ 已实现;下一步是发布工具与鉴权 scope。
2. 发布工具(生成索引/签名/zip);
3. 鉴权 scope 与审计日志。

## 7. 验证命令

```powershell
go test -race -count=1 ./platform/host/registry
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```