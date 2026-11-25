// plugin-helper 的 TS 实现:给模型一组「看插件、生成插件、改 preset」的工具。
//
// 归属与边界:
//   - 它是 tool 插件,只把文件操作与 JSON 编辑包成工具,不实现装载/校验机制;
//   - 文件读写全部走 `ada:fs`(fs seam 的装饰器链),因此 fs-sandbox / workspace-guard
//     仍然决定「能不能写、写到哪」;
//   - **只生成 TS 插件**:TS 是运行期能从目录装载的唯一形态,生成即装配;
//     Go 插件要注册进 builtins 并重新编译,不在本工具范围(只读体检仍可看它的清单)。
import { read, write, list } from "ada:fs"

type ToolSpec = {
  name: string
  summary?: string
  input?: Record<string, unknown>
  readOnly?: boolean
  timeoutMs?: number
}

type ToolResult = {
  status?: "ok" | "denied" | "error" | "timeout"
  content?: string
  details?: unknown
}

type Ctx = {
  log: (msg: string) => void
  tool: (spec: ToolSpec, handler: (args: any) => ToolResult) => void
}

type Entry = { Path: string; Name: string; Kind: string; Size: number }
type Manifest = {
  id?: string
  name?: string
  version?: string
  kind?: string
  entry?: string
  artifact?: string
  inject?: { required?: string[]; optional?: string[] }
  provide?: string[]
  config?: Record<string, unknown>
  disabled?: boolean
  [key: string]: unknown
}

const ID_RE = /^[a-z0-9][a-z0-9-]{0,47}$/
const MAX_FOUND = 500

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

function fail(msg: string): never {
  throw new Error(msg)
}

// asPath 只做词法归一:绝对路径原样,相对路径挂到 /workspace。
// 真实的越界/只读/沙箱判定由 fs 实现与策略负责。
function asPath(value: unknown, fallback: string): string {
  if (value === undefined || value === null || value === "") return fallback
  if (typeof value !== "string") fail("path 必须是字符串")
  const p = value.trim().replace(/\\/g, "/")
  if (p === "") return fallback
  if (p.indexOf("\u0000") >= 0) fail("path 非法")
  return p.startsWith("/") ? p : "/workspace/" + p.replace(/^\.\//, "")
}

function parentOf(p: string): string {
  const at = p.lastIndexOf("/")
  if (at <= 0) return "/workspace"
  return p.slice(0, at)
}

function baseName(p: string): string {
  const at = p.lastIndexOf("/")
  return at < 0 ? p : p.slice(at + 1)
}

function joinPath(a: string, b: string): string {
  if (b.startsWith("/")) return b
  return a.replace(/\/+$/, "") + "/" + b
}

// listNames 列出目录一层的名字;目录不存在时返回空数组而不是抛错。
function listNames(dir: string): Entry[] {
  try {
    return list(dir, false, MAX_FOUND) as Entry[]
  } catch (err) {
    return []
  }
}

function readText(path: string): string {
  return read(path)
}

// parseManifest 读清单;JSON 坏了直接报错,不猜。
function parseManifest(path: string): Manifest {
  const raw = readText(path)
  let obj: any
  try {
    obj = JSON.parse(raw)
  } catch (err) {
    fail(path + " 不是合法 JSON: " + errorText(err))
  }
  if (obj === null || typeof obj !== "object" || Array.isArray(obj)) {
    fail(path + " 的顶层必须是对象")
  }
  return obj as Manifest
}

function manifestText(m: Manifest): string {
  return JSON.stringify(m, null, 2)
}

function injectText(m: Manifest): string {
  const req = m.inject?.required ?? []
  const opt = m.inject?.optional ?? []
  const parts: string[] = []
  if (req.length > 0) parts.push(req.join(","))
  if (opt.length > 0) parts.push("?" + opt.join(","))
  return parts.join(" | ")
}

// scanPlugins 从 root 往下找 plugin.json。只扫两层目录:
// 插件目录通常在 <root>/<id>/plugin.json 或 <root>/<group>/<id>/plugin.json。
function scanPlugins(root: string): Array<{ dir: string; manifest: Manifest }> {
  const found: Array<{ dir: string; manifest: Manifest }> = []
  // 根目录不存在时按「没有插件」处理:首次使用不该是一个错误。
  let first: Entry[]
  try {
    first = list(root, false, MAX_FOUND) as Entry[]
  } catch (err) {
    return found
  }
  for (const e of first) {
    if (e.Kind !== "dir") continue
    if (e.Name === "node_modules" || e.Name.startsWith(".")) continue
    const direct = joinPath(e.Path, "plugin.json")
    if (hasFile(e.Path, "plugin.json")) {
      found.push({ dir: e.Path, manifest: parseManifest(direct) })
      continue
    }
    for (const sub of listNames(e.Path)) {
      if (sub.Kind !== "dir") continue
      if (sub.Name === "node_modules" || sub.Name.startsWith(".")) continue
      if (!hasFile(sub.Path, "plugin.json")) continue
      found.push({ dir: sub.Path, manifest: parseManifest(joinPath(sub.Path, "plugin.json")) })
    }
  }
  return found
}

// hasFile 判断目录里有没有同名条目。
// fs seam 的 Kind 是 text/binary/image/dir/other(不是 "file"),所以「不是目录」即文件。
function hasFile(dir: string, name: string): boolean {
  return listNames(dir).some((e) => e.Kind !== "dir" && e.Name === name)
}

// checkManifest 做静态体检:清单形状、目录名、入口文件、依赖声明。
// 不做编译校验——那属于 `ada check`/装配期的事,不在工具里重复。
function checkManifest(dir: string, m: Manifest): { oks: string[]; problems: string[] } {
  const oks: string[] = []
  const problems: string[] = []
  const dirName = baseName(dir)
  if (m.id === undefined || m.id === "") {
    problems.push("缺少 id")
  } else if (m.id !== dirName) {
    problems.push("id(" + m.id + ")与目录名(" + dirName + ")不一致")
  } else {
    oks.push("id 与目录同名")
  }
  if (m.version === undefined || m.version === "") problems.push("缺少 version")
  else oks.push("version=" + m.version)
  if (m.kind !== "go" && m.kind !== "ts") {
    problems.push("kind 必须是 go 或 ts,得到 " + String(m.kind))
  } else {
    oks.push("kind=" + m.kind)
  }
  if (m.kind === "ts") {
    const entry = typeof m.entry === "string" && m.entry !== "" ? m.entry : "plugin.ts"
    if (!hasFile(dir, entry)) problems.push("入口文件 " + entry + " 不存在")
    else oks.push("入口 " + entry)
  } else if (m.kind === "go") {
    oks.push("Go 插件:本工具只支持 TS 生成,这里仅做只读体检(需注册 builtins 后重编)")
  }
  const req = m.inject?.required ?? []
  for (const svc of req) {
    if (typeof svc !== "string" || svc === "") problems.push("inject.required 里有空项")
  }
  if (req.length > 0) oks.push("依赖 " + req.join(","))
  return { oks, problems }
}

// scaffoldTS 生成一个能被 preset 直接引用的 TS 插件(源码即产物)。
function scaffoldTS(id: string, name: string, summary: string, toolName: string): string {
  const toolDecl = toolName === "" ? "" : `
  ctx.tool(
    {
      name: "${toolName}",
      summary: "${summary}",
      input: {
        type: "object",
        properties: {
          text: { type: "string", description: "示例输入;换成这个插件真正需要的参数" },
        },
      },
    },
    (args: any) => {
      const text = typeof args?.text === "string" ? args.text : ""
      return { status: "ok", content: "${id}: " + (text === "" ? "(空输入)" : text) }
    },
  )`
  return `// ${name}:${summary}
//
// 这是 plugin-helper 生成的骨架:apply(ctx, config) 可重入、无模块级状态,
// 工具/服务的注册都必须在 ctx 里完成,卸载时由内核回收。
type ToolSpec = {
  name: string
  summary?: string
  input?: Record<string, unknown>
  readOnly?: boolean
  timeoutMs?: number
}

type ToolResult = {
  status?: "ok" | "denied" | "error" | "timeout"
  content?: string
  details?: unknown
}

type Ctx = {
  log: (msg: string) => void
  tool: (spec: ToolSpec, handler: (args: any) => ToolResult) => void
}

export function apply(ctx: Ctx, cfg?: Record<string, unknown>): void {
  ctx.log("${id} 装配完成")${toolDecl}
}
`
}

// scaffoldReadme 说明下一步:TS 插件生成即可装配,不需要改二进制。
function scaffoldReadme(id: string, name: string, summary: string): string {
  const next = `在 preset 里加一行:\n\n\`\`\`json\n"plugins": ["plugins", "plugins/${id}"]\n\`\`\`\n\n然后用 \`ada plan --preset <preset>\` 验证装配。`
  return `# ${id}

${name}:${summary}

由 \`plugin-helper\` 生成。

## 下一步

${next}

## 约定

- 注释用中文,标识符用英文;
- 一个插件只做一件事;跨插件协作走服务或事件,不 import 别的插件包;
- secret 不进 config/日志,按名字走 \`ada:secrets\`。
`
}

export function apply(ctx: Ctx): void {
  ctx.log("plugin-helper(ts) 装配完成")

  ctx.tool(
    {
      name: "plugin_list",
      summary: "列出插件目录下的插件清单(id/kind/依赖/入口)",
      readOnly: true,
      timeoutMs: 30000,
      input: {
        type: "object",
        properties: {
          root: { type: "string", description: "插件根目录,缺省 /workspace/plugins" },
        },
      },
    },
    (args: any): ToolResult => {
      const root = asPath(args?.root, "/workspace/plugins")
      try {
        const found = scanPlugins(root)
        if (found.length === 0) {
          return { status: "ok", content: root + " 下没有找到 plugin.json" }
        }
        const lines = ["插件(" + found.length + "):"]
        for (const p of found) {
          const m = p.manifest
          lines.push(
            "- " + String(m.id ?? "?") + "  [" + String(m.kind ?? "?") + "]" +
            "  v" + String(m.version ?? "?") +
            (injectText(m) !== "" ? "  inject=" + injectText(m) : "") +
            (p.dir !== joinPath(root, String(m.id ?? "")) ? "  " + p.dir : ""),
          )
        }
        return {
          status: "ok",
          content: lines.join("\n"),
          details: {
            root,
            plugins: found.map((p) => ({
              id: p.manifest.id ?? "",
              kind: p.manifest.kind ?? "",
              version: p.manifest.version ?? "",
              inject: p.manifest.inject ?? {},
              provide: p.manifest.provide ?? [],
              dir: p.dir,
            })),
          },
        }
      } catch (err) {
        return { status: "error", content: "列插件失败: " + errorText(err) }
      }
    },
  )

  ctx.tool(
    {
      name: "plugin_show",
      summary: "读一个插件的 plugin.json 并做静态体检(目录名/入口/依赖)",
      readOnly: true,
      timeoutMs: 30000,
      input: {
        type: "object",
        required: ["dir"],
        properties: {
          dir: { type: "string", description: "插件目录,例如 plugins/x-helper" },
        },
      },
    },
    (args: any): ToolResult => {
      const dir = asPath(args?.dir, "")
      if (dir === "") return { status: "error", content: "dir 不能为空" }
      try {
        if (!hasFile(dir, "plugin.json")) {
          return { status: "error", content: dir + " 下没有 plugin.json" }
        }
        const m = parseManifest(joinPath(dir, "plugin.json"))
        const check = checkManifest(dir, m)
        const lines = ["体检:"]
        for (const ok of check.oks) lines.push("ok: " + ok)
        for (const bad of check.problems) lines.push("问题: " + bad)
        return {
          status: check.problems.length > 0 ? "error" : "ok",
          content: "清单(" + dir + "/plugin.json):\n" + manifestText(m) + "\n\n" + lines.join("\n"),
          details: { dir, manifest: m, checks: check },
        }
      } catch (err) {
        return { status: "error", content: "读取插件失败: " + errorText(err) }
      }
    },
  )

  ctx.tool(
    {
      name: "plugin_scaffold",
      summary: "生成一个新的 TS 插件骨架(生成即可被 preset 装配)",
      timeoutMs: 30000,
      input: {
        type: "object",
        required: ["id"],
        properties: {
          id: { type: "string", description: "插件 id,小写 kebab-case,与目录同名" },
          dir: { type: "string", description: "目录,缺省 plugins/<id>" },
          name: { type: "string", description: "展示名,缺省取 id" },
          summary: { type: "string", description: "一句话说明,写进清单与注释" },
          kind: { type: "string", enum: ["ts"], description: "只支持 ts(Go 插件需注册 builtins 后重编,不在本工具范围)" },
          tool: { type: "string", description: "示例工具的名字;留空则不生成工具" },
          overwrite: { type: "boolean", description: "目录已存在时是否覆盖" },
        },
      },
    },
    (args: any): ToolResult => {
      const id = typeof args?.id === "string" ? args.id.trim() : ""
      if (!ID_RE.test(id)) {
        return { status: "error", content: "id 必须是小写 kebab-case(字母数字与 -),得到 " + JSON.stringify(id) }
      }
      const name = typeof args?.name === "string" && args.name.trim() !== "" ? args.name.trim() : id
      const summary = typeof args?.summary === "string" && args.summary.trim() !== "" ? args.summary.trim() : name
      // schema 的 enum 已经挡住 go;这里再挡一次,避免调用方绕过 schema 直接传参。
      if (args?.kind !== undefined && args.kind !== "ts") {
        return { status: "error", content: "plugin-helper 只生成 TS 插件;Go 插件必须注册进 builtins 并重新编译" }
      }
      const toolName = typeof args?.tool === "string" ? args.tool.trim() : ""
      const dir = asPath(args?.dir, "/workspace/plugins/" + id)
      const overwrite = args?.overwrite === true

      try {
        if (!overwrite && hasFile(dir, "plugin.json")) {
          return { status: "error", content: dir + " 已存在;要覆盖请传 overwrite=true" }
        }
        const files: Array<{ path: string; content: string }> = []
        const manifest: Manifest = {
          id,
          name,
          version: "0.1.0",
          kind: "ts",
          entry: "plugin.ts",
          inject: { required: toolName !== "" ? ["tools"] : [] },
        }
        files.push({ path: joinPath(dir, "plugin.json"), content: JSON.stringify(manifest, null, 2) + "\n" })
        files.push({ path: joinPath(dir, "plugin.ts"), content: scaffoldTS(id, name, summary, toolName) })
        files.push({ path: joinPath(dir, "README.md"), content: scaffoldReadme(id, name, summary) })
        for (const f of files) write(f.path, f.content, true)

        const next = "下一步:在 preset 的 plugins 里加 \"" + dir + "\",然后 ada plan --preset <preset> 验证。"
        return {
          status: "ok",
          content: "已生成 " + id + "(ts):\n- " + files.map((f) => f.path).join("\n- ") + "\n\n" + next,
          details: { id, kind: "ts", dir, files: files.map((f) => f.path) },
        }
      } catch (err) {
        return { status: "error", content: "生成插件失败: " + errorText(err) }
      }
    },
  )

  ctx.tool(
    {
      name: "plugin_preset",
      summary: "改 preset:加/去掉插件目录,或启用/禁用插件 id",
      timeoutMs: 30000,
      input: {
        type: "object",
        required: ["preset", "action"],
        properties: {
          preset: { type: "string", description: "preset 文件路径,例如 presets/agent-release.preset.json" },
          action: { type: "string", enum: ["add", "remove", "enable", "disable"] },
          id: { type: "string", description: "enable/disable 的插件 id(与清单 id 一致)" },
          dir: { type: "string", description: "add/remove 的插件目录;缺省 plugins/<id>" },
        },
      },
    },
    (args: any): ToolResult => {
      const presetPath = asPath(args?.preset, "")
      if (presetPath === "") return { status: "error", content: "preset 不能为空" }
      const action = typeof args?.action === "string" ? args.action : ""
      const id = typeof args?.id === "string" ? args.id.trim() : ""
      // preset 里的路径是**相对 preset 文件**的真实路径,不是 /workspace 虚拟路径;
      // 这里按原样写入,只在缺省时给 "plugins/<id>"。
      const dir = typeof args?.dir === "string" && args.dir.trim() !== ""
        ? args.dir.trim()
        : (id === "" ? "" : "plugins/" + id)
      const needID = action === "enable" || action === "disable"
      if (needID && id === "") return { status: "error", content: action + " 需要 id" }
      if (!needID && dir === "") return { status: "error", content: action + " 需要 dir" }

      try {
        const raw = readText(presetPath)
        let preset: any
        try {
          preset = JSON.parse(raw)
        } catch (err) {
          return { status: "error", content: presetPath + " 不是合法 JSON: " + errorText(err) }
        }
        if (preset === null || typeof preset !== "object" || Array.isArray(preset)) {
          return { status: "error", content: presetPath + " 的顶层必须是对象" }
        }
        const plugins: string[] = Array.isArray(preset.plugins) ? preset.plugins.map(String) : []
        const disable: string[] = Array.isArray(preset.disable) ? preset.disable.map(String) : []
        let changed = false
        switch (action) {
          case "add":
            if (plugins.indexOf(dir) < 0) { plugins.push(dir); changed = true }
            break
          case "remove":
            for (let i = plugins.length - 1; i >= 0; i--) { if (plugins[i] === dir) { plugins.splice(i, 1); changed = true } }
            break
          case "enable":
            for (let i = disable.length - 1; i >= 0; i--) { if (disable[i] === id) { disable.splice(i, 1); changed = true } }
            break
          case "disable":
            if (disable.indexOf(id) < 0) { disable.push(id); changed = true }
            break
          default:
            return { status: "error", content: "action 必须是 add/remove/enable/disable,得到 " + JSON.stringify(action) }
        }
        if (!changed) {
          return { status: "ok", content: presetPath + " 已经是目标状态(action=" + action + ")" }
        }
        preset.plugins = plugins
        preset.disable = disable
        write(presetPath, JSON.stringify(preset, null, 2) + "\n", false)
        return {
          status: "ok",
          content: "已更新 " + presetPath + "(" + action + ")\nplugins=" + JSON.stringify(plugins) + "\ndisable=" + JSON.stringify(disable),
          details: { preset: presetPath, action, plugins, disable },
        }
      } catch (err) {
        return { status: "error", content: "更新 preset 失败: " + errorText(err) }
      }
    },
  )
}
