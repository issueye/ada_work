// shell-helper 的 TS 实现:把 shell seam 暴露成模型工具 run_shell。
//
// 安全边界仍属于 shell 实现与策略(shell-guard/sandbox/approval),本插件只做
// 参数映射与结果整理。命令执行走宿主注入模块 ada:shell 的结构化 exec:
// 非零退出、stderr、截断与耗时由工具自己呈现,而不是被宿主当成异常吞掉。
import { exec, platform } from "ada:shell"

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
  tool: (spec: ToolSpec, handler: (args: RunArgs) => ToolResult) => void
}

type Config = {
  program?: string
  args?: string[]
  maxTimeoutSeconds?: number
}

type RunArgs = {
  command?: string
  dir?: string
  timeoutSeconds?: number
}

type ShellResult = {
  stdout: string
  stderr: string
  exitCode: number
  truncated: boolean
  durationMs: number
  enforced: boolean
  backend?: string
  mode?: string
}

const MAX_RESULT_BYTES = 64 << 10

// defaultShell 按平台给出默认 shell 与命令前缀;preset 可以覆盖。
function defaultShell(): { program: string; args: string[] } {
  if (platform() === "windows") {
    return { program: "cmd.exe", args: ["/C"] }
  }
  return { program: "/bin/sh", args: ["-c"] }
}

function virtualDir(raw: unknown): string {
  if (typeof raw !== "string") return "/workspace"
  const dir = raw.trim().replace(/\\/g, "/")
  if (dir === "") return "/workspace"
  if (dir.startsWith("/")) return dir
  return "/workspace/" + dir.replace(/^\.\//, "")
}

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

function outputText(res: ShellResult): string {
  const parts: string[] = []
  const stdout = res.stdout.trim()
  const stderr = res.stderr.trim()
  if (stdout !== "") parts.push(stdout)
  if (stderr !== "") parts.push("[stderr]\n" + stderr)
  if (parts.length === 0) parts.push("(无输出)")
  if (res.exitCode !== 0) parts.push("[exit " + res.exitCode + "]")
  if (res.truncated) parts.push("[输出已截断]")
  let text = parts.join("\n")
  if (utf8Len(text) > MAX_RESULT_BYTES) {
    text = truncateUTF8(text, MAX_RESULT_BYTES) + "\n[结果已截断]"
  }
  return text
}

function toolError(text: string): ToolResult {
  return { status: "error", content: text }
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message
  return String(err)
}

export function apply(ctx: Ctx, cfg?: Config): void {
  const conf = cfg ?? {}
  const fallback = defaultShell()
  const program = typeof conf.program === "string" && conf.program.trim() !== ""
    ? conf.program.trim()
    : fallback.program
  if (/\s/.test(program)) {
    throw new Error("program 不能包含空白")
  }
  const args = program === fallback.program && (conf.program === undefined || conf.program === "")
    ? fallback.args
    : Array.isArray(conf.args) ? conf.args.map((v) => String(v)) : []

  let maxTimeout = Number(conf.maxTimeoutSeconds)
  if (!Number.isFinite(maxTimeout) || maxTimeout <= 0) maxTimeout = 120
  if (maxTimeout > 1800) maxTimeout = 1800
  maxTimeout = Math.floor(maxTimeout)

  ctx.log("shell-helper(ts) 装配完成,program=" + program + ", maxTimeout=" + maxTimeout + "s")

  ctx.tool(
    {
      name: "run_shell",
      summary: "在工作区执行一条 shell 命令,返回 stdout/stderr 与退出码",
      readOnly: false,
      timeoutMs: (maxTimeout + 30) * 1000,
      input: {
        type: "object",
        properties: {
          command: {
            type: "string",
            description: "要执行的命令,经本机 shell 解析,支持管道与重定向",
          },
          dir: {
            type: "string",
            description: "相对工作区的目录;留空表示工作区根目录",
          },
          timeoutSeconds: {
            type: "integer",
            description: "最长运行秒数,默认 60",
          },
        },
        required: ["command"],
      },
    },
    (raw: RunArgs): ToolResult => {
      const command = typeof raw?.command === "string" ? raw.command.trim() : ""
      if (command === "") return toolError("command 不能为空")

      let timeout = Number(raw?.timeoutSeconds)
      if (!Number.isFinite(timeout) || timeout <= 0) timeout = 60
      if (timeout > maxTimeout) timeout = maxTimeout
      timeout = Math.floor(timeout)

      const dir = virtualDir(raw?.dir)
      try {
        const res = exec(program, args.concat(command), dir, { timeoutMs: timeout * 1000 })
        return {
          status: "ok",
          content: outputText(res),
          details: {
            program,
            dir,
            exitCode: res.exitCode,
            durationMs: res.durationMs,
            truncated: res.truncated,
            enforced: res.enforced,
            mode: res.mode ?? "",
          },
        }
      } catch (err) {
        return toolError("命令执行失败: " + errorText(err))
      }
    },
  )
}
