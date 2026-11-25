// node:child_process —— 受限子集:只有 execFile / execFileSync。
//
// 参数数组直接交给 shell seam,不经过 shell 解析;spawn / fork / exec / execSync
// 不存在(不是「存在但可绕过」)。cwd 必须是 /workspace 下的虚拟路径;
// 命令超时、取消与沙箱由 shell 实现/策略决定。
const host = globalThis.__adaHost;

function unsupported(name) {
  const err = new Error("child_process." + name + " 不提供:只支持 execFile/execFileSync");
  err.code = "ERR_NOT_SUPPORTED";
  return err;
}

function requireHost() {
  if (!host || typeof host.shellExec !== "function") {
    const err = new Error("Cannot import \"node:child_process\": shell 服务不可用");
    err.code = "ERR_ACCESS_DENIED";
    throw err;
  }
}

function normalizeCwd(cwd) {
  if (cwd === undefined || cwd === null || cwd === "") return "/workspace";
  if (typeof cwd !== "string") {
    const err = new TypeError("child_process cwd 必须是字符串");
    err.code = "ERR_INVALID_ARG_TYPE";
    throw err;
  }
  let p = cwd.trim().replace(/\\/g, "/");
  if (p === "") return "/workspace";
  if (!p.startsWith("/")) {
    p = "/workspace/" + p;
  }
  if (p !== "/workspace" && !p.startsWith("/workspace/")) {
    const err = new Error("child_process cwd 必须位于 /workspace 下: " + cwd);
    err.code = "ERR_INVALID_ARG_VALUE";
    throw err;
  }
  return p;
}

function run(file, args, options) {
  requireHost();
  if (typeof file !== "string" || file.trim() === "") {
    const err = new TypeError("child_process file 必须是非空字符串");
    err.code = "ERR_INVALID_ARG_TYPE";
    throw err;
  }
  if (options && options.shell) {
    const err = new Error("child_process 不允许 shell: 请用 execFile 的参数数组");
    err.code = "ERR_ACCESS_DENIED";
    throw err;
  }
  if (options && options.env !== undefined) {
    const err = new Error("child_process.options.env 暂不支持:环境变量由宿主策略管理");
    err.code = "ERR_NOT_SUPPORTED";
    throw err;
  }
  const argv = Array.isArray(args) ? args.map(String) : [];
  const cwd = normalizeCwd(options && options.cwd);
  let timeout = Number(options && options.timeout);
  if (!Number.isFinite(timeout) || timeout <= 0) timeout = 120000;
  const result = JSON.parse(host.shellExec(file.trim(), argv, cwd, JSON.stringify({ timeoutMs: timeout })));
  return result;
}

function commandError(file, args, result) {
  const err = new Error("Command failed: " + file + " " + args.join(" "));
  err.code = result.exitCode;
  err.status = result.exitCode;
  err.signal = null;
  err.stdout = result.stdout || "";
  err.stderr = result.stderr || "";
  return err;
}

export function execFileSync(file, args, options) {
  const result = run(file, args, options || {});
  if (result.exitCode !== 0) {
    throw commandError(file, args || [], result);
  }
  return result.stdout || "";
}

export function execFile(file, args, options, callback) {
  let argv = args;
  let opts = options;
  let cb = callback;
  if (typeof argv === "function") {
    cb = argv;
    argv = [];
    opts = {};
  } else if (typeof opts === "function") {
    cb = opts;
    opts = {};
  }
  opts = opts || {};
  const child = { pid: 0, stdout: null, stderr: null, killed: false, kill() { this.killed = true } };
  try {
    const result = run(file, argv, opts);
    if (typeof cb === "function") {
      const err = result.exitCode === 0 ? null : commandError(file, argv || [], result);
      queueMicrotask(() => cb(err, result.stdout || "", result.stderr || ""));
    }
  } catch (err) {
    if (typeof cb === "function") {
      queueMicrotask(() => cb(err, "", ""));
    } else {
      throw err;
    }
  }
  return child;
}

export const spawn = () => { throw unsupported("spawn"); };
export const fork = () => { throw unsupported("fork"); };
export const exec = () => { throw unsupported("exec"); };
export const execSync = () => { throw unsupported("execSync"); };

export default {
  execFile,
  execFileSync,
  spawn,
  fork,
  exec,
  execSync,
};
