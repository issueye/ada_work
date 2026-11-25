// text-helper:把 fs seam 的读写/编辑/搜索/列目录暴露成模型工具。
//
// 这是仓库里**唯一**的文件工具实现(原 `plugins/tool/fs` 已并入本插件)。
// 路径限制、只读/沙箱由 fs 实现与策略负责;本插件只做参数校验、路径归一化
// 与结果格式化,实际文件操作全部经宿主注入模块 ada:fs。
import { read, write, edit, search, list } from "ada:fs"

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
}

type Match = { Path: string; Line: number; Text: string }
type Entry = { Path: string; Name: string; Kind: string; Size: number }

// asPath 只做词法归一:绝对路径原样,相对路径挂到 /workspace;
// 真实越界、符号链接与只读判定由 fs 实现与策略负责。
function asPath(value: unknown): string {
  if (typeof value !== "string" || value.trim() === "") throw new Error("path 不能为空")
  const p = value.trim()
  if (p.includes("\u0000")) throw new Error("path 非法")
  return p.startsWith("/") ? p : "/workspace/" + p
}

// clampLimit 给搜索/列目录一个有界默认值:不传就用 200,超过 1000 直接截到上限,
// 避免脚本用一个巨大 limit 把整个工作区拉进模型上下文。
function clampLimit(value: unknown, def: number, max: number): number {
  const n = Number(value)
  if (!Number.isFinite(n) || n <= 0) return def
  if (n > max) return max
  return Math.floor(n)
}

// utf8Len 数的是字节数而不是字符数:模型需要知道写入了多少字节。
function utf8Len(s: string): number {
  let n = 0
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c < 0x80) n += 1
    else if (c < 0x800) n += 2
    else if (c >= 0xd800 && c <= 0xdbff) {
      n += 4
      i++
    } else n += 3
  }
  return n
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

// fail 统一抛中文上下文错误:底层错误原样透出会丢掉"哪个操作、哪个路径失败"。
function fail(msg: string): never {
  throw new Error(msg)
}

export function apply(ctx: Ctx): void {
  ctx.log("text-helper(ts) 装配完成")

  ctx.tool(
    {
      name: "read_file",
      summary: "读取工作区内的文本文件",
      readOnly: true,
      timeoutMs: 30000,
      input: {
        type: "object",
        required: ["path"],
        properties: {
          path: { type: "string", description: "相对 /workspace 的路径,或以 / 开头的虚拟路径" },
        },
      },
    },
    (args: any) => {
      const path = asPath(args?.path)
      try {
        return read(path)
      } catch (err) {
        fail("读取 " + path + " 失败: " + errorText(err))
      }
    },
  )

  ctx.tool(
    {
      name: "write_file",
      summary: "写入工作区文本文件(原子写)",
      timeoutMs: 30000,
      input: {
        type: "object",
        required: ["path", "content"],
        properties: {
          path: { type: "string" },
          content: { type: "string" },
        },
      },
    },
    (args: any) => {
      const path = asPath(args?.path)
      if (typeof args?.content !== "string") fail("content 必须是字符串")
      try {
        write(path, args.content, true)
      } catch (err) {
        fail("写入 " + path + " 失败: " + errorText(err))
      }
      return "已写入 " + path + "(" + utf8Len(args.content) + " bytes)"
    },
  )

  ctx.tool(
    {
      name: "edit_file",
      summary: "精确替换文本(read-before-edit)",
      timeoutMs: 30000,
      input: {
        type: "object",
        required: ["path", "old", "new"],
        properties: {
          path: { type: "string" },
          old: { type: "string" },
          new: { type: "string" },
          all: { type: "boolean", description: "是否替换全部" },
        },
      },
    },
    (args: any) => {
      const path = asPath(args?.path)
      if (typeof args?.old !== "string" || args.old === "") fail("old 不能为空")
      if (typeof args?.new !== "string") fail("new 必须是字符串")
      try {
        const n = edit(path, args.old, args.new, args.all === true)
        return "已替换 " + n + " 处(" + path + ")"
      } catch (err) {
        fail("编辑 " + path + " 失败: " + errorText(err))
      }
    },
  )

  ctx.tool(
    {
      name: "search_files",
      summary: "在工作区搜索文本",
      readOnly: true,
      timeoutMs: 30000,
      input: {
        type: "object",
        required: ["query"],
        properties: {
          query: { type: "string" },
          path: { type: "string", description: "缺省为 /workspace" },
          limit: { type: "integer", description: "缺省 200,上限 1000" },
        },
      },
    },
    (args: any) => {
      if (typeof args?.query !== "string" || args.query.trim() === "") fail("query 不能为空")
      const path = typeof args?.path === "string" && args.path !== "" ? asPath(args.path) : "/workspace"
      const limit = clampLimit(args?.limit, 200, 1000)
      let matches: Match[]
      try {
        matches = search(args.query, path, limit) as Match[]
      } catch (err) {
        fail("搜索失败: " + errorText(err))
      }
      if (matches.length === 0) return "(无匹配)"
      return matches.map((m) => m.Path + ":" + m.Line + ": " + m.Text).join("\n")
    },
  )

  ctx.tool(
    {
      name: "list_dir",
      summary: "列出工作区目录",
      readOnly: true,
      timeoutMs: 30000,
      input: {
        type: "object",
        properties: {
          path: { type: "string", description: "缺省为 /workspace" },
          recursive: { type: "boolean" },
          limit: { type: "integer", description: "缺省 200,上限 1000" },
        },
      },
    },
    (args: any) => {
      const path = typeof args?.path === "string" && args.path !== "" ? asPath(args.path) : "/workspace"
      const recursive = args?.recursive === true
      const limit = clampLimit(args?.limit, 200, 1000)
      let entries: Entry[]
      try {
        entries = list(path, recursive, limit) as Entry[]
      } catch (err) {
        fail("列目录失败: " + errorText(err))
      }
      if (entries.length === 0) return "(空目录)"
      return entries.map((e) => e.Kind + " " + e.Path).join("\n")
    },
  )
}
