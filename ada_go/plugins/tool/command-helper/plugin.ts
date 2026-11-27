// 脚本插件示例:command-helper。
//
// 通过注入模块 ada:shell 执行受控命令;程序白名单写在插件配置里,
// 生产环境仍应叠加 shell-guard/sandbox/caps。
import { run } from "ada:shell"

type ToolSpec = { name: string; summary?: string; input?: Record<string, unknown>; readOnly?: boolean }
type Ctx = {
  log: (msg: string) => void
  tool: (spec: ToolSpec, handler: (args: any) => any) => void
}
type Config = { allow?: string[] }
type RunArgs = { program?: string; args?: unknown; dir?: string }

const DEFAULT_ALLOW = ["git", "go"]
const MAX_ARGS = 64

function checkDir(dir: unknown): string {
  if (dir === undefined) return "/workspace"
  if (typeof dir !== "string" || !dir.startsWith("/workspace")) {
    throw new Error("dir 必须以 /workspace 开头")
  }
  if (dir.includes("..") || dir.includes("\u0000")) throw new Error("dir 非法")
  return dir
}

function checkArgs(raw: unknown): string[] {
  if (raw === undefined) return []
  if (!Array.isArray(raw)) throw new Error("args 必须是字符串数组")
  if (raw.length > MAX_ARGS) throw new Error("args 过多")
  const out: string[] = []
  for (const item of raw) {
    if (typeof item !== "string") throw new Error("args 只能包含字符串")
    if (item.includes("\u0000") || item.includes("\n") || item.includes("\r")) {
      throw new Error("args 不能包含控制字符")
    }
    out.push(item)
  }
  return out
}

export function apply(ctx: Ctx, cfg: Config): void {
  const allow = new Set((cfg?.allow && cfg.allow.length > 0 ? cfg.allow : DEFAULT_ALLOW).map(String))
  ctx.log("command-helper-ts 装配完成,allow=" + Array.from(allow).join(","))

  ctx.tool(
    {
      name: "cmd_list",
      summary: "列出允许执行的程序",
      readOnly: true,
      input: { type: "object", properties: {} },
    },
    () => Array.from(allow).join("\n"),
  )

  ctx.tool(
    {
      name: "cmd_run",
      summary: "执行白名单内的命令(不经过 shell 解析)",
      readOnly: false,
      input: {
        type: "object",
        required: ["program"],
        properties: {
          program: { type: "string", description: "程序名,必须在 allow 白名单内" },
          args: { type: "array", items: { type: "string" }, description: "参数数组" },
          dir: { type: "string", description: "工作目录,默认 /workspace" },
        },
      },
    },
    (raw: RunArgs) => {
      const program = typeof raw?.program === "string" ? raw.program.trim() : ""
      if (program === "") throw new Error("program 不能为空")
      if (!allow.has(program)) throw new Error("程序不在白名单: " + program)
      const argv = checkArgs(raw?.args)
      const dir = checkDir(raw?.dir)
      const output = run(program, argv, dir)
      return output === "" ? "(无输出)" : output
    },
  )
}