// node:process —— 受限子集:只暴露宿主愿意给的状态。
//
// cwd 固定为虚拟工作区根 /workspace;env 为空(密钥与宿主环境不进入脚本);
// process.exit 抛异常而不是结束宿主进程;没有 kill/binding/dlopen/setuid。
const info = globalThis.__adaOSInfo || {};

export const platform = info.platform || "";
export const arch = info.arch || "";
export const version = "v20.0.0";
export const versions = { node: "20.0.0", ada: "0.1.0", moejs: "" };
export const pid = 0;
export const env = {};
export const argv = [];
export const execPath = "/ada";

export function cwd() {
  return "/workspace";
}

export function nextTick(fn, ...args) {
  if (typeof fn !== "function") {
    const err = new TypeError("The callback argument must be of type function");
    err.code = "ERR_INVALID_ARG_TYPE";
    throw err;
  }
  Promise.resolve().then(() => fn(...args));
}

export function uptime() {
  return 0;
}

export function hrtime() {
  return [0, 0];
}

export function exit() {
  const err = new Error("process.exit 被宿主禁用:脚本不能结束 Core");
  err.code = "ERR_ACCESS_DENIED";
  throw err;
}

export default {
  platform,
  arch,
  version,
  versions,
  pid,
  env,
  argv,
  execPath,
  cwd,
  nextTick,
  uptime,
  hrtime,
  exit,
};
