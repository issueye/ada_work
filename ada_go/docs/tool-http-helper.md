# http-helper:HTTP 请求与网页搜索工具(TS 实现)

`plugins/tool/http-helper` 属于 **tool 插件**,用 **TypeScript** 实现,给模型两组 HTTP 能力:
`http_request`(一次 HTTP(S) 请求)与 `http_search`(搜索引擎聚合:DeepSeek 网页搜索 /
DuckDuckGo / SearXNG)。启用它等于「允许模型对外发请求」——是否装这个工具由 preset 决定,
本插件不做网络策略。

Go 版本不存在:插件目录只有 `plugin.json` 与 `plugin.ts`,由 esbuild 编译、moejs 执行。

## 配置

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `maxTimeoutSeconds` | 30,上限 120 | 单次请求/搜索允许的最长秒数;工具参数可再缩短 |
| `maxBytes` | 1 MiB,上限 4 MiB | 响应体字节上限(与宿主 `httpDo` 的 4 MiB 一致) |
| `maxRedirects` | 5,上限 20 | 跟随重定向的最多次数;工具参数可再缩短 |
| `userAgent` | `ada-go/http-helper` | 没有显式 `User-Agent` 时补上的值 |
| `allowInsecure` | `false` | 是否允许调用方用 `insecure: true` 跳过 https 证书校验 |
| `searchProvider` | `auto` | `auto`/`deepseek`/`duckduckgo`/`searxng`;`auto` = 有 DeepSeek key 用它,否则 DuckDuckGo |
| `deepseekEndpoint` | `https://api.deepseek.com/v1/tools/search` | DeepSeek 网页搜索端点(测试可指向本地 fake) |
| `deepseekKeySecret` | `DEEPSEEK_API_KEY` | secret **名字**(不是 key 本身);经 `ada:secrets` 读取 |
| `duckduckgoEndpoint` | `https://html.duckduckgo.com/html/` | DuckDuckGo HTML 端点,无需 key |
| `searxEndpoint` | 空 | 自建 SearXNG 的根地址,例如 `http://127.0.0.1:8888`;空表示不可用 |
| `maxResults` | 5,上限 10 | `http_search` 默认返回条数 |

## 工具 `http_request`

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `url` | string,必填 | 完整地址,必须带 `http://` 或 `https://` |
| `method` | string | 默认 `GET`;常用 `GET`/`POST`/`PUT`/`PATCH`/`DELETE`/`HEAD` |
| `headers` | object | 请求头键值对;数组值按 `, ` 合并 |
| `query` | object | 附加到 URL 的查询参数;数组值展开成重复键 |
| `body` | string \| object | 字符串原样发送;对象/数组默认按 JSON 序列化 |
| `bodyType` | `json` \| `text` \| `form` | 请求体编码;省略时按 `body` 类型推断 |
| `timeoutSeconds` | integer | 本次请求超时(不超过配置上限) |
| `followRedirects` | integer | 本次跟随重定向次数(不超过配置上限) |
| `maxBytes` | integer | 本次响应体上限(不超过 4 MiB) |
| `insecure` | boolean | https 跳过证书校验;仅当 `allowInsecure=true` 时可用 |

行为约定:

- **重定向**:`301`/`302`/`303`/`307`/`308` 且带 `Location` 时跟随,上限 `followRedirects`;
  `followRedirects=0` 表示**不跟随**,把 3xx 原样交给模型。`303` 与 `301`/`302` 上的非 `GET`/`HEAD`
  请求按浏览器语义降级为 `GET` 并丢弃请求体与 `Content-Type`/`Content-Length`;超过上限直接报错,
  而不是静默返回 3xx。
- **压缩**:默认补 `Accept-Encoding: gzip, deflate`(调用方显式给出时不覆盖),响应按
  `Content-Encoding` 用 `node:zlib` 解开;`br`/`zstd` 不支持,会明确报错而不是静默返回乱码。
- **截断**:正文超过 `maxBytes` 时宿主置 `truncated`,工具在正文前标注;压缩响应被截断时
  无法解压,会返回说明而不是半个乱码。
- **状态码不是错误**:任何已完成的响应(含 4xx/5xx)都返回 `StatusOK`,状态码放进正文与
  `Details`;只有连不上、超时、URL 非法、编码不支持这类才返回 `StatusError`。
- **可见正文**上限 64 KiB(UTF-8 安全截断),与 `shell-helper` 同口径。

正文形态:

```text
200 OK  GET https://api.example.com/v1/users  (application/json, 142 B)
---
{"items":[{"id":1}]}
```

`Details` 字段:`url`、`method`、`status`、`statusText`、`headers`(小写键)、`bytes`、
`durationMs`、`truncated`、`redirects`(跳转链)、`encoding`。

## 工具 `http_search`

| 参数 | 类型 | 说明 |
| --- | --- | --- |
| `query` | string,必填 | 搜索词;支持 `site:` / `-site:` / 引号等引擎语法 |
| `provider` | `auto` \| `deepseek` \| `duckduckgo` \| `searxng` | 默认取配置 `searchProvider` |
| `maxResults` | integer | 1–10,默认取配置 `maxResults` |
| `site` | string | 限定站点,拼成 `site:<域名>` |
| `oriQuestion` | string | 触发搜索的原始问题(仅 DeepSeek) |
| `timeoutSeconds` | integer | 本次超时,不超过配置上限 |

引擎对照:

| provider | 请求 | key | 结果来源 |
| --- | --- | --- | --- |
| `deepseek` | `POST <endpoint>` `{query,count,ori_question,localization,need_content:false}`,`Authorization: Bearer <secret>` | 需要(`deepseekKeySecret`) | `results[]`:title/url/snippet/summary/site_name/date/rerank_score |
| `duckduckgo` | `GET <endpoint>?q=` HTML | 不需要 | `result__a` + `result__snippet`,`uddg=` 跳转链接还原 |
| `searxng` | `GET <searxEndpoint>/search?format=json&q=` | 不需要 | `results[]`:title/url/content |

输出形态:

```text
搜索 "ada go 插件"  (provider: deepseek, 2 条, 231 ms)
1. Ada Go 插件
   https://example.com/ada
   Example · 2026-01-01T00:00:00Z
   一段短摘要
```

`Details`:provider、query、count、durationMs、results(含 title/url/snippet/summary/date/site/score)。

约定与边界:

- **key 只按名取**:`http_search` 调 `ada:secrets.get(<deepseekKeySecret>)`,从不把 key 写进配置、日志或工具结果;缺 secret 时显式报错并给出配置指引,**不会静默换成别的引擎**。
- `provider=auto` 用 `ada:secrets.has(...)` 判断「有没有 key」:没有 secrets 服务或没有 key 时退到 DuckDuckGo,默认装配即可用。
- DuckDuckGo 走 **HTML 结果页**,执行层没有 HTML 解析器,按 `result__a` / `result__snippet` 两个标记切块。**这是脆弱契约**:DDG 改版时会退化成「0 条」而不是乱码;要稳定结果请用 DeepSeek 或自建 SearXNG。
- `need_content` 固定 `false`:正文按次计费且撑大上下文;要正文时让模型用 `http_request` 抓具体链接。
- 搜索是只读工具(`readOnly: true`),但它会产生外部请求与第三方计费;是否启用仍由 preset 决定。
- 频率限制与条款由各引擎负责;本插件不重试、不做背压。

DeepSeek 搜索需要 key。**发布 preset 默认已装好这条链**:`seam-secrets` + `secrets-env`
(`allow` 只含 `DEEPSEEK_API_KEY`)随 `agent-release` 启用,`secrets-guard` 保持关闭(它要 principal,
CLI/桌面单机场景没有身份可查,开了反而会把读取拒掉)。你只需要把值放到运行环境里:

```dotenv
# 包根 .env,或机器人的「环境变量」页
ADA_SECRET_DEEPSEEK_API_KEY=sk-...
```

之后 `http_search` 的 `provider=auto` 会检测到 key 并走 DeepSeek;没有 key 才退回 DuckDuckGo
(DuckDuckGo 在部分网络下不可达,会表现为连接超时)。想显式固定引擎就在 preset 里写
`"http-helper": {"searchProvider": "deepseek"}`。

> 注意:不要挂 `secrets-guard`——它要求 `kernel.Principal(ctx)` 与 `secrets.<name>` 能力,
> 没有 principal 时会直接拒绝;发布 preset 因此只启用 provider 的白名单。
> 需要按身份收紧时,由宿主注入 principal 后再启用它。

## 为什么需要补内置库

`node:http` 只提供「一次请求、回调同步投递」的最小客户端
(见 [node-compat.md](node-compat.md) §5):不跟随重定向(与 Node 一致)、不解压、
不提供 `fetch`,也**没有 `node:zlib`**。搜索则多一个缺口:JS 侧原本没有任何
**按名读取 secret** 的路径,而 DeepSeek 需要 API key,又不能在 config/日志里出现。
所以先补执行层,再写插件,而不是在 TS 里绕:

| 扩展点 | 位置 | 作用 |
| --- | --- | --- |
| `__adaHost.zlib(op, base64)` | `ada_jsrt/jsrt/nodezlib.go` | 同步 gzip/deflate/raw-deflate 的压缩与解压,输出上限 8 MiB |
| `node:zlib` | `ada_jsrt/jsrt/nodemods/src/zlib.js` | `gzipSync`/`gunzipSync`/`deflateSync`/`inflateSync`/`deflateRawSync`/`inflateRawSync`/`unzipSync` |
| `__adaHost.secretsGet(name)` | `ada_jsrt/jsrt/secrets.go` | 按名读 secret,透传调用级 ctx(`secrets-guard` 从 ctx 取 principal) |
| `ada:secrets` | `platform/bridge/plugin.go` | `get(name)` 读 secret、`has(name)` 只回答能否读到(不给值) |
| HTTP 重定向与解压 | `plugins/tool/http-helper` | 组合上面两者,不新增 seam |

`node:zlib` 的解压输出有上限(`maxZlibOutBytes`,8 MiB):压缩比可以做到上万倍,
不设上限等于把 zip bomb 交给插件的 heap。输入上限同为 8 MiB。
流式接口(`createGzip`/管道)、brotli 与 zip 归档不在本层承诺内。

## 响应清洗(非结构化数据)

`http_request` 默认把 HTML 先清洗成纯文本再给模型,避免模板、脚本、`meta`、统计代码把上下文淹没:

- 丢弃注释、`<script>`、`<style>`、`<noscript>`、`<iframe>`、`<svg>` 与整个 `<head>`;
  页面 `<title>` 保留为第一行 `标题: …`;
- `<br>` 与块级元素(`p`/`div`/`li`/`tr`/`h1..h6`/`pre` 等)转换成换行,其余标签去掉;
- 解 HTML 实体(`&amp;`/`&nbsp;` 等),压缩行内空白与连续空行;
- 结构化类型(`application/json`、`text/plain`、XML/CSV)原样返回;二进制(图片/压缩包等)
  不再硬转 UTF-8,只报类型与大小;
- 非 UTF-8 页面(`charset=gbk` 等)会在开头提示"按 UTF-8 解码可能出现乱码"——转码不在本插件范围内。

调用时传 `clean: false` 可拿原文;结果 `details` 里带 `cleaned`(是否清洗过)与
`kind`(`html`/`text`/`binary`),便于模型或调试判断。`http_search` 本身只返回归一化后的
标题/链接/摘要,不走这条清洗。
## 安全边界

- 网络**没有** seam、caps 或审批:按 [ADR 0011](adr/0011-node-network-direct.md) /
  [ADR 0012](adr/0012-remove-seam-net.md),安全边界只约束文件与写入,
  因此 `node:http`/`https` 直连标准库。启用 `http-helper` 就是把这能力交给模型,
  没有第二道闸门。
- 是否启用由 preset 决定:**发布 preset(`agent-release` / `agent-release-http`)默认启用**,
  模型因此默认拥有联网工具;不想给网络能力时,在 preset 的 `disable` 里加回
  `"http-helper"`(或从 `plugins` 里移除它)。它随包发布,不需要重新编译。
- 请求体与响应体都受 JSON 边界上限约束(4 MiB);超时、取消由调用级 `ctx` 中断在途请求,
  插件卸载同样会中断。
- `insecure` 默认关闭:httptest 之外的真实端点应使用有效证书;打开它必须由宿主配置
  `allowInsecure=true` 显式允许。
- **secret 不落配置**:DeepSeek key 只以名字出现,读值经 `seam/secrets` 链。
  没有 `secrets-env` 的白名单(或 `secrets-guard` 的 `secrets.<name>` 授权)就读不到;
  插件输出、`Details` 与错误信息里都不含 key。
- `ada:secrets` 对**所有** TS 插件可导入(与 `ada:fs`/`ada:shell` 同级的注入模块),
  因此同一进程里的其它脚本插件也能读到白名单内的 secret;真正的边界是
  `secrets-env` 的白名单与 `secrets-guard` 的 principal 授权,不是「插件不知道这个模块」。

## 示例

仓库里有一个只含装配的示例(复用 agent 示例的 Go 清单,不复制插件目录):

```powershell
go run ./cmd/ada plan --preset examples/http/http.preset.json
go run ./cmd/ada up   --preset examples/http/http.preset.json
```

`examples/http/http.preset.json` 的内容:

```json
{
  "name": "http",
  "plugins": ["../agent/plugins", "../../plugins/tool/http-helper"],
  "disable": ["llm-openai", "session-jsonl"],
  "patch": {"agent-loop": {"model": "replay"}}
}
```

它只证明装配(TS 现场编译 + `http-helper` active),不带回放脚本:真实调用需要一个可访问的
目标地址,不适合放进离线测试。接真实模型时,让模型调用一次 `http_request`:

```text
GET https://example.com/  →  200 OK,正文为 HTML 前 64 KiB
```

## 测试

```powershell
go test ./platform/bridge -run 'NodeZlib|JSToolHTTP'
(cd ../ada_jsrt; go test ./jsrt -run 'Node')
```

覆盖:

- `TestNodeZlib`:`gzipSync`/`deflateSync`/`deflateRawSync` 的往返、`unzipSync`,
  以及「JS 产出的 gzip 能被 Go 的 `compress/gzip` 解开」——证明压缩不是伪造的近似实现;
- `TestJSToolHTTPHelper`:本地 `httptest` 服务上的 GET(自定义头)、POST JSON 与 form、
  重定向跟随、gzip 解压、响应截断、连接失败返回 `StatusError`;
- `TestJSToolHTTPHelperRejectsBadArgs`:缺 `url` 时由 tools seam 的 schema 校验拒绝(`ESCHEMA`);
- `TestSearchDeepSeek`:`Authorization` 来自 secret、请求体形状、结果归一化、**输出不含 key**;
- `TestSearchDeepSeekWithoutSecret`:显式 `deepseek` 但没有 secret 时 fail-closed 并给出配置指引;
- `TestSearchDuckDuckGo`:HTML 实体解码、`uddg=` 跳转链接还原、标签清理、第二条结果;
- `TestSearchAutoFallsBackToDuckDuckGo`:无 secrets 服务时 `auto` 退到 DuckDuckGo;
- `TestSearchSearXNG`:自建实例的 `format=json` 接口。
