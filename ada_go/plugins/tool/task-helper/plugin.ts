// 脚本插件示例:task-helper。
//
// 功能由宿主注入的 moejs 模块 `ada:fs` 提供;任务持久化在工作区 .ada/tasks.json。
import { read, write } from "ada:fs"

type Task = { id: number; title: string; priority: number; status: "open" | "done" }
type ToolSpec = { name: string; summary?: string; input?: Record<string, unknown>; readOnly?: boolean }
type Ctx = {
  log: (msg: string) => void
  tool: (spec: ToolSpec, handler: (args: any) => any) => void
}

const FILE = "/workspace/.ada/tasks.json"

function load(): Task[] {
  try {
    const text = read(FILE)
    const parsed = JSON.parse(text)
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

function save(tasks: Task[]): void {
  write(FILE, JSON.stringify(tasks, null, 2), true)
}

function render(task: Task): string {
  return `#${task.id} [${task.status}] p${task.priority} ${task.title}`
}

export function apply(ctx: Ctx): void {
  ctx.log("task-helper-ts 装配完成")

  ctx.tool(
    {
      name: "task_add",
      summary: "新增一个任务",
      input: {
        type: "object",
        required: ["title"],
        properties: {
          title: { type: "string", description: "任务标题" },
          priority: { type: "integer", description: "1-5,默认 3" },
        },
      },
    },
    (args: any) => {
      const title = typeof args?.title === "string" ? args.title.trim() : ""
      if (title === "") throw new Error("title 不能为空")
      let priority = Number(args?.priority)
      if (!Number.isFinite(priority)) priority = 3
      priority = Math.max(1, Math.min(5, Math.trunc(priority)))
      const tasks = load()
      const id = tasks.reduce((max, t) => Math.max(max, t.id), 0) + 1
      const task: Task = { id, title, priority, status: "open" }
      tasks.push(task)
      save(tasks)
      return "已添加 " + render(task)
    },
  )

  ctx.tool(
    {
      name: "task_list",
      summary: "列出任务(默认只列未完成)",
      input: {
        type: "object",
        properties: { includeDone: { type: "boolean", description: "是否包含已完成任务" } },
      },
    },
    (args: any) => {
      const includeDone = args?.includeDone === true
      const tasks = load()
      const list = tasks.filter((t) => includeDone || t.status !== "done")
      if (list.length === 0) return "(没有任务)"
      return list.sort((a, b) => a.id - b.id).map(render).join("\n")
    },
  )

  ctx.tool(
    {
      name: "task_done",
      summary: "把任务标记为完成",
      input: {
        type: "object",
        required: ["id"],
        properties: { id: { type: "integer", description: "任务 id" } },
      },
    },
    (args: any) => {
      const id = Number(args?.id)
      const tasks = load()
      const task = tasks.find((t) => t.id === id)
      if (!task) throw new Error("任务不存在: #" + id)
      task.status = "done"
      save(tasks)
      return "已完成 " + render(task)
    },
  )

  ctx.tool(
    {
      name: "task_remove",
      summary: "删除一个任务",
      input: {
        type: "object",
        required: ["id"],
        properties: { id: { type: "integer", description: "任务 id" } },
      },
    },
    (args: any) => {
      const id = Number(args?.id)
      const tasks = load()
      const idx = tasks.findIndex((t) => t.id === id)
      if (idx < 0) throw new Error("任务不存在: #" + id)
      const removed = tasks.splice(idx, 1)[0]
      save(tasks)
      return "已删除 " + render(removed)
    },
  )
}