// 脚本插件示例:git-helper。
//
// 功能来自宿主注入的 moejs 模块 `ada:shell`;插件本身只做参数校验与工具注册。
import { run } from "ada:shell"

type ToolSpec = {
  name: string
  summary?: string
  input?: Record<string, unknown>
  readOnly?: boolean
}
type Ctx = {
  log: (msg: string) => void
  tool: (spec: ToolSpec, handler: (args: any) => any) => void
}

function pathArg(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined
  const p = value.trim()
  if (p === "" || p.startsWith("-") || p.includes("\u0000")) return undefined
  return p
}

export function apply(ctx: Ctx): void {
  ctx.log("git-helper-ts 装配完成")

  ctx.tool(
    {
      name: "git_status",
      summary: "查看当前 git 分支与工作区状态",
      readOnly: true,
      input: { type: "object", properties: { path: { type: "string", description: "可选:限制到某个相对路径" } } },
    },
    (args: any) => {
      const argv = ["status", "--porcelain=v1", "--branch"]
      const p = pathArg(args?.path)
      if (p) argv.push("--", p)
      return run("git", argv, "/workspace")
    },
  )

  ctx.tool(
    {
      name: "git_diff",
      summary: "查看 git diff(可只取统计)",
      readOnly: true,
      input: {
        type: "object",
        properties: {
          path: { type: "string", description: "可选:限制到某个相对路径" },
          stat: { type: "boolean", description: "只返回 --stat 摘要" },
        },
      },
    },
    (args: any) => {
      const argv = ["diff"]
      if (args?.stat === true) argv.push("--stat")
      const p = pathArg(args?.path)
      if (p) argv.push("--", p)
      return run("git", argv, "/workspace")
    },
  )

  ctx.tool(
    {
      name: "git_log",
      summary: "查看最近的 git 提交(one-line)",
      readOnly: true,
      input: { type: "object", properties: { limit: { type: "integer", minimum: 1 } } },
    },
    (args: any) => {
      let limit = Number(args?.limit)
      if (!Number.isFinite(limit) || limit <= 0) limit = 10
      if (limit > 50) limit = 50
      return run("git", ["log", "--oneline", "-n", String(Math.trunc(limit))], "/workspace")
    },
  )
}