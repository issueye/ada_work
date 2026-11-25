// ada_go_v2 执行层的定时器记账模块(在插件模块求值之前安装)。
//
// 为什么要在 JS 里记账:eventloop 给出一组 Node 形状的定时器,但句柄不出现在 Go 侧
// (没有公开的清理接口),而「入口结算时把这次调用遗留的定时器清掉」这件事恰好发生在
// JS 里(boot.js 的结算回调)。见 docs/event-loop.md §3.2 与 ADR 0014 §2。
//
// 为什么挂在 globalThis:安装(本模块)与清除(boot.js)在不同模块里,用全局对象传
// 避免跨模块的模块级绑定被别处的 `export *` 干扰。
const pending = new Set();
globalThis.__adaPendingTimers = pending;

function track(handle) {
  // 常驻模式下定时器跨调用存活，记账既没有清理时机也会无限增长；
  // 直接返回原始句柄。
  if (globalThis.__adaResident === true) return handle;
  if (handle !== null && handle !== undefined) pending.add(handle);
  return handle;
}

const rawSetTimeout = globalThis.setTimeout;
const rawSetInterval = globalThis.setInterval;
const rawSetImmediate = globalThis.setImmediate;
const rawClearTimeout = globalThis.clearTimeout;
const rawClearInterval = globalThis.clearInterval;
const rawClearImmediate = globalThis.clearImmediate;

// 包装只改「返回值要先记账」,其余语义(参数、this、ref/unref、延迟截断)保持上游。
// 上游没装定时器时(理论上不会发生:事件循环总会安装)保持原样,不要伪造能力。
if (typeof rawSetTimeout === "function") {
  globalThis.setTimeout = function setTimeout(fn, delay, ...args) {
    return track(rawSetTimeout(fn, delay, ...args));
  };
}
if (typeof rawSetInterval === "function") {
  globalThis.setInterval = function setInterval(fn, delay, ...args) {
    return track(rawSetInterval(fn, delay, ...args));
  };
}
if (typeof rawSetImmediate === "function") {
  globalThis.setImmediate = function setImmediate(fn, ...args) {
    return track(rawSetImmediate(fn, ...args));
  };
}

// __adaClearPendingTimers 清掉本次调用创建、到入口结算时仍未到期的定时器。
// 用 try 包住每一次 clear:清错类别(用 clearTimeout 清 Immediate)在 Node 里是
// 静默忽略,这里也不能因为一个句柄失败就让整个调用失败。
globalThis.__adaClearPendingTimers = function __adaClearPendingTimers() {
  for (const handle of pending) {
    try {
      if (typeof rawClearTimeout === "function") rawClearTimeout(handle);
    } catch (_) {}
    try {
      if (typeof rawClearInterval === "function") rawClearInterval(handle);
    } catch (_) {}
    try {
      if (typeof rawClearImmediate === "function") rawClearImmediate(handle);
    } catch (_) {}
  }
  pending.clear();
};
