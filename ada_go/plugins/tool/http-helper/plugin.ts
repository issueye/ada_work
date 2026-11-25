// http-helper 的 TS 实现:给模型两组 HTTP 工具——`http_request`(一次请求)与
// `http_search`(搜索引擎聚合)。
//
// 安全边界不在本插件里。网络模块直连宿主标准库(ADR 0011/0012):没有 seam、
// 没有 caps、也没有审批,因此**是否让模型碰网络由 preset 决定**(把 plugins 目录里
// 的 http-helper 启用才会出现这些工具)。本插件只做四件事:
//   1. 参数校验与请求体编码(json / text / form);
//   2. 有界重定向(301/302/303/307/308),按浏览器的语义降级方法与请求体;
//   3. 按 content-encoding 解压(gzip / deflate / raw deflate),走 node:zlib;
//   4. 把结果整理成模型可读的文本 + 结构化 details,并截断超大正文。
//
// `http_search` 只做「适配器」:每个引擎一条请求 + 一次结果归一化。需要 API key 的
// 引擎(DeepSeek 网页搜索)按 secret **名字**取 key(`ada:secrets`),不把 key 放进
// 配置、日志或工具结果;提取不到就明确报错,不静默降级。
import http from "node:http"
import https from "node:https"
import zlib from "node:zlib"
import { URL, URLSearchParams } from "node:url"
import { get as getSecret, has as hasSecret } from "ada:secrets"

type ToolSpec = {
  name: string
  summary?: string
  input?: Record<string, unknown>
  readOnly?: boolean
  timeoutMs?: number
}

type ToolResult = {
  status?: "ok" | "denied" | "error" | "timeout"
  content?: string | Array<{ kind?: string; text: string }>
  details?: unknown
  terminate?: boolean
}

type Ctx = {
  log: (msg: string) => void
  tool: (spec: ToolSpec, handler: (args: any) => ToolResult) => void
}

type Config = {
  maxTimeoutSeconds?: number
  maxBytes?: number
  maxRedirects?: number
  userAgent?: string
  allowInsecure?: boolean
  // ——— http_search ———
  searchProvider?: string
  deepseekEndpoint?: string
  deepseekKeySecret?: string
  duckduckgoEndpoint?: string
  searxEndpoint?: string
  maxResults?: number
}

type RequestArgs = {
  url?: string
  method?: string
  headers?: Record<string, unknown>
  query?: Record<string, unknown>
  body?: unknown
  bodyType?: string
  timeoutSeconds?: number
  followRedirects?: number
  maxBytes?: number
  insecure?: boolean
}

type Response = {
  status: number
  statusText: string
  headers: Record<string, string>
  body: Uint8Array
  truncated: boolean
  url: string
  redirects: string[]
}

const MAX_VISIBLE_BYTES = 64 << 10
// ABS_MAX_BYTES 与宿主 __adaHost.httpDo 的响应体上限(4 MiB)保持一致:
// 请求比它更大的数字只会被宿主静默截到上限,不如在工具层显式报错。
const ABS_MAX_BYTES = 4 << 20
const REDIRECT_CODES = [301, 302, 303, 307, 308]

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

// bufferOfBytes 把裸字节包成 Buffer。本运行时的 Uint8Array 没有 toString
// (那是 Node Buffer 的成员),正文转字符串必须经 Buffer.from。
function bufferOfBytes(bytes: Uint8Array): any {
  const B = (globalThis as any).Buffer
  if (!B || typeof B.from !== "function") {
    throw new Error("http-helper 需要 Buffer 支持(node:buffer 未安装)")
  }
  return B.from(bytes)
}

function toolError(text: string): ToolResult {
  return { status: "error", content: text }
}

// utf8Size / utf8Len / truncateUTF8 与 shell-helper 同一套口径:
// 模型看到的正文按字节上限裁剪,而不是按字符数。
function utf8Size(code: number): number {
  if (code < 0x80) return 1
  if (code < 0x800) return 2
  if (code >= 0xd800 && code <= 0xdbff) return 4
  return 3
}

function utf8Len(s: string): number {
  let n = 0
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    n += utf8Size(c)
    if (c >= 0xd800 && c <= 0xdbff) i++
  }
  return n
}

function truncateUTF8(s: string, limit: number): string {
  let n = 0
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    const size = utf8Size(c)
    if (n + size > limit) return s.slice(0, i)
    n += size
    if (c >= 0xd800 && c <= 0xdbff) i++
  }
  return s
}

// clampInt 给配置与调用参数一个统一的有界取值。
function clampInt(value: unknown, def: number, min: number, max: number): number {
  const n = Number(value)
  if (!Number.isFinite(n) || n <= 0) return def
  const floor = Math.floor(n)
  if (floor < min) return min
  if (floor > max) return max
  return floor
}

// clampCount 与 clampInt 的区别是**允许 0**:重定向次数 0 表示不跟随,
// 与「没填就用默认值」是两件事,不能被同一个 0 值吞掉。
function clampCount(value: unknown, def: number, max: number): number {
  if (value === undefined || value === null) return def
  const n = Number(value)
  if (!Number.isFinite(n)) return def
  if (n <= 0) return 0
  return Math.min(Math.floor(n), max)
}

// withQuery 把 query 对象拼到 URL 上;数组值展开成重复键,与 URLSearchParams 一致。
function withQuery(raw: string, query: unknown): string {
  if (!isObject(query)) return raw
  const params = new URLSearchParams()
  for (const key of Object.keys(query)) {
    const value = query[key]
    if (value === undefined || value === null) continue
    if (Array.isArray(value)) {
      for (const item of value) params.append(key, String(item))
    } else {
      params.append(key, String(value))
    }
  }
  const qs = params.toString()
  if (qs === "") return raw
  const hashAt = raw.indexOf("#")
  const base = hashAt >= 0 ? raw.slice(0, hashAt) : raw
  return base + (base.indexOf("?") >= 0 ? "&" : "?") + qs
}

function lowerHeaders(headers: unknown): Record<string, string> {
  const out: Record<string, string> = {}
  if (!isObject(headers)) return out
  for (const key of Object.keys(headers)) {
    const value = headers[key]
    if (value === undefined || value === null) continue
    out[key] = Array.isArray(value) ? value.map((v) => String(v)).join(", ") : String(value)
  }
  return out
}

// bodyOf 把调用参数编码成请求体,并补上对应的 content-type。
// 规则:显式 bodyType 优先;否则对象/数组按 JSON,字符串按原样。
function bodyOf(raw: unknown, bodyType: unknown): { text: string; contentType: string } {
  if (raw === undefined || raw === null) return { text: "", contentType: "" }
  const kind = typeof bodyType === "string" ? bodyType.trim().toLowerCase() : ""
  if (kind === "form") {
    const params = new URLSearchParams()
    if (isObject(raw)) {
      for (const key of Object.keys(raw)) {
        const value = raw[key]
        if (value === undefined || value === null) continue
        if (Array.isArray(value)) {
          for (const item of value) params.append(key, String(item))
        } else {
          params.append(key, String(value))
        }
      }
      return { text: params.toString(), contentType: "application/x-www-form-urlencoded" }
    }
    return { text: String(raw), contentType: "application/x-www-form-urlencoded" }
  }
  if (kind === "json" || (kind === "" && typeof raw === "object")) {
    return { text: JSON.stringify(raw), contentType: "application/json" }
  }
  return { text: String(raw), contentType: "" }
}

function decodeBody(res: Response): Uint8Array {
  const encoding = String(res.headers["content-encoding"] || "").trim().toLowerCase()
  if (encoding === "" || encoding === "identity") return res.body
  if (res.truncated) {
    throw new Error("响应体被截断且是 " + encoding + " 编码,无法解压;请调大 maxBytes 或让服务端不压缩")
  }
  if (encoding === "gzip" || encoding === "x-gzip") return zlib.gunzipSync(res.body)
  if (encoding === "deflate") {
    // HTTP 的 deflate 有两种实现:zlib 容器与裸 flate 流,先按容器解,失败再按裸流。
    try {
      return zlib.inflateSync(res.body)
    } catch {
      return zlib.inflateRawSync(res.body)
    }
  }
  throw new Error("响应编码 " + encoding + " 不受支持(node:zlib 只做 gzip/deflate)")
}

// perform 发一次请求;重定向由调用方处理,这里只保证一次请求拿到完整响应。
function perform(target: URL, method: string, headers: Record<string, string>, body: string, opts: { timeoutMs: number; insecure: boolean; maxBytes: number }): Response {
  const secure = target.protocol === "https:"
  const client = secure ? https : http
  const chunks: Uint8Array[] = []
  let failure: unknown = null
  let status = 0
  let statusText = ""
  let responseHeaders: Record<string, string> = {}
  let truncated = false
  let ended = false
  let bytes = 0

  const req = client.request(target.toString(), {
    method,
    headers,
    timeout: opts.timeoutMs,
    maxBody: opts.maxBytes,
    rejectUnauthorized: opts.insecure ? false : undefined,
  }, (res: any) => {
    status = Number(res.statusCode) || 0
    statusText = String(res.statusMessage || "")
    responseHeaders = res.headers || {}
    truncated = !!res.truncated
    res.on("data", (chunk: any) => {
      const buf = chunk as Uint8Array
      chunks.push(buf)
      bytes += buf.length
    })
    res.on("end", () => {
      ended = true
    })
  })

  req.on("error", (err: unknown) => {
    failure = err
  })
  req.end(body === "" ? undefined : body)
  if (failure !== null) throw failure
  if (!ended && status === 0) throw new Error("请求没有返回响应")

  let merged = new Uint8Array(bytes)
  let offset = 0
  for (const chunk of chunks) {
    merged.set(chunk, offset)
    offset += chunk.length
  }
  return {
    status,
    statusText,
    headers: responseHeaders,
    body: merged,
    truncated,
    url: target.toString(),
    redirects: [],
  }
}

// follow 处理重定向:最多 maxRedirects 次,303 或 301/302 上的非 GET/HEAD 降级为 GET。
function follow(target: URL, method: string, headers: Record<string, string>, body: string, opts: { timeoutMs: number; insecure: boolean; maxBytes: number; maxRedirects: number }): Response {
  let current = target
  let currentMethod = method
  let currentBody = body
  let currentHeaders = headers
  const chain: string[] = []
  for (let hop = 0; ; hop++) {
    const res = perform(current, currentMethod, currentHeaders, currentBody, opts)
    res.redirects = chain.slice()
    const location = res.headers["location"] || ""
    if (REDIRECT_CODES.indexOf(res.status) < 0 || location === "") return res
    // 0 表示「不跟随」:把 3xx 原样交给模型,而不是报错。
    if (opts.maxRedirects <= 0) return res
    if (hop >= opts.maxRedirects) {
      throw new Error("重定向超过上限 " + opts.maxRedirects + " 次(最后跳到 " + location + ")")
    }
    const next = new URL(location, current.toString())
    if (res.status === 303 || ((res.status === 301 || res.status === 302) && currentMethod !== "GET" && currentMethod !== "HEAD")) {
      currentMethod = "GET"
      currentBody = ""
      const dropHeaders = ["content-type", "content-length", "Content-Type", "Content-Length"]
      currentHeaders = { ...currentHeaders }
      for (const key of dropHeaders) delete currentHeaders[key]
    }
    chain.push(current.toString())
    current = next
  }
}

// —— 响应体清洗 ——
//
// 直接把 HTML 原文丢给模型会淹没上下文:模板、脚本、meta、统计代码都算 token。
// 这里只做「可读化」而不是解析 DOM:去注释/脚本/样式/head、块级换行、解实体、压空白,
// 并把页面 <title> 保留在第一行。JSON/纯文本保持原样;二进制不硬转 utf8,只报类型与大小。

function isHTMLType(ct: string): boolean {
	return /text\/html|application\/xhtml\+xml/i.test(ct)
}

function isTextType(ct: string): boolean {
	return isHTMLType(ct) || /^\s*text\/|application\/(json|xml|ld\+json|x-www-form-urlencoded)|text\/(plain|csv|xml)/i.test(ct)
}

function declaredCharset(ct: string): string {
	const m = /charset=([a-z0-9_-]+)/i.exec(ct)
	return m === null ? "" : m[1].toLowerCase()
}

function cleanHTML(html: string): string {
	let s = html
	// 注释、脚本、样式、内嵌页面:一律丢弃。
	s = s.replace(/<!--[\s\S]*?-->/g, " ")
	s = s.replace(/<script\b[\s\S]*?<\/script>/gi, " ")
	s = s.replace(/<style\b[\s\S]*?<\/style>/gi, " ")
	s = s.replace(/<noscript\b[\s\S]*?<\/noscript>/gi, " ")
	s = s.replace(/<iframe\b[\s\S]*?<\/iframe>/gi, " ")
	s = s.replace(/<svg\b[\s\S]*?<\/svg>/gi, " ")
	// 标题先取出来,再丢掉整个 head(meta/css/link 对模型没有用处)。
	const titleMatch = /<title[^>]*>([\s\S]*?)<\/title>/i.exec(s)
	s = s.replace(/<head\b[\s\S]*?<\/head>/gi, " ")
	// 块级元素给出换行,避免所有正文挤成一行。
	s = s.replace(/<br\s*[\/]?>/gi, "\n")
	s = s.replace(/<\/(p|div|section|article|header|footer|main|li|tr|td|th|h[1-6]|blockquote|pre|dd|dt)>/gi, "\n")
	s = s.replace(/<[^>]*>/g, " ")
	s = decodeEntities(s)
	s = s.replace(/\u00a0/g, " ")
	// 压空白:行内折叠为单空格,行首尾 trim,连续空行折叠成一个。
	s = s.replace(/[ \t\f\v]+/g, " ")
	const lines = s.split("\n").map((line) => line.replace(/^\s+|\s+$/g, ""))
	const kept: string[] = []
	for (const line of lines) {
		if (line === "" && (kept.length === 0 || kept[kept.length - 1] === "")) continue
		kept.push(line)
	}
	while (kept.length > 0 && kept[kept.length - 1] === "") kept.pop()
	const body = kept.join("\n")
	const title = titleMatch === null ? "" : decodeEntities(titleMatch[1]).replace(/\s+/g, " ").trim()
	return title === "" ? body : "标题: " + title + "\n" + body
}
// renderedBody 是清洗后的正文与它的类型:html 会去标签,text 原样,binary 只报大小。
type renderedBody = {
	text: string
	kind: string
	cleaned: boolean
}

function responseText(res: Response, method: string, clean: boolean): renderedBody {
	const contentType = String(res.headers["content-type"] || "")
	const encoding = String(res.headers["content-encoding"] || "")
	let kind = isHTMLType(contentType) ? "html" : (isTextType(contentType) ? "text" : "binary")
	const lines: string[] = []
	lines.push(
		String(res.status) + " " + (res.statusText || "") + "  " + method + " " + res.url +
		"  (" + (contentType || "未知类型") + ", " + res.body.length + " B" +
		(encoding !== "" ? ", " + encoding : "") + ")",
	)
	if (res.redirects.length > 0) lines.push("重定向: " + res.redirects.join(" → ") + " → " + res.url)
	if (res.truncated) lines.push("[响应体已被宿主截断]")
	let body: Uint8Array
	try {
		body = decodeBody(res)
	} catch (err) {
		return { text: lines.join("\n") + "\n---\n[无法解码响应体: " + errorText(err) + "]", kind, cleaned: false }
	}
	// 有些站点不回 content-type:按开头嗅探一下,别把网页当二进制省略掉。
	if (kind === "binary" && contentType === "") {
		const head = bufferOfBytes(body.subarray(0, 512)).toString("utf8").trimStart()
		if (/^<(!doctype|html|head|body|div|p|span)/i.test(head)) kind = "html"
	}
	const charset = declaredCharset(contentType)
	if (charset !== "" && charset !== "utf-8" && charset !== "utf8") {
		lines.push("[注意:响应声明 charset=" + charset + ",这里按 UTF-8 解码,可能出现乱码]")
	}
	let text = ""
	let cleaned = false
	if (kind === "binary") {
		text = "[二进制响应(" + (contentType || "未知类型") + ", " + body.length + " B),未解码为文本]"
	} else {
		text = bufferOfBytes(body).toString("utf8")
		if (kind === "html" && clean) {
			text = cleanHTML(text)
			cleaned = true
		}
	}
	lines.push("---")
	lines.push(text === "" ? "(空响应)" : text)
	let joined = lines.join("\n")
	if (utf8Len(joined) > MAX_VISIBLE_BYTES) {
		joined = truncateUTF8(joined, MAX_VISIBLE_BYTES) + "\n[结果已截断]"
	}
	return { text: joined, kind, cleaned }
}

// —— http_search:搜索引擎适配 ——
//
// 每个 provider 只做「一次请求 + 结果归一化」;公共的重定向、解压、超时复用
// http_request 那一套原语,不重复实现传输细节。
type SearchArgs = {
  query?: string
  provider?: string
  maxResults?: number
  site?: string
  oriQuestion?: string
  timeoutSeconds?: number
}

type SearchItem = {
  title: string
  url: string
  snippet: string
  summary?: string
  date?: string
  site?: string
  score?: number
}

// requestText 发一次请求并把响应正文解成字符串(含解压)。
// 搜索接口都是「一次响应、无流式」,所以不需要 http_request 的正文截断语义。
function requestText(url: string, method: string, headers: Record<string, string>, body: string, opts: { timeoutMs: number; insecure: boolean; maxBytes: number; maxRedirects: number }): { res: Response; text: string } {
  let target: any
  try {
    target = new URL(url)
  } catch (err) {
    throw new Error("搜索地址非法: " + url)
  }
  const res = follow(target, method, headers, body, opts)
  const bytes = decodeBody(res)
  return { res, text: bufferOfBytes(bytes).toString("utf8") }
}

function clip(s: string, limit: number): string {
  const text = String(s || "").trim().replace(/\s+/g, " ")
  if (text.length <= limit) return text
  return text.slice(0, limit) + "…"
}

// decodeEntities 只处理搜索结果里最常见的那几个实体:引擎会把标题与摘要里的
// `<`、`&`、引号转义,原样回给模型会破坏排版。
function decodeEntities(s: string): string {
  return String(s || "")
    .replace(/&lt;/gi, "<")
    .replace(/&gt;/gi, ">")
    .replace(/&quot;/gi, '"')
    .replace(/&#39;/g, "'")
    .replace(/&#x27;/gi, "'")
    .replace(/&#(\d+);/g, (_m, d) => String.fromCharCode(Number(d)))
    .replace(/&#x([0-9a-fA-F]+);/g, (_m, h) => String.fromCharCode(parseInt(h, 16)))
    .replace(/&amp;/gi, "&")
}

function stripTags(s: string): string {
  return String(s || "").replace(/<[^>]*>/g, " ").replace(/\s+/g, " ").trim()
}

// unwrapDDGLink 还原 DuckDuckGo 的跳转链接:result__a 的 href 通常是
// `//duckduckgo.com/l/?uddg=<encodeURIComponent(真实地址)>&...`。
function unwrapDDGLink(href: string): string {
  let url = decodeEntities(href)
  if (url.startsWith("//")) url = "https:" + url
  const marker = "uddg="
  const at = url.indexOf(marker)
  if (at < 0) return url
  const rest = url.slice(at + marker.length).split("&")[0]
  try {
    return decodeURIComponent(rest)
  } catch (err) {
    return url
  }
}

// parseDuckDuckGo 从 html.duckduckgo.com 的结果页里抽标题/链接/摘要。
//
// 没有 HTML 解析器可用(执行层不提供 DOM),所以按结果块的稳定标记切:
// 每个结果有一个 class 含 result__a 的锚点,摘要的 class 含 result__snippet。
// 这是一个**脆弱的契约**:DDG 改版时这里会先坏,工具会退化成「0 条」而不是乱码。
function parseDuckDuckGo(html: string, max: number): SearchItem[] {
  const items: SearchItem[] = []
  const anchor = /<a[^>]*class="[^"]*result__a[^"]*"[^>]*>/gi
  const starts: Array<{ tagStart: number; tagEnd: number; href: string }> = []
  let m: any
  while ((m = anchor.exec(html)) !== null) {
    const tag = m[0]
    const hrefM = tag.match(/href="([^"]*)"/i)
    if (hrefM === null) continue
    starts.push({ tagStart: m.index, tagEnd: m.index + tag.length, href: hrefM[1] })
  }
  for (let i = 0; i < starts.length && items.length < max; i++) {
    const s = starts[i]
    const segEnd = i + 1 < starts.length ? starts[i + 1].tagStart : Math.min(html.length, s.tagEnd + 6000)
    const closeA = html.indexOf("</a>", s.tagEnd)
    const titleEnd = closeA >= 0 && closeA < segEnd ? closeA : Math.min(segEnd, s.tagEnd + 600)
    const title = decodeEntities(stripTags(html.slice(s.tagEnd, titleEnd))).trim()
    const url = unwrapDDGLink(s.href)
    if (title === "" || url === "") continue
    const chunk = html.slice(titleEnd, segEnd)
    const snipM = chunk.match(/class="[^"]*result__snippet[^"]*"[^>]*>([\s\S]*?)(?:<\/a>|<\/span>|<\/div>)/i)
    const snippet = snipM === null ? "" : decodeEntities(stripTags(snipM[1])).trim()
    items.push({ title, url, snippet })
  }
  return items
}

// searchDeepSeek 调 DeepSeek 的网页搜索(POST /v1/tools/search)。
// key 走 ada:secrets: 只按名字取,调用失败时给出配置指引,不回显 key。
function searchDeepSeek(query: string, max: number, args: SearchArgs, endpoint: string, keySecret: string, opts: { timeoutMs: number; insecure: boolean; maxBytes: number; maxRedirects: number; userAgent: string }): SearchItem[] {
  let key = ""
  try {
    key = getSecret(keySecret)
  } catch (err) {
    throw new Error("DeepSeek 搜索需要 secret " + keySecret + ":请在 preset 里挂载 seam-secrets + secrets-env(allow 包含该名字),并设置环境变量 ADA_SECRET_" + keySecret)
  }
  if (key.trim() === "") {
    throw new Error("secret " + keySecret + " 为空")
  }
  const payload = JSON.stringify({
    query,
    count: max,
    ori_question: typeof args.oriQuestion === "string" && args.oriQuestion.trim() !== "" ? args.oriQuestion.trim() : null,
    localization: "",
    // 正文按次计费且会撑大结果;模型要正文时应该用 http_request 抓具体链接。
    need_content: false,
  })
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "Accept": "application/json",
    "User-Agent": opts.userAgent,
    "Authorization": "Bearer " + key,
  }
  const { res, text } = requestText(endpoint, "POST", headers, payload, opts)
  if (res.status === 401 || res.status === 403) {
    throw new Error("DeepSeek 搜索鉴权失败(" + res.status + "):检查 " + keySecret + " 是否有效")
  }
  let data: any
  try {
    data = JSON.parse(text)
  } catch (err) {
    throw new Error("DeepSeek 搜索返回的不是 JSON(" + res.status + ")")
  }
  const list = Array.isArray(data?.results) ? data.results : []
  return list.map((r: any): SearchItem => ({
    title: String(r?.title || ""),
    url: String(r?.url || ""),
    snippet: String(r?.snippet || ""),
    summary: String(r?.summary || ""),
    date: typeof r?.date === "string" ? r.date : "",
    site: typeof r?.site_name === "string" ? r.site_name : "",
    score: typeof r?.rerank_score === "number" ? r.rerank_score : undefined,
  })).filter((r: SearchItem) => r.title !== "" && r.url !== "").slice(0, max)
}

function searchDuckDuckGo(query: string, max: number, endpoint: string, opts: { timeoutMs: number; insecure: boolean; maxBytes: number; maxRedirects: number; userAgent: string }): SearchItem[] {
  const sep = endpoint.indexOf("?") >= 0 ? "&" : "?"
  const url = endpoint + sep + "q=" + encodeURIComponent(query)
  const headers: Record<string, string> = {
    "Accept": "text/html,application/xhtml+xml",
    "User-Agent": opts.userAgent,
  }
  const { res, text } = requestText(url, "GET", headers, "", opts)
  if (res.status !== 200) {
    throw new Error("DuckDuckGo 搜索返回状态 " + res.status)
  }
  return parseDuckDuckGo(text, max)
}

function searchSearXNG(query: string, max: number, base: string, opts: { timeoutMs: number; insecure: boolean; maxBytes: number; maxRedirects: number; userAgent: string }): SearchItem[] {
  if (base === "") {
    throw new Error("searxng 需要配置 searxEndpoint(自建实例的根地址)")
  }
  const root = base.endsWith("/") ? base.slice(0, -1) : base
  const url = root + "/search?format=json&q=" + encodeURIComponent(query)
  const headers: Record<string, string> = { "Accept": "application/json", "User-Agent": opts.userAgent }
  const { text } = requestText(url, "GET", headers, "", opts)
  let data: any
  try {
    data = JSON.parse(text)
  } catch (err) {
    throw new Error("searxng 返回的不是 JSON(检查 -json 输出是否开启)")
  }
  const list = Array.isArray(data?.results) ? data.results : []
  return list.map((r: any): SearchItem => ({
    title: String(r?.title || ""),
    url: String(r?.url || ""),
    snippet: String(r?.content || ""),
    date: typeof r?.publishedDate === "string" ? r.publishedDate : "",
    site: typeof r?.engine === "string" ? r.engine : "",
  })).filter((r: SearchItem) => r.title !== "" && r.url !== "").slice(0, max)
}

function searchText(provider: string, query: string, items: SearchItem[], durationMs: number): string {
  const lines: string[] = ["搜索 \"" + query + "\"  (provider: " + provider + ", " + items.length + " 条, " + durationMs + " ms)"]
  if (items.length === 0) {
    lines.push("(没有结果)")
  }
  for (let i = 0; i < items.length; i++) {
    const it = items[i]
    lines.push(String(i + 1) + ". " + it.title)
    lines.push("   " + it.url)
    if (it.site !== undefined && it.site !== "") lines.push("   " + it.site + (it.date ? " · " + it.date : ""))
    if (it.snippet !== "") lines.push("   " + clip(it.snippet, 240))
  }
  let text = lines.join("\n")
  if (utf8Len(text) > MAX_VISIBLE_BYTES) {
    text = truncateUTF8(text, MAX_VISIBLE_BYTES) + "\n[结果已截断]"
  }
  return text
}

export function apply(ctx: Ctx, cfg?: Config): void {
  const conf = cfg ?? {}
  const maxTimeout = clampInt(conf.maxTimeoutSeconds, 30, 1, 120)
  const maxBytes = clampInt(conf.maxBytes, 1 << 20, 1, ABS_MAX_BYTES)
  const maxRedirects = clampCount(conf.maxRedirects, 5, 20)
  const allowInsecure = conf.allowInsecure === true
  const userAgent = typeof conf.userAgent === "string" && conf.userAgent.trim() !== ""
    ? conf.userAgent.trim()
    : "ada-go/http-helper"

  // ——— http_search 的配置 ———
  const searchProvider = typeof conf.searchProvider === "string" && conf.searchProvider.trim() !== ""
    ? conf.searchProvider.trim().toLowerCase()
    : "auto"
  const deepseekEndpoint = typeof conf.deepseekEndpoint === "string" && conf.deepseekEndpoint.trim() !== ""
    ? conf.deepseekEndpoint.trim()
    : "https://api.deepseek.com/v1/tools/search"
  const deepseekKeySecret = typeof conf.deepseekKeySecret === "string" && conf.deepseekKeySecret.trim() !== ""
    ? conf.deepseekKeySecret.trim()
    : "DEEPSEEK_API_KEY"
  const duckduckgoEndpoint = typeof conf.duckduckgoEndpoint === "string" && conf.duckduckgoEndpoint.trim() !== ""
    ? conf.duckduckgoEndpoint.trim()
    : "https://html.duckduckgo.com/html/"
  const searxEndpoint = typeof conf.searxEndpoint === "string" ? conf.searxEndpoint.trim() : ""
  const maxResults = clampInt(conf.maxResults, 5, 1, 10)

  ctx.log("http-helper(ts) 装配完成,maxTimeout=" + maxTimeout + "s, maxBytes=" + maxBytes +
    ", maxRedirects=" + maxRedirects + ", search=" + searchProvider)

  ctx.tool(
    {
      name: "http_request",
      summary: "发一次 HTTP(S) 请求,返回状态码、响应头概要与响应体(支持重定向与 gzip/deflate)",
      readOnly: false,
      timeoutMs: (maxTimeout + 10) * 1000,
      input: {
        type: "object",
        properties: {
          url: { type: "string", description: "完整地址,必须带 http:// 或 https://" },
          method: { type: "string", description: "HTTP 方法,默认 GET;常用 GET/POST/PUT/PATCH/DELETE/HEAD" },
          headers: { type: "object", description: "请求头键值对;值为数组时按逗号合并" },
          query: { type: "object", description: "附加到 URL 的查询参数;数组值展开成重复键" },
          body: {
            type: ["string", "object"],
            description: "请求体:字符串原样发送;对象/数组默认按 JSON 序列化",
          },
          bodyType: { type: "string", enum: ["json", "text", "form"], description: "请求体编码方式,默认按 body 的类型推断" },
          timeoutSeconds: { type: "integer", description: "本次请求超时秒数(上限 " + maxTimeout + ")" },
          followRedirects: { type: "integer", description: "跟随重定向的次数(上限 " + maxRedirects + ")" },
          maxBytes: { type: "integer", description: "响应体字节上限(上限 " + ABS_MAX_BYTES + ")" },
          clean: { type: "boolean", description: "HTML 响应是否清洗成纯文本(默认 true;关掉返回原文)" },
          insecure: { type: "boolean", description: "https 是否跳过证书校验(需宿主允许)" },
        },
        required: ["url"],
      },
    },
    (raw: RequestArgs): ToolResult => {
      const rawURL = typeof raw?.url === "string" ? raw.url.trim() : ""
      if (rawURL === "") return toolError("url 不能为空")

      let target: any
      try {
        target = new URL(rawURL)
      } catch (err) {
        return toolError("url 不是合法地址: " + rawURL)
      }
      if (target.protocol !== "http:" && target.protocol !== "https:") {
        return toolError("只支持 http/https,得到 " + target.protocol)
      }

      const method = (typeof raw?.method === "string" && raw.method.trim() !== "" ? raw.method.trim().toUpperCase() : "GET")
      const headerMap = lowerHeaders(raw?.headers)
      if (typeof headerMap["user-agent"] === "undefined" && typeof headerMap["User-Agent"] === "undefined") {
        headerMap["User-Agent"] = userAgent
      }
      // 默认让服务端压缩:宿主直连 Go 的 http,压缩由本插件用 node:zlib 解开。
      // 调用方显式指定 Accept-Encoding 时不覆盖,由它自己负责解码。
      if (typeof headerMap["accept-encoding"] === "undefined" && typeof headerMap["Accept-Encoding"] === "undefined") {
        headerMap["Accept-Encoding"] = "gzip, deflate"
      }

      const encoded = bodyOf(raw?.body, raw?.bodyType)
      if (encoded.text !== "" && typeof headerMap["content-type"] === "undefined" && typeof headerMap["Content-Type"] === "undefined" && encoded.contentType !== "") {
        headerMap["Content-Type"] = encoded.contentType
      }
      // Content-Length 不在这里算:string.length 是 UTF-16 码元数,与字节数不同;
      // node:http 在 end() 时按 Buffer 的实际字节补上它(见 nodemods/src/http.js)。
      const body = encoded.text

      const timeout = clampInt(raw?.timeoutSeconds, maxTimeout, 1, maxTimeout)
      const limit = clampInt(raw?.maxBytes, maxBytes, 1, ABS_MAX_BYTES)
      const redirects = clampCount(raw?.followRedirects, maxRedirects, maxRedirects)
      const insecure = raw?.insecure === true
      if (insecure && !allowInsecure) {
        return toolError("insecure 未在插件配置里允许(allowInsecure=false)")
      }

      const finalURL = withQuery(target.toString(), raw?.query)
      let start = 0
      try {
        target = new URL(finalURL)
        // 毫秒计时用 Date.now:本运行时没有 node:timers,也没有 performance。
        start = Date.now()
        const res = follow(target, method, headerMap, body, {
          timeoutMs: timeout * 1000,
          insecure,
          maxBytes: limit,
          maxRedirects: redirects,
        })
        const durationMs = Date.now() - start
        const shown = responseText(res, method, raw?.clean !== false)
        return {
          status: "ok",
          content: shown.text,
          details: {
            url: finalURL,
            method,
            status: res.status,
            statusText: res.statusText,
            headers: res.headers,
            bytes: res.body.length,
            durationMs,
            truncated: res.truncated,
            redirects: res.redirects,
            encoding: res.headers["content-encoding"] || "",
            cleaned: shown.cleaned,
            kind: shown.kind,
          },
        }
      } catch (err) {
        return toolError("HTTP 请求失败(" + method + " " + finalURL + "): " + errorText(err))
      }
    },
  )

  // —— http_search ——
  // 只读:搜索不改变远端状态。provider=auto 时按「有没有配 DeepSeek key」选,
  // 没 key 就退到 DuckDuckGo(不需要 key),这样默认配置开箱可用。
  ctx.tool(
    {
      name: "http_search",
      summary: "搜索网页:返回标题/链接/摘要(DeepSeek 网页搜索 / DuckDuckGo / SearXNG)",
      readOnly: true,
      timeoutMs: (maxTimeout + 10) * 1000,
      input: {
        type: "object",
        properties: {
          query: { type: "string", description: "搜索词;支持 site: / -site: / 引号等引擎语法" },
          provider: {
            type: "string",
            enum: ["auto", "deepseek", "duckduckgo", "searxng"],
            description: "搜索引擎,默认按配置(auto:有 DeepSeek key 用它,否则 DuckDuckGo)",
          },
          maxResults: { type: "integer", description: "返回条数 1-10,默认 " + maxResults },
          site: { type: "string", description: "限定站点,拼成 site:<域名>" },
          oriQuestion: { type: "string", description: "触发搜索的原始问题(仅 DeepSeek,帮助理解搜索意图)" },
          timeoutSeconds: { type: "integer", description: "本次搜索超时秒数(上限 " + maxTimeout + ")" },
        },
        required: ["query"],
      },
    },
    (raw: SearchArgs): ToolResult => {
      const rawQuery = typeof raw?.query === "string" ? raw.query.trim() : ""
      if (rawQuery === "") return toolError("query 不能为空")
      let query = rawQuery
      if (typeof raw?.site === "string" && raw.site.trim() !== "") {
        query = query + " site:" + raw.site.trim()
      }
      const count = clampInt(raw?.maxResults, maxResults, 1, 10)
      const timeout = clampInt(raw?.timeoutSeconds, maxTimeout, 1, maxTimeout)

      const wanted = typeof raw?.provider === "string" && raw.provider.trim() !== ""
        ? raw.provider.trim().toLowerCase()
        : searchProvider
      let provider = wanted
      if (provider === "auto") {
        provider = hasSecret(deepseekKeySecret) ? "deepseek" : "duckduckgo"
      }
      if (["deepseek", "duckduckgo", "searxng"].indexOf(provider) < 0) {
        return toolError("不支持的 provider: " + provider + "(可用 auto/deepseek/duckduckgo/searxng)")
      }

      const opts = {
        timeoutMs: timeout * 1000,
        insecure: false,
        maxBytes: maxBytes,
        maxRedirects: Math.min(3, maxRedirects),
        userAgent,
      }
      const start = Date.now()
      try {
        let items: SearchItem[]
        if (provider === "deepseek") {
          items = searchDeepSeek(query, count, raw, deepseekEndpoint, deepseekKeySecret, opts)
        } else if (provider === "searxng") {
          items = searchSearXNG(query, count, searxEndpoint, opts)
        } else {
          items = searchDuckDuckGo(query, count, duckduckgoEndpoint, opts)
        }
        const durationMs = Date.now() - start
        return {
          status: "ok",
          content: searchText(provider, query, items, durationMs),
          details: {
            provider,
            query,
            count: items.length,
            durationMs,
            results: items,
          },
        }
      } catch (err) {
        return toolError("搜索失败(" + provider + "): " + errorText(err))
      }
    },
  )
}
