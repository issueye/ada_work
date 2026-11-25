// skill-helper:把「技能」(SKILL.md + 附带文件)暴露成模型工具。
//
// 技能是数据不是代码:每个技能一个目录,里面放 SKILL.md(带 YAML 头),
// 可以有 references/、scripts/ 等附带文件。本插件只做扫描、解析、读取与生成,
// 实际文件操作全部经宿主注入的 ada:fs;路径限制、只读与沙箱由 fs 策略负责。

import { list, read, write } from "ada:fs"

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
type Entry = { Path: string; Name: string; Kind: string; Size: number }

// 默认扫描两处:工作区根的 skills/,以及 .ada/skills(隐藏目录更贴近本机习惯)。
const DEFAULT_ROOTS = ["/workspace/skills", "/workspace/.ada/skills"]
const MAX_SKILLS = 200
const MAX_BODY = 24 * 1024
const MAX_FILES = 60

// —— 路径与解析 ——

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

// asVirtual 只做词法归一:绝对路径原样,相对路径挂到 /workspace。
function asVirtual(value: unknown): string {
  if (typeof value !== "string" || value.trim() === "") throw new Error("path 不能为空")
  const p = value.trim()
  if (p.includes("\u0000")) throw new Error("path 非法")
  return p.startsWith("/") ? p : "/workspace/" + p
}

function rootsOf(config: any): string[] {
  const raw = config?.roots
  if (!Array.isArray(raw) || raw.length === 0) return DEFAULT_ROOTS
  const out: string[] = []
  for (const item of raw) {
    if (typeof item !== "string" || item.trim() === "") continue
    out.push(asVirtual(item))
  }
  return out.length === 0 ? DEFAULT_ROOTS : out
}

type Frontmatter = { name: string; description: string; body: string }

// parseSkill 解析 SKILL.md:开头 --- 与结尾 --- 之间是简单 key: value;
// 没有头部时整篇都是正文。description 支持引号与单行折行。
function parseSkill(raw: string): Frontmatter {
  const text = raw.replace(/\r\n/g, "\n").replace(/^\uFEFF/, "")
  const meta: Record<string, string> = {}
  let body = text
  if (text.startsWith("---\n")) {
    const end = text.indexOf("\n---", 4)
    if (end >= 0) {
      const head = text.slice(4, end)
      body = text.slice(end + 4).replace(/^\n+/, "")
      for (const line of head.split("\n")) {
        const m = /^([A-Za-z0-9_.-]+)\s*:\s*(.*)$/.exec(line.trim())
        if (m === null) continue
        let value = m[2].trim()
        if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
          value = value.slice(1, -1)
        }
        meta[m[1].toLowerCase()] = value
      }
    }
  }
  return {
    name: (meta["name"] || "").trim(),
    description: (meta["description"] || "").trim(),
    body,
  }
}

type Skill = {
  id: string
  name: string
  description: string
  path: string
  dir: string
}

// scan 扫描所有根目录的下一层:每个目录里找 SKILL.md(或 skill.md)即为一个技能。
function scan(roots: string[]): { skills: Skill[]; skipped: string[] } {
  const skills: Skill[] = []
  const skipped: string[] = []
  for (const root of roots) {
    let entries: Entry[]
    try {
      entries = list(root, false, MAX_SKILLS) as Entry[]
    } catch (err) {
      const msg = errorText(err)
      if (!/not found|no such|ENOENT|不存在/i.test(msg)) skipped.push(root + ": " + msg)
      continue
    }
    for (const entry of entries) {
      if (entry.Kind !== "dir") continue
      const dir = (entry.Path || root + "/" + entry.Name).replace(/\/+$/, "")
      let fileName = ""
      let raw = ""
      for (const candidate of ["SKILL.md", "skill.md"]) {
        try {
          raw = read(dir + "/" + candidate)
          fileName = candidate
          break
        } catch (err) {
          continue
        }
      }
      if (fileName === "") {
        skipped.push(dir + ": 缺少 SKILL.md")
        continue
      }
      const fm = parseSkill(raw)
      skills.push({
        id: entry.Name,
        name: fm.name || entry.Name,
        description: fm.description,
        path: dir + "/" + fileName,
        dir,
      })
    }
  }
  skills.sort((a, b) => a.name.localeCompare(b.name))
  return { skills, skipped }
}

function findSkill(roots: string[], name: unknown): Skill {
  if (typeof name !== "string" || name.trim() === "") throw new Error("name 不能为空")
  const want = name.trim().toLowerCase()
  const { skills } = scan(roots)
  for (const s of skills) {
    if (s.id.toLowerCase() === want || s.name.toLowerCase() === want) return s
  }
  const names = skills.map((s) => s.name + "(" + s.id + ")").join(", ")
  throw new Error("找不到技能 " + name + ";可用技能: " + (names === "" ? "(无)" : names))
}

// format 把技能渲染成模型可读的几行:名称、目录、描述。
function formatOne(s: Skill): string {
  const desc = s.description === "" ? "(没有描述)" : s.description
  return "- " + s.name + " (" + s.id + "): " + desc + "  [" + s.dir + "]"
}

function siblings(s: Skill): string[] {
  try {
    const entries = list(s.dir, true, MAX_FILES) as Entry[]
    const out: string[] = []
    for (const e of entries) {
      if (e.Kind === "dir") continue
      const rel = e.Path.startsWith(s.dir + "/") ? e.Path.slice(s.dir.length + 1) : e.Name
      if (e.Name === "SKILL.md" || e.Name === "skill.md") continue
      out.push(rel)
    }
    return out
  } catch (err) {
    return []
  }
}

// withinSkill 只允许读技能目录内的相对文件,拒绝 ..、绝对路径与空值。
function withinSkill(s: Skill, file: unknown): string {
  if (typeof file !== "string" || file.trim() === "") throw new Error("file 不能为空")
  const rel = file.trim()
  if (rel.startsWith("/") || rel.includes("..")) throw new Error("file 必须是技能目录内的相对路径")
  return s.dir + "/" + rel
}

// slugify 把技能名变成目录名:小写、空格与下划线转 -,只留字母数字与 -。
function slugify(name: string): string {
  const s = name
    .toLowerCase()
    .replace(/[\s_]+/g, "-")
    .replace(/[^a-z0-9-]/g, "")
    .replace(/-+/g, "-")
    .replace(/^-(.*)-$/, "$1")
  return s
}

function truncate(s: string, limit: number): string {
  if (s.length <= limit) return s
  return s.slice(0, limit) + "\n…(已截断,原文 " + s.length + " 字符)"
}

export function apply(ctx: Ctx, config?: any): void {
  const roots = rootsOf(config)
  ctx.log("skill-helper(ts) 装配完成,技能根: " + roots.join(", "))

  ctx.tool(
    {
      name: "skill_list",
      summary: "列出可用技能(SKILL.md 的名称与一句话描述)",
      readOnly: true,
      timeoutMs: 10000,
      input: {
        type: "object",
        properties: {
          query: { type: "string", description: "只显示名称/描述/目录名包含该词的技能" },
        },
      },
    },
    (args: any) => {
      const { skills, skipped } = scan(roots)
      const q = typeof args?.query === "string" ? args.query.trim().toLowerCase() : ""
      const shown = skills.filter(
        (s) => q === "" || s.name.toLowerCase().includes(q) || s.id.toLowerCase().includes(q) || s.description.toLowerCase().includes(q),
      )
      if (shown.length === 0) {
        return "没有技能。在 skills/<id>/SKILL.md 放置技能文件;扫描根: " + roots.join(", ")
      }
      const lines = shown.map(formatOne)
      if (skipped.length > 0) lines.push("跳过的目录: " + skipped.join("; "))
      return "技能(" + shown.length + "):\n" + lines.join("\n")
    },
  )

  ctx.tool(
    {
      name: "skill_show",
      summary: "读取一个技能的 SKILL.md 正文或其附带文件",
      readOnly: true,
      timeoutMs: 15000,
      input: {
        type: "object",
        required: ["name"],
        properties: {
          name: { type: "string", description: "技能名或目录名" },
          file: { type: "string", description: "可选:技能目录内的相对文件,如 references/note.md" },
        },
      },
    },
    (args: any) => {
      const s = findSkill(roots, args?.name)
      if (typeof args?.file === "string" && args.file.trim() !== "") {
        const p = withinSkill(s, args.file)
        try {
          return truncate(read(p), MAX_BODY)
        } catch (err) {
          throw new Error("读取技能文件 " + p + " 失败: " + errorText(err))
        }
      }
      const fm = parseSkill(
        (() => {
          try {
            return read(s.path)
          } catch (err) {
            throw new Error("读取技能 " + s.id + " 失败: " + errorText(err))
          }
        })(),
      )
      const files = siblings(s)
      const head =
        "技能: " + (fm.name || s.id) + " (" + s.id + ")\n" +
        "描述: " + (fm.description || "(没有描述)") + "\n" +
        "路径: " + s.path + "\n"
      const tail =
        (files.length > 0 ? "\n附带文件: " + files.join(", ") + "\n" : "") +
        "\n---\n" +
        truncate(fm.body.trim(), MAX_BODY)
      return head + tail
    },
  )

  ctx.tool(
    {
      name: "skill_scaffold",
      summary: "新建一个技能目录与 SKILL.md(不覆盖已有技能)",
      timeoutMs: 15000,
      input: {
        type: "object",
        required: ["name"],
        properties: {
          name: { type: "string", description: "技能显示名,如 PDF 处理" },
          description: { type: "string", description: "一句话说明什么时候用这个技能" },
          id: { type: "string", description: "目录名(可选,默认由名称生成)" },
        },
      },
    },
    (args: any) => {
      const name = typeof args?.name === "string" ? args.name.trim() : ""
      if (name === "") throw new Error("name 不能为空")
      const id = (typeof args?.id === "string" && args.id.trim() !== "" ? args.id.trim() : slugify(name))
      if (id === "" || !/^[a-z0-9][a-z0-9-]*$/.test(id)) {
        throw new Error("技能目录名必须是小写字母/数字/-: " + id)
      }
      const root = roots[0]
      const dir = root + "/" + id
      const path = dir + "/SKILL.md"
      try {
        read(path)
        throw new Error("技能已存在: " + path)
      } catch (err) {
        const msg = errorText(err)
        if (/已存在/.test(msg)) throw err
        if (!/not found|no such|ENOENT|不存在/i.test(msg)) throw new Error("检查 " + path + " 失败: " + msg)
      }
      const description = typeof args?.description === "string" ? args.description.trim() : ""
      const content =
        "---\n" +
        "name: " + name + "\n" +
        "description: " + description + "\n" +
        "---\n\n" +
        "# " + name + "\n\n" +
        (description === "" ? "" : description + "\n\n") +
        "在这里写触发条件、步骤与注意事项;附带文件可放在 references/、scripts/ 下。\n"
      try {
        write(path, content, true)
      } catch (err) {
        throw new Error("写入 " + path + " 失败: " + errorText(err))
      }
      return "已创建技能 " + path + "\n提示: skill_show name=" + id + " 可回读;要把它是为默认技能,下次重启进程即可。"
    },
  )
}
