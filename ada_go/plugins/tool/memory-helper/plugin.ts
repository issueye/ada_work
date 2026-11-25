// memory-helper:给模型用的长期记忆系统(TypeScript 主实现)。
//
// 分层:
//   - 记录模型、中文分词、相关度排序、去重、淘汰与渲染都在本脚本里;
//   - 持久化只经宿主注入的 ada:fs(走 fs seam 与策略链),脚本不直接碰磁盘;
//   - 稳定 id 与内容指纹用宿主补充的 node:crypto(ada_jsrt/jsrt/nodemods/src/crypto.js);
//   - 中文没有词边界:用 moejs 内建的 Intl.Segmenter 切段,再把连续汉字合成 bigram。
//
// 已知边界:
//   - 单运行时内按工具调用串行读改写;多进程同时写同一文件会丢更新
//     (ada:fs 没有跨进程锁,与 task-helper 的边界相同);
//   - 检索是词典匹配(非向量语义),同义改写不一定命中;
//   - 记忆文件是工作区里的普通文本,只读/沙箱/caps 策略同样约束它。
import { read, write } from "ada:fs"
import { createHash, randomUUID } from "node:crypto"

type ToolSpec = {
  name: string
  summary?: string
  input?: Record<string, unknown>
  readOnly?: boolean
  timeoutMs?: number
}
type Ctx = {
  log: (msg: string) => void
  tool: (spec: ToolSpec, handler: (args: any) => any) => void
  waterfall?: (event: string, handler: (input: any, next: (v: any) => any) => any) => void
}

// Config 与 preset 里 memory-helper 的 patch 对应;缺省值在 normalizeConfig 补齐。
type Config = {
  path?: string
  maxRecords?: number
  maxTextChars?: number
  searchLimit?: number
  listLimit?: number
  defaultScope?: string
  dedupeThreshold?: number
  maxSummaryChars?: number
  // autoRecall 打开后,在每次模型请求前按最后一条用户消息自动召回(top N)。
  autoRecall?: boolean
  recallLimit?: number
  recallMaxChars?: number
  recallMinScore?: number
}
type Settings = {
  path: string
  maxRecords: number
  maxTextChars: number
  searchLimit: number
  listLimit: number
  defaultScope: Scope
  dedupeThreshold: number
  maxSummaryChars: number
  autoRecall: boolean
  recallLimit: number
  recallMaxChars: number
  recallMinScore: number
}

type Scope = "project" | "user" | "global"
type MemoryRecord = {
  id: string
  text: string
  tags: string[]
  scope: Scope
  importance: number
  createdAt: string
  updatedAt: string
  fingerprint: string
  // pinned 的记忆不会被容量/文件上限自动淘汰;需要先手动解除或删除。
  pinned?: boolean
}
type MemoryStore = { version: number; updatedAt: string; records: MemoryRecord[] }
type Hit = { record: MemoryRecord; score: number }

const DEFAULT_PATH = "/workspace/.ada/memory.json"
// ada:fs 单次写入上限是 4 MiB;这里留出余量,避免写满后工具整体失败。
const MAX_FILE_BYTES = 3 << 20
const MAX_TAGS = 8
const MAX_TAG_CHARS = 24
const MAX_QUERY_CHARS = 500
const SCOPES: Scope[] = ["project", "user", "global"]

// 停用词表只处理最常见的功能词;中文检索主要靠 bigram,单字权重已经降到 0.45。
const STOP = new Set<string>([
  "的", "了", "是", "在", "我", "你", "他", "她", "它", "们", "这", "那", "就", "也",
  "和", "与", "及", "或", "而", "但", "被", "把", "让", "对", "为", "以", "于", "之",
  "上", "下", "个", "一", "不", "有", "会", "要", "可", "能", "很", "更", "最", "都",
  "又", "还", "并", "从", "到", "向", "给", "等", "里", "外", "着", "过", "其", "该",
  "此", "些", "么", "呢", "吗", "啊", "吧", "中",
  "the", "a", "an", "is", "are", "was", "were", "be", "of", "to", "in", "on", "at",
  "and", "or", "for", "with", "this", "that", "these", "those", "it", "its", "as",
  "by", "from", "not", "no", "yes", "do", "does", "did", "done", "have", "has", "had",
  "i", "you", "he", "she", "we", "they", "me", "my", "your", "our", "their",
])

function nowISO(): string {
  return new Date().toISOString()
}

function dateOnly(iso: string): string {
  return iso.length >= 10 ? iso.slice(0, 10) : iso
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

// isMissing 用错误码判定「文件还不存在」:ada:fs 的错误前缀是 ENOENT。
function isMissing(err: unknown): boolean {
  return errorText(err).indexOf("ENOENT") >= 0
}

function clampInt(value: unknown, def: number, min: number, max: number): number {
  const n = Number(value)
  if (!Number.isFinite(n)) return def
  const v = Math.floor(n)
  if (v < min) return min
  if (v > max) return max
  return v
}

function utf8Len(s: string): number {
  let n = 0
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c < 0x80) n += 1
    else if (c < 0x800) n += 2
    else if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
      n += 4
      i++
    } else n += 3
  }
  return n
}

function normalizeText(text: string): string {
  let out = text
  try {
    out = text.normalize("NFKC")
  } catch {
    out = text
  }
  return out.replace(/\s+/g, " ").trim()
}

function normalizeConfig(raw: Config | undefined): Settings {
  const c = raw === undefined || raw === null ? {} : raw
  let path = typeof c.path === "string" && c.path.trim() !== "" ? c.path.trim() : DEFAULT_PATH
  if (path.indexOf("\u0000") >= 0) throw new Error("path 非法")
  if (path.charAt(0) !== "/") path = "/workspace/" + path
  let defaultScope: Scope = "project"
  if (typeof c.defaultScope === "string" && isScope(c.defaultScope)) defaultScope = normalizeScopeValue(c.defaultScope)
  let dedupe = Number(c.dedupeThreshold)
  if (!Number.isFinite(dedupe)) dedupe = 0.85
  if (dedupe < 0) dedupe = 0
  if (dedupe > 1) dedupe = 1
  let recallMinScore = Number(c.recallMinScore)
  if (!Number.isFinite(recallMinScore) || recallMinScore < 0) recallMinScore = 0
  return {
    path,
    maxRecords: clampInt(c.maxRecords, 2000, 50, 10000),
    maxTextChars: clampInt(c.maxTextChars, 4000, 100, 20000),
    searchLimit: clampInt(c.searchLimit, 8, 1, 50),
    listLimit: clampInt(c.listLimit, 20, 1, 100),
    defaultScope,
    dedupeThreshold: dedupe,
    maxSummaryChars: clampInt(c.maxSummaryChars, 240, 60, 1000),
    autoRecall: c.autoRecall === true,
    recallLimit: clampInt(c.recallLimit, 5, 1, 20),
    recallMaxChars: clampInt(c.recallMaxChars, 1200, 200, 8000),
    recallMinScore,
  }
}

function isScope(value: string): boolean {
  return value === "project" || value === "user" || value === "global"
}

function normalizeScopeValue(value: string): Scope {
  const v = value.trim().toLowerCase()
  if (!isScope(v)) throw new Error("scope 只能是 " + SCOPES.join(" / ") + ",得到 " + JSON.stringify(value))
  return v as Scope
}

// normalizeScope 在缺省时用配置里的 defaultScope;显式传入必须合法。
function normalizeScope(s: Settings, value: unknown): Scope {
  if (value === undefined || value === null || value === "") return s.defaultScope
  if (typeof value !== "string") throw new Error("scope 必须是字符串")
  return normalizeScopeValue(value)
}

function normalizeImportance(value: unknown, def: number): number {
  if (value === undefined || value === null) return def
  const n = Number(value)
  if (!Number.isFinite(n) || Math.floor(n) !== n) throw new Error("importance 必须是整数 1-5")
  if (n < 1 || n > 5) throw new Error("importance 必须在 1-5 之间")
  return n
}

function normalizeTags(value: unknown): string[] {
  if (value === undefined || value === null) return []
  let list: any[]
  if (typeof value === "string") list = [value]
  else if (Array.isArray(value)) list = value
  else throw new Error("tags 必须是字符串数组")
  const out: string[] = []
  for (let i = 0; i < list.length; i++) {
    const item = list[i]
    if (item === undefined || item === null) continue
    let tag = String(item).trim().toLowerCase()
    if (tag === "") continue
    if (tag.length > MAX_TAG_CHARS) throw new Error("tag 太长(最多 " + MAX_TAG_CHARS + " 字符): " + tag)
    if (out.indexOf(tag) >= 0) continue
    out.push(tag)
  }
  if (out.length > MAX_TAGS) throw new Error("tags 最多 " + MAX_TAGS + " 个")
  return out
}

function requireText(s: Settings, value: unknown): string {
  if (typeof value !== "string") throw new Error("text 必须是字符串")
  if (value.indexOf("\u0000") >= 0) throw new Error("text 不能包含 NUL")
  const text = normalizeText(value)
  if (text === "") throw new Error("text 不能为空")
  if (text.length > s.maxTextChars) throw new Error("text 太长(最多 " + s.maxTextChars + " 字符)")
  return text
}

function requireQuery(value: unknown): string {
  if (typeof value !== "string") throw new Error("query 必须是字符串")
  const query = normalizeText(value)
  if (query === "") throw new Error("query 不能为空")
  if (query.length > MAX_QUERY_CHARS) throw new Error("query 太长(最多 " + MAX_QUERY_CHARS + " 字符)")
  return query
}

function fingerprintOf(text: string): string {
  return createHash("sha256").update(normalizeText(text).toLowerCase()).digest("hex")
}

// shortId 只取 UUID 的第一段:8 个 hex 足够在默认上限内避免前缀冲突,
// resolveRecord 仍会拒绝歧义前缀,所以碰撞不会静默读错记录。
function shortId(id: string): string {
  return id.slice(0, 8)
}

function summary(text: string, max: number): string {
  const flat = text.replace(/\s+/g, " ").trim()
  if (flat.length <= max) return flat
  return flat.slice(0, max - 1) + "…"
}

// ---------- 分词与相关度 ----------

function isCJKChar(ch: string): boolean {
  const c = ch.charCodeAt(0)
  return (c >= 0x4e00 && c <= 0x9fff) || (c >= 0x3400 && c <= 0x4dbf) ||
    (c >= 0xf900 && c <= 0xfaff) || (c >= 0x3040 && c <= 0x30ff) ||
    (c >= 0xac00 && c <= 0xd7af)
}

function allCJK(s: string): boolean {
  if (s.length === 0) return false
  for (let i = 0; i < s.length; i++) {
    if (!isCJKChar(s[i])) return false
  }
  return true
}

function bump(out: Map<string, number>, term: string, weight: number): void {
  out.set(term, (out.get(term) || 0) + weight)
}

// addCJKBag 把连续汉字压成 unigram + bigram:没有词典时 bigram 是最稳的近似。
function addCJKBag(out: Map<string, number>, run: string): void {
  for (let i = 0; i < run.length; i++) {
    const ch = run[i]
    if (!STOP.has(ch)) bump(out, ch, 0.45)
    if (i + 1 < run.length) {
      const next = run[i + 1]
      if (STOP.has(ch) && STOP.has(next)) continue
      bump(out, run.slice(i, i + 2), 1)
    }
  }
}

function addWord(out: Map<string, number>, word: string): void {
  const w = word.toLowerCase()
  if (w === "" || STOP.has(w)) return
  // 单个 ASCII 字母没有区分度;单个汉字已经由 addCJKBag 处理。
  if (w.length === 1 && !isCJKChar(w)) return
  bump(out, w, w.length >= 2 ? 1 : 0.45)
}

function isWordChar(ch: string): boolean {
  const c = ch.charCodeAt(0)
  return (c >= 0x30 && c <= 0x39) || (c >= 0x61 && c <= 0x7a) ||
    (c >= 0xc0 && c <= 0x2af) || (c >= 0x370 && c <= 0x1fff)
}

// tokenize 返回「词 → 加权出现次数」。分段优先用 moejs 的 Intl.Segmenter;
// 引擎没有它时退化为逐字符扫描,保证顺序与结果仍然确定。
function tokenize(text: string): Map<string, number> {
  const out = new Map<string, number>()
  const norm = normalizeText(text).toLowerCase()
  if (norm === "") return out
  let segmenter: any = null
  try {
    if (typeof (Intl as any).Segmenter === "function") {
      segmenter = new (Intl as any).Segmenter("zh", { granularity: "word" })
    }
  } catch {
    segmenter = null
  }
  if (segmenter !== null) {
    let run = ""
    const flush = () => {
      if (run !== "") {
        addCJKBag(out, run)
        run = ""
      }
    }
    for (const piece of segmenter.segment(norm)) {
      const word = String(piece.segment)
      if (!piece.isWordLike) {
        flush()
        continue
      }
      if (allCJK(word)) {
        run += word
        continue
      }
      flush()
      addWord(out, word)
    }
    flush()
    return out
  }

  let ascii = ""
  let run = ""
  const flushAscii = () => {
    if (ascii !== "") {
      addWord(out, ascii)
      ascii = ""
    }
  }
  const flushRun = () => {
    if (run !== "") {
      addCJKBag(out, run)
      run = ""
    }
  }
  for (let i = 0; i < norm.length; i++) {
    const ch = norm[i]
    if (isCJKChar(ch)) {
      flushAscii()
      run += ch
    } else if (isWordChar(ch)) {
      flushRun()
      ascii += ch
    } else {
      flushAscii()
      flushRun()
    }
  }
  flushAscii()
  flushRun()
  return out
}

function tokenCount(tokens: Map<string, number>): number {
  let n = 0
  tokens.forEach((v) => { n += v })
  return n
}

// bm25 只算词频与文档长度,不引入外部索引:记忆规模(默认 ≤ 2000 条)线性扫描够用。
function bm25(tf: number, dl: number, avgdl: number, idf: number, weight: number): number {
  if (tf <= 0 || idf <= 0) return 0
  const k1 = 1.2
  const b = 0.75
  const denom = tf + k1 * (1 - b + b * (avgdl > 0 ? dl / avgdl : 1))
  return idf * (tf * (k1 + 1)) / denom * weight
}

function ageDays(iso: string): number {
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return 9999
  const days = (Date.now() - t) / 86400000
  return days < 0 ? 0 : days
}

function filterRecords(records: MemoryRecord[], scope: string, tags: string[]): MemoryRecord[] {
  const out: MemoryRecord[] = []
  for (let i = 0; i < records.length; i++) {
    const rec = records[i]
    if (scope !== "" && rec.scope !== scope) continue
    let all = true
    for (let j = 0; j < tags.length; j++) {
      if (rec.tags.indexOf(tags[j]) < 0) { all = false; break }
    }
    if (!all) continue
    out.push(rec)
  }
  return out
}

// rank 做 BM25 + 精确短语 + 重要度/新鲜度/作用域加成;返回有上限的命中列表。
function rank(records: MemoryRecord[], query: string, scope: string, tags: string[], limit: number): Hit[] {
  const q = tokenize(query)
  if (q.size === 0) throw new Error("query 里没有可用于检索的词")
  const candidates = filterRecords(records, scope, tags)
  if (candidates.length === 0) return []

  const docs: Map<string, number>[] = []
  let total = 0
  for (let i = 0; i < candidates.length; i++) {
    const rec = candidates[i]
    const doc = tokenize(rec.text + " " + rec.tags.join(" "))
    docs.push(doc)
    total += tokenCount(doc)
  }
  const avgdl = total / candidates.length

  const df = new Map<string, number>()
  for (let i = 0; i < docs.length; i++) {
    const doc = docs[i]
    q.forEach((_weight, term) => {
      if (doc.has(term)) df.set(term, (df.get(term) || 0) + 1)
    })
  }
  const idf = new Map<string, number>()
  df.forEach((n, term) => {
    idf.set(term, Math.log(1 + (candidates.length - n + 0.5) / (n + 0.5)))
  })

  const phrase = normalizeText(query).toLowerCase()
  const hits: Hit[] = []
  for (let i = 0; i < candidates.length; i++) {
    const rec = candidates[i]
    const doc = docs[i]
    let score = 0
    q.forEach((weight, term) => {
      score += bm25(doc.get(term) || 0, tokenCount(doc), avgdl, idf.get(term) || 0, weight)
    })
    if (score <= 0) continue
    if (phrase.length >= 2 && normalizeText(rec.text).toLowerCase().indexOf(phrase) >= 0) score += 1.2
    score += (rec.importance - 3) * 0.12
    score += 0.3 / (1 + ageDays(rec.updatedAt) / 60)
    if (scope !== "" && rec.scope === scope) score += 0.15
    hits.push({ record: rec, score })
  }
  hits.sort((a, b) => {
    if (b.score !== a.score) return b.score - a.score
    if (a.record.updatedAt < b.record.updatedAt) return 1
    if (a.record.updatedAt > b.record.updatedAt) return -1
    return 0
  })
  return hits.slice(0, limit)
}

// ---------- 存取 ----------

function emptyStore(): MemoryStore {
  return { version: 1, updatedAt: nowISO(), records: [] }
}

function normalizeRecord(value: any): MemoryRecord | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null
  if (typeof value.id !== "string" || value.id === "") return null
  if (typeof value.text !== "string") return null
  const text = normalizeText(value.text)
  if (text === "") return null
  const scope = typeof value.scope === "string" && isScope(value.scope) ? (value.scope as Scope) : "project"
  // 读文件时对脏字段做保守修复:单条记录写坏不应让整库不可用。
  let tags: string[] = []
  if (Array.isArray(value.tags)) {
    try {
      tags = normalizeTags(value.tags)
    } catch {
      tags = []
    }
  }
  let importance = 3
  try {
    importance = normalizeImportance(value.importance, 3)
  } catch {
    importance = 3
  }
  const createdAt = typeof value.createdAt === "string" ? value.createdAt : nowISO()
  const updatedAt = typeof value.updatedAt === "string" ? value.updatedAt : createdAt
  const fingerprint = typeof value.fingerprint === "string" && value.fingerprint !== ""
    ? value.fingerprint
    : fingerprintOf(text)
  const pinned = value.pinned === true
  return { id: value.id, text, tags, scope, importance, createdAt, updatedAt, fingerprint, pinned }
}

function loadStore(s: Settings): MemoryStore {
  let raw = ""
  try {
    raw = read(s.path)
  } catch (err) {
    if (isMissing(err)) return emptyStore()
    throw new Error("读取记忆文件 " + s.path + " 失败: " + errorText(err))
  }
  if (raw.trim() === "") return emptyStore()
  let parsed: any
  try {
    parsed = JSON.parse(raw)
  } catch (err) {
    // 损坏时拒绝覆盖:静默重置会把已有记忆当成空库写掉。
    throw new Error("记忆文件 " + s.path + " 不是合法 JSON,拒绝覆盖: " + errorText(err))
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("记忆文件 " + s.path + " 的顶层必须是对象,拒绝覆盖")
  }
  const listed = Array.isArray(parsed.records) ? parsed.records : []
  const records: MemoryRecord[] = []
  for (let i = 0; i < listed.length; i++) {
    const rec = normalizeRecord(listed[i])
    if (rec !== null) records.push(rec)
  }
  return {
    version: 1,
    updatedAt: typeof parsed.updatedAt === "string" ? parsed.updatedAt : nowISO(),
    records,
  }
}

// evictIndex 返回「最该被淘汰」的下标:先按重要度、再按更新时间;
// pinned 记录不参与淘汰,全部 pinned 时返回 -1(由调用方报错,不静默丢数据)。
function evictIndex(records: MemoryRecord[]): number {
  let idx = -1
  for (let i = 0; i < records.length; i++) {
    if (records[i].pinned === true) continue
    if (idx < 0 || lowerPriority(records[i], records[idx])) idx = i
  }
  return idx
}

function lowerPriority(a: MemoryRecord, b: MemoryRecord): boolean {
  if (a.importance !== b.importance) return a.importance < b.importance
  return a.updatedAt < b.updatedAt
}

function saveStore(s: Settings, store: MemoryStore): number {
  const evictOne = () => {
    const idx = evictIndex(store.records)
    if (idx < 0) throw new Error("记忆已满且全部 pinned;请先解除 pinned 或 memory_forget")
    store.records.splice(idx, 1)
  }
  let evicted = 0
  while (store.records.length > s.maxRecords) {
    evictOne()
    evicted++
  }
  store.updatedAt = nowISO()
  let body = JSON.stringify(store)
  let guard = 0
  while (utf8Len(body) > MAX_FILE_BYTES && store.records.length > 1 && guard < 200) {
    evictOne()
    evicted++
    guard++
    store.updatedAt = nowISO()
    body = JSON.stringify(store)
  }
  if (utf8Len(body) > MAX_FILE_BYTES) {
    throw new Error("记忆文件超过 " + MAX_FILE_BYTES + " 字节上限,请先 memory_forget 一些条目")
  }
  write(s.path, body, true)
  return evicted
}

function resolveRecord(store: MemoryStore, value: unknown): MemoryRecord {
  if (typeof value !== "string") throw new Error("id 必须是字符串")
  const id = value.trim().toLowerCase()
  if (id === "") throw new Error("id 不能为空")
  if (id.length < 4) throw new Error("id 至少给 4 个字符(完整 UUID 或唯一前缀)")
  let exact: MemoryRecord | null = null
  let prefix: MemoryRecord | null = null
  let matches = 0
  for (let i = 0; i < store.records.length; i++) {
    const rec = store.records[i]
    if (rec.id === id) exact = rec
    if (rec.id.indexOf(id) === 0) {
      matches++
      prefix = rec
    }
  }
  if (exact !== null) return exact
  if (matches === 0) throw new Error("没有这条记忆: " + id)
  if (matches > 1) throw new Error("id 前缀不唯一,请多给几位: " + id)
  return prefix as MemoryRecord
}

function findNearDuplicate(s: Settings, store: MemoryStore, scope: Scope, text: string): MemoryRecord | null {
  if (s.dedupeThreshold <= 0 || s.dedupeThreshold >= 1) return null
  const tokens = tokenize(text)
  if (tokens.size === 0) return null
  let best: MemoryRecord | null = null
  let bestScore = 0
  for (let i = 0; i < store.records.length; i++) {
    const rec = store.records[i]
    if (rec.scope !== scope) continue
    const other = tokenize(rec.text)
    if (other.size === 0) continue
    let inter = 0
    tokens.forEach((_w, term) => { if (other.has(term)) inter++ })
    const union = tokens.size + other.size - inter
    const sim = union === 0 ? 0 : inter / union
    if (sim >= s.dedupeThreshold && sim > bestScore) {
      bestScore = sim
      best = rec
    }
  }
  return best
}

function mergeInto(rec: MemoryRecord, text: string, tags: string[], importance: number, pinned: boolean, now: string): void {
  for (let i = 0; i < tags.length; i++) {
    if (rec.tags.indexOf(tags[i]) < 0 && rec.tags.length < MAX_TAGS) rec.tags.push(tags[i])
  }
  if (importance > rec.importance) rec.importance = importance
  if (pinned) rec.pinned = true
  // 新内容明显更长时替换正文,否则保留原文:合并的目标是去重,不是覆盖历史事实。
  if (text.length > rec.text.length * 1.25) {
    rec.text = text
    rec.fingerprint = fingerprintOf(text)
  }
  rec.updatedAt = now
}

function renderMeta(rec: MemoryRecord): string {
  let out = "scope=" + rec.scope + ", p" + rec.importance
  if (rec.pinned === true) out += ", pinned"
  if (rec.tags.length > 0) out += ", tags=" + rec.tags.join("/")
  return out
}

function renderRecord(rec: MemoryRecord): string {
  return [
    "#" + shortId(rec.id) + " 完整记忆",
    "id: " + rec.id,
    "scope: " + rec.scope,
    "importance: " + rec.importance,
    "pinned: " + (rec.pinned === true ? "true" : "false"),
    "tags: " + (rec.tags.length > 0 ? rec.tags.join(", ") : "(无)"),
    "created: " + rec.createdAt,
    "updated: " + rec.updatedAt,
    "---",
    rec.text,
  ].join("\n")
}

// ---------- 工具实现 ----------

function memoryWrite(s: Settings, args: any): string {
  const text = requireText(s, args === undefined ? undefined : args.text)
  const scope = normalizeScope(s, args === undefined ? undefined : args.scope)
  const tags = normalizeTags(args === undefined ? undefined : args.tags)
  const importance = normalizeImportance(args === undefined ? undefined : args.importance, 3)
  const pinned = args !== undefined && args.pinned === true
  const store = loadStore(s)
  const fingerprint = fingerprintOf(text)
  const now = nowISO()

  let target: MemoryRecord | null = null
  for (let i = 0; i < store.records.length; i++) {
    const rec = store.records[i]
    if (rec.scope === scope && rec.fingerprint === fingerprint) { target = rec; break }
  }
  if (target === null) target = findNearDuplicate(s, store, scope, text)
  if (target !== null) {
    mergeInto(target, text, tags, importance, pinned, now)
    const evicted = saveStore(s, store)
    let msg = "与已有记忆 #" + shortId(target.id) + " 合并(" + renderMeta(target) + ")。当前共 " + store.records.length + " 条。"
    if (evicted > 0) msg += " 已淘汰 " + evicted + " 条低价值记忆。"
    return msg
  }

  const record: MemoryRecord = {
    id: randomUUID(),
    text,
    tags,
    scope,
    importance,
    createdAt: now,
    updatedAt: now,
    fingerprint,
    pinned,
  }
  store.records.push(record)
  const evicted = saveStore(s, store)
  let msg = "已记住 #" + shortId(record.id) + "(" + renderMeta(record) + ")。当前共 " + store.records.length + " 条。"
  if (evicted > 0) msg += " 已淘汰 " + evicted + " 条低价值记忆。"
  return msg
}

function memorySearch(s: Settings, args: any): string {
  const query = requireQuery(args === undefined ? undefined : args.query)
  const scope = args !== undefined && args.scope !== undefined && args.scope !== null && args.scope !== ""
    ? normalizeScope(s, args.scope)
    : ""
  const tags = normalizeTags(args === undefined ? undefined : args.tags)
  const limit = clampInt(args === undefined ? undefined : args.limit, s.searchLimit, 1, 50)
  const store = loadStore(s)
  const hits = rank(store.records, query, scope, tags, limit)
  if (hits.length === 0) {
    return "没有匹配的记忆(可用 memory_write 添加,或换一个关键词)。"
  }
  const lines: string[] = ["找到 " + hits.length + " 条相关记忆:"]
  for (let i = 0; i < hits.length; i++) {
    const hit = hits[i]
    const rec = hit.record
    lines.push((i + 1) + ". #" + shortId(rec.id) + " [相关度 " + hit.score.toFixed(2) +
      ", " + rec.scope + ", p" + rec.importance + (rec.pinned === true ? ", pinned" : "") + "] " +
      summary(rec.text, s.maxSummaryChars))
    if (rec.tags.length > 0) lines.push("   tags: " + rec.tags.join(", "))
  }
  return lines.join("\n")
}

function memoryRead(s: Settings, args: any): string {
  const store = loadStore(s)
  const rec = resolveRecord(store, args === undefined ? undefined : args.id)
  return renderRecord(rec)
}

function memoryList(s: Settings, args: any): string {
  const scope = args !== undefined && args.scope !== undefined && args.scope !== null && args.scope !== ""
    ? normalizeScope(s, args.scope)
    : ""
  const tags = normalizeTags(args === undefined ? undefined : args.tags)
  const limit = clampInt(args === undefined ? undefined : args.limit, s.listLimit, 1, 100)
  const offset = clampInt(args === undefined ? undefined : args.offset, 0, 0, 100000)
  const store = loadStore(s)
  const items = filterRecords(store.records, scope, tags)
  items.sort((a, b) => {
    if (a.importance !== b.importance) return b.importance - a.importance
    if (a.updatedAt < b.updatedAt) return 1
    if (a.updatedAt > b.updatedAt) return -1
    return 0
  })
  if (items.length === 0) return "还没有记忆(可用 memory_write 添加)。"
  const start = offset >= items.length ? items.length : offset
  const end = start + limit > items.length ? items.length : start + limit
  const lines: string[] = [
    "共 " + items.length + " 条记忆,显示第 " + (start + 1) + "-" + end + " 条:",
  ]
  for (let i = start; i < end; i++) {
    const rec = items[i]
    lines.push("#" + shortId(rec.id) + " p" + rec.importance + " " + rec.scope +
      (rec.pinned === true ? " [pin]" : "") + " " + dateOnly(rec.updatedAt) + " " +
      summary(rec.text, s.maxSummaryChars))
    if (rec.tags.length > 0) lines.push("   tags: " + rec.tags.join(", "))
  }
  return lines.join("\n")
}

function memoryUpdate(s: Settings, args: any): string {
  const store = loadStore(s)
  const rec = resolveRecord(store, args === undefined ? undefined : args.id)
  let changed = false
  if (args !== undefined && args.text !== undefined) {
    const text = requireText(s, args.text)
    if (text !== rec.text) {
      rec.text = text
      rec.fingerprint = fingerprintOf(text)
      changed = true
    }
  }
  if (args !== undefined && args.tags !== undefined) {
    rec.tags = normalizeTags(args.tags)
    changed = true
  }
  if (args !== undefined && args.scope !== undefined) {
    const scope = normalizeScope(s, args.scope)
    if (scope !== rec.scope) { rec.scope = scope; changed = true }
  }
  if (args !== undefined && args.importance !== undefined) {
    const importance = normalizeImportance(args.importance, 3)
    if (importance !== rec.importance) { rec.importance = importance; changed = true }
  }
  if (args !== undefined && args.pinned !== undefined) {
    const pinned = args.pinned === true
    if (pinned !== (rec.pinned === true)) { rec.pinned = pinned; changed = true }
  }
  if (!changed) throw new Error("没有需要更新的字段")
  for (let i = 0; i < store.records.length; i++) {
    const other = store.records[i]
    if (other.id !== rec.id && other.scope === rec.scope && other.fingerprint === rec.fingerprint) {
      throw new Error("更新后与已有记忆 #" + shortId(other.id) + " 重复;请先 memory_forget 或换一种写法")
    }
  }
  rec.updatedAt = nowISO()
  saveStore(s, store)
  return "已更新 #" + shortId(rec.id) + "(" + renderMeta(rec) + ")。"
}

function memoryForget(s: Settings, args: any): string {
  const store = loadStore(s)
  const rec = resolveRecord(store, args === undefined ? undefined : args.id)
  const idx = store.records.indexOf(rec)
  if (idx < 0) throw new Error("记忆状态异常: " + rec.id)
  store.records.splice(idx, 1)
  saveStore(s, store)
  return "已忘记 #" + shortId(rec.id) + "(" + summary(rec.text, 60) + ")。当前共 " + store.records.length + " 条。"
}

function memoryStats(s: Settings): string {
  const store = loadStore(s)
  const scopes = new Map<string, number>()
  const tags = new Map<string, number>()
  let tokens = 0
  for (let i = 0; i < store.records.length; i++) {
    const rec = store.records[i]
    scopes.set(rec.scope, (scopes.get(rec.scope) || 0) + 1)
    for (let j = 0; j < rec.tags.length; j++) {
      tags.set(rec.tags[j], (tags.get(rec.tags[j]) || 0) + 1)
    }
    tokens += tokenCount(tokenize(rec.text))
  }
  const most: string[] = []
  const entries: Array<{ tag: string; n: number }> = []
  tags.forEach((n, tag) => entries.push({ tag, n }))
  entries.sort((a, b) => {
    if (b.n !== a.n) return b.n - a.n
    if (a.tag < b.tag) return -1
    if (a.tag > b.tag) return 1
    return 0
  })
  for (let i = 0; i < entries.length && i < 10; i++) most.push(entries[i].tag + "(" + entries[i].n + ")")
  return [
    "记忆文件: " + s.path,
    "总数: " + store.records.length + " / 上限 " + s.maxRecords,
    "作用域: project=" + (scopes.get("project") || 0) + ", user=" + (scopes.get("user") || 0) +
      ", global=" + (scopes.get("global") || 0),
    "词条规模: 约 " + tokens + " 个词项",
    "常见标签: " + (most.length > 0 ? most.join(", ") : "(无)"),
    "更新时间: " + store.updatedAt,
  ].join("\n")
}

// ---------- 自动召回 ----------

// lastUserText 取最后一条用户消息的文本块。多步循环里最后一条 user 就是本轮输入,
// 工具结果不算查询,否则召回会被工具输出带偏。
function lastUserText(messages: any[]): string {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i]
    if (m === null || typeof m !== "object" || m.role !== "user") continue
    const content = Array.isArray(m.content) ? m.content : []
    let text = ""
    for (let j = 0; j < content.length; j++) {
      const block = content[j]
      if (block !== null && typeof block === "object" && typeof block.text === "string" &&
        (block.kind === "text" || block.kind === undefined)) {
        text += block.text + "\n"
      }
    }
    const trimmed = text.trim()
    if (trimmed !== "") return trimmed.length > 2000 ? trimmed.slice(0, 2000) : trimmed
  }
  return ""
}

// recallDigest 只产出「值得塞进提示」的短摘要:不复制正文、不带内部元数据,
// 并按 recallMaxChars 截断,避免自动召回把上下文预算吃光。
function recallDigest(s: Settings, query: string): string {
  const store = loadStore(s)
  const hits = rank(store.records, query, "", [], s.recallLimit)
  const lines: string[] = []
  let used = 0
  for (let i = 0; i < hits.length; i++) {
    const hit = hits[i]
    if (hit.score < s.recallMinScore) continue
    const rec = hit.record
    let line = "- #" + shortId(rec.id) + " [" + rec.scope + ", p" + rec.importance + "] " +
      summary(rec.text, 160)
    if (used + line.length > s.recallMaxChars) {
      if (lines.length === 0) line = line.slice(0, s.recallMaxChars)
      else break
    }
    lines.push(line)
    used += line.length
  }
  if (lines.length === 0) return ""
  return "【长期记忆】以下是与当前请求相关的记忆,引用时以它们为准;不相关可以忽略:\n" +
    lines.join("\n")
}

// applyAutoRecall 是 llm/request 的中间件:先让下游处理请求,再按需插入一条
// system 消息。召回失败不能让模型请求失败,所以异常只记日志并原样返回。
//
// 注意:JS 返回的是 JSON 对象;jtsr(执行层)会把它还原成 *llm.Request,
// 因此 agent-loop 能看到新增的 system 消息(见 ada_jsrt/jsrt/waterfall_coerce.go)。
function applyAutoRecall(s: Settings, ctx: Ctx, req: any, next: (v: any) => any): any {
  const out = next(req)
  if (!s.autoRecall) return out
  try {
    const base = out !== null && typeof out === "object" ? out : req
    if (base === null || typeof base !== "object") return out
    const messages = Array.isArray(base.messages) ? base.messages : null
    if (messages === null) return out
    const query = lastUserText(messages)
    if (query === "") return out
    // 已经注入过(同一请求被重复派发)就不要重复占预算。
    for (let i = 0; i < messages.length; i++) {
      const m = messages[i]
      if (m === null || typeof m !== "object" || m.role !== "system") continue
      const blocks = Array.isArray(m.content) ? m.content : []
      for (let j = 0; j < blocks.length; j++) {
        const b = blocks[j]
        if (b !== null && typeof b === "object" && typeof b.text === "string" &&
          b.text.indexOf("【长期记忆】") === 0) return out
      }
    }
    const digest = recallDigest(s, query)
    if (digest === "") return out
    // 插在首条 system(角色/规则)之后,保持系统提示在最前面。
    const at = messages.length > 0 ? 1 : 0
    messages.splice(at, 0, { role: "system", content: [{ kind: "text", text: digest }] })
    return out
  } catch (err) {
    ctx.log("memory auto-recall 跳过: " + errorText(err))
    return out
  }
}

// ---------- 注册 ----------

export function apply(ctx: Ctx, config?: Config): void {
  const s = normalizeConfig(config)
  ctx.log("memory-helper 装配完成(memory=" + s.path + ", maxRecords=" + s.maxRecords +
    ", autoRecall=" + (s.autoRecall ? "on" : "off") + ")")

  if (s.autoRecall) {
    if (typeof ctx.waterfall !== "function") throw new Error("autoRecall 需要宿主提供 ctx.waterfall")
    ctx.waterfall("llm/request", (req: any, next: (v: any) => any) => applyAutoRecall(s, ctx, req, next))
  }

  ctx.tool(
    {
      name: "memory_write",
      summary: "记住一条长期事实/偏好;重复或近似内容会合并到已有记忆",
      input: {
        type: "object",
        required: ["text"],
        properties: {
          text: { type: "string", minLength: 1, description: "要记住的内容,一句完整陈述" },
          tags: { type: "array", items: { type: "string" }, description: "可选标签,最多 8 个" },
          scope: { type: "string", enum: ["project", "user", "global"], description: "作用域,默认 project" },
          importance: { type: "integer", minimum: 1, maximum: 5, description: "重要度 1-5,默认 3" },
          pinned: { type: "boolean", description: "固定该记忆,不参与容量/文件上限自动淘汰" },
        },
      },
    },
    (args: any) => memoryWrite(s, args),
  )

  ctx.tool(
    {
      name: "memory_search",
      summary: "按关键词检索长期记忆,返回相关度最高的若干条",
      readOnly: true,
      input: {
        type: "object",
        required: ["query"],
        properties: {
          query: { type: "string", minLength: 1, description: "检索词,中文/英文均可" },
          scope: { type: "string", enum: ["project", "user", "global"], description: "只看某个作用域" },
          tags: { type: "array", items: { type: "string" }, description: "只返回同时带这些标签的记忆" },
          limit: { type: "integer", minimum: 1, maximum: 50, description: "返回条数上限" },
        },
      },
    },
    (args: any) => memorySearch(s, args),
  )

  ctx.tool(
    {
      name: "memory_read",
      summary: "按 id(或唯一前缀)读取一条记忆的完整内容",
      readOnly: true,
      input: {
        type: "object",
        required: ["id"],
        properties: {
          id: { type: "string", minLength: 4, description: "记忆 id,可用搜索结果里的短 id 前缀" },
        },
      },
    },
    (args: any) => memoryRead(s, args),
  )

  ctx.tool(
    {
      name: "memory_list",
      summary: "按重要度与时间列出长期记忆(支持分页)",
      readOnly: true,
      input: {
        type: "object",
        properties: {
          scope: { type: "string", enum: ["project", "user", "global"], description: "只看某个作用域" },
          tags: { type: "array", items: { type: "string" }, description: "只列同时带这些标签的记忆" },
          limit: { type: "integer", minimum: 1, maximum: 100, description: "每页条数,默认 20" },
          offset: { type: "integer", minimum: 0, description: "跳过条数,默认 0" },
        },
      },
    },
    (args: any) => memoryList(s, args),
  )

  ctx.tool(
    {
      name: "memory_update",
      summary: "修正一条记忆的正文/标签/作用域/重要度",
      input: {
        type: "object",
        required: ["id"],
        properties: {
          id: { type: "string", minLength: 4, description: "记忆 id 或唯一前缀" },
          text: { type: "string", minLength: 1, description: "新的正文" },
          tags: { type: "array", items: { type: "string" }, description: "替换标签" },
          scope: { type: "string", enum: ["project", "user", "global"], description: "新的作用域" },
          importance: { type: "integer", minimum: 1, maximum: 5, description: "新的重要度" },
          pinned: { type: "boolean", description: "是否固定该记忆(不参与自动淘汰)" },
        },
      },
    },
    (args: any) => memoryUpdate(s, args),
  )

  ctx.tool(
    {
      name: "memory_forget",
      summary: "删除一条长期记忆(物理删除,不可恢复)",
      input: {
        type: "object",
        required: ["id"],
        properties: {
          id: { type: "string", minLength: 4, description: "记忆 id 或唯一前缀" },
        },
      },
    },
    (args: any) => memoryForget(s, args),
  )

  ctx.tool(
    {
      name: "memory_stats",
      summary: "查看长期记忆的数量、作用域分布与常见标签",
      readOnly: true,
      input: { type: "object", properties: {} },
    },
    () => memoryStats(s),
  )
}
