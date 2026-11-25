// node:timers —— 定时器的模块形态。
//
// 为什么只是转出全局函数:定时器由执行层的事件循环安装(docs/event-loop.md),
// 全局与模块必须是同一组函数——否则 `timers.setTimeout === setTimeout` 不成立,
// 且 boot_timers 的记账(结算时清理未 await 的定时器)会漏掉模块路径创建的定时器。
//
// 不提供:Node 的 enroll/_unrefActive 等内部接口,以及 timers/promises(单独模块)。
const g = globalThis;

export const setTimeout = g.setTimeout;
export const clearTimeout = g.clearTimeout;
export const setInterval = g.setInterval;
export const clearInterval = g.clearInterval;
export const setImmediate = g.setImmediate;
export const clearImmediate = g.clearImmediate;

export default {
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
  setImmediate,
  clearImmediate,
};
