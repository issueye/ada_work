# platform/host/server(只读 HTTP 适配器)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §12(宿主)与 AGENTS §2.2(宿主适配器)。

## 1. 范围

```
platform/host/server/   只读 HTTP API:plan / caps / inspect / verify / list
```

它只做协议映射与路径约束,不做业务;所有查询都必须落在 `Options.Root` 之下。

## 2. 路由

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/plan?path=<相对路径>` | 插件目录或 `.preset.json` → 依赖顺序/问题(JSON) |
| GET | `/api/caps?path=<相对路径>` | 清单 caps/trust → `GrantManifests` 静态检查(JSON) |
| GET | `/api/list?dest=<相对路径>` | 读取安装索引(JSON) |
| GET | `/api/inspect?path=<相对路径>` | 临时挂载内置 Go 插件并返回快照(会执行副作用,仅本地) |
| GET | `/api/verify?path=<相对路径>` | 对插件目录做 VerifyDir(清单/摘要/编译/信任/签名) |
| GET | `/` | 列出可用路由 |

- 非 GET → **405**;
- 绝对路径、`..` 逃逸、符号链接指向根外 → **400**;
- `Options.Token` 非空时要求 `Authorization: Bearer <token>`(常量时间比较);空 Token 表示不鉴权,此时只应绑定 localhost 或由外层网关保护。
- `Options.RateLimit` > 0 时固定窗口限流(每秒),超限返回 429 + `Retry-After: 1`;`Options.Now` 可注入时钟。
- `Options.CertFile/KeyFile` 成对提供时 `Serve` 使用 TLS(`ServeTLS`);只给一个会报错。
- `Options.AllowOrigin` 非空时启用 CORS(响应带 `Access-Control-Allow-Origin`,OPTIONS 预检 204)。

## 3. 内置插件与 inspect

`/api/inspect` 使用共享的 `platform/host/builtins` 注册表(与 platform/host/cli 同一份内置 Go 插件),并在挂载前 `ApplyAutoGrants`。
它**会执行插件副作用**(打开文件、注册服务、启动 goroutine),因此只适合本地开发/诊断,文档明确不应暴露到公网。

## 4. 安全约束

`resolve` 做三层检查:相对路径 + lexical 越界 + `EvalSymlinks` 后的越界(防止符号链接逃逸)。
根目录在 `NewHandler` 时解析为绝对路径并解析符号链接一次。

## 4.1 Serve 与 CLI

- `server.Serve(ctx, opts, ln)` 在 listener 上服务,ctx 取消时 `Shutdown` 并返回 nil;
- `ada serve -root <目录> -addr <地址> [-token] [-rate-limit N]`:监听并打印地址,SIGINT 优雅停止;

## 5. 测试

`platform/host/server.Test*` 用 `httptest` 直接打 handler:

- plan 顺序、caps 授权、路径逃逸 400、POST 405、list 索引、根路由、inspect 内置栈快照、verify 插件、Token 认证、限流、Serve 优雅退出、TLS 服务、CORS;
- 不启动真实端口,不依赖网络。

## 6. 偏差与边界

1. **基本只读**:`/api/inspect` 会临时挂载(有副作用)后立即停止;没有长期 app 生命周期与审批/执行接口。
2. **有 Token/限流/TLS/CORS(可选)**:证书文件由调用方提供;没有签发/轮换/ACME;限流仍是单实例语义;CORS 只设置允许源,不做 Origin 白名单匹配。
3. **root 内任意目录可读**:没有按插件/用户的能力过滤。
4. **没有分页/流式**:结果一次性 JSON;大插件集另说。
5. **没有长期 app 生命周期**:`/api/inspect` 每次临时挂载后停止。

## 7. 下一步

1. 长期 app 生命周期与执行接口(当前只做只读/临时挂载);
2. SSE/流式输出与分页;
3. 按插件/用户的能力过滤;
4. desktop 宿主(mygo)共享同一套路由语义。

## 8. 验证命令

```powershell
go test -race -count=1 ./platform/host/server
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```