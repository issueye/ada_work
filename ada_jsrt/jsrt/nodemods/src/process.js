// node:process —— 受限子集:只暴露宿主愿意给的状态。
//
// cwd 固定为虚拟工作区根 /workspace；env 只包含 EnvPermission 允许的键，
// 写 env 需要 EnvPermission.Set；process.exit 受 ExitPolicy 控制。
// uncaughtException / unhandledRejection 等事件由宿主通过 __adaProcessEmit 投递。
import { EventEmitter } from "node:events";

const info = globalThis.__adaOSInfo || {};
const emitter = new EventEmitter();
const processStart = Date.now();

const envTarget = Object.assign({}, globalThis.__adaEnv || {});
const envSetAllowed = globalThis.__adaEnvSet === true;

export const platform = info.platform || "";
export const arch = info.arch || "";
export const version = "v20.0.0";
export const versions = { node: "20.0.0", ada: "0.1.0", moejs: "" };
export const pid = 0;
export const env = new Proxy(envTarget, {
  set(target, key, value) {
    if (!envSetAllowed) {
      const err = new Error("process.env 写入被禁用");
      err.code = "ERR_ACCESS_DENIED";
      throw err;
    }
    target[String(key)] = String(value);
    return true;
  },
  deleteProperty(target, key) {
    if (!envSetAllowed) {
      const err = new Error("process.env 写入被禁用");
      err.code = "ERR_ACCESS_DENIED";
      throw err;
    }
    delete target[key];
    return true;
  },
});
export const argv = [];
export const execPath = "/ada";
export let exitCode = 0;

export function on(name, listener) {
  return emitter.on(name, listener);
}

export function once(name, listener) {
  return emitter.once(name, listener);
}

export function off(name, listener) {
  return emitter.off(name, listener);
}

export function emit(name, ...args) {
  return emitter.emit(name, ...args);
}

export function listeners(name) {
  return emitter.listeners(name);
}

export function listenerCount(name) {
  return emitter.listenerCount(name);
}

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
  return (Date.now() - processStart) / 1000;
}

export function hrtime(previous) {
  const elapsedMs = Date.now() - processStart;
  const seconds = Math.floor(elapsedMs / 1000);
  const nanoseconds = Math.floor((elapsedMs % 1000) * 1e6);
  if (Array.isArray(previous) && previous.length >= 2) {
    let ds = seconds - Number(previous[0]);
    let dn = nanoseconds - Number(previous[1]);
    if (dn < 0) {
      ds -= 1;
      dn += 1e9;
    }
    return [ds, dn];
  }
  return [seconds, nanoseconds];
}

hrtime.bigint = function () {
  return BigInt(Date.now() - processStart) * BigInt(1000000);
};

export function exit(code) {
  const value = code === undefined ? exitCode : Number(code) || 0;
  emitter.emit("exit", value);
  const fn = globalThis.__adaExit;
  if (typeof fn === "function") {
    fn(value);
    return;
  }
  const err = new Error("process.exit 被宿主禁用:脚本不能结束 Core");
  err.code = "ERR_ACCESS_DENIED";
  throw err;
}

// 宿主投递 uncaughtException / unhandledRejection 的入口。
globalThis.__adaProcessEmit = function __adaProcessEmit(name, payload) {
  emitter.emit(name, payload);
};

export default {
  platform,
  arch,
  version,
  versions,
  pid,
  env,
  argv,
  execPath,
  exitCode,
  on,
  once,
  off,
  emit,
  listeners,
  listenerCount,
  cwd,
  nextTick,
  uptime,
  hrtime,
  exit,
};
