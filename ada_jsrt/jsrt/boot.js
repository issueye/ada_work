// ada_go_v2 运行时引导模块:把插件入口包装成「宿主可以等它结算」的形式。
//
// 为什么需要它:宿主在调用边界上驱动事件循环(docs/event-loop.md),必须知道一次调用
// 什么时候真正结束。async 函数返回的是 Promise,直接 Call 只能拿到 Promise 对象,
// 所以由这里调用插件函数、await 它的结果,再用宿主注入的 onOK/onErr 把值/错误交回去。
//
// 约定:
//   - 参数只走 JSON(跨边界不传引用,C3);ctx 对象是唯一的例外,按 Value 原样传;
//   - 入口结算时清掉本次调用遗留的定时器(见 ADR 0014 §2:未 await 的定时器不该
//     活到下一次调用,那会把授权面错位到别的 ctx 上);
//   - onOK/onErr 永不抛错(Go 侧把转换失败也走 onErr),所以这里不需要再兜异常。
import "ada:boot";
import * as entry from "ada:entry";

// 入口的导出原样透传:宿主的诊断(ada inspect)看到的仍是插件自己声明的表面。
export * from "ada:entry";

// text 取错误的人类可读形式:优先堆栈,让宿主的错误信息能定位到行。
function text(e) {
  if (e !== null && typeof e === "object" && typeof e.stack === "string") return e.stack;
  return String(e);
}

function clearPending() {
  const clear = globalThis.__adaClearPendingTimers;
  if (typeof clear === "function") clear();
}

// parseArgs 把宿主给的 JSON 数组展开成参数表。空串表示「没有参数」。
function parseArgs(argsJSON) {
  if (argsJSON === undefined || argsJSON === null || argsJSON === "") return [];
  const args = JSON.parse(argsJSON);
  return Array.isArray(args) ? args : [];
}

// call 调用一个函数并处理「同步返回 / thenable」两种形态。
function call(fn, thisArg, args, onOK, onErr) {
  let out;
  try {
    out = thisArg === undefined ? fn(...args) : fn.apply(thisArg, args);
  } catch (e) {
    clearPending();
    onErr(text(e));
    return;
  }
  if (out !== null && (typeof out === "object" || typeof out === "function") && typeof out.then === "function") {
    // 异步:结算后先清定时器再交回宿主。reject 也走同一条清理路径,
    // 否则一个失败的调用会把定时器留给下一次调用。
    out.then(
      (v) => {
        clearPending();
        onOK(v);
      },
      (e) => {
        clearPending();
        onErr(text(e));
      },
    );
    return;
  }
  clearPending();
  onOK(out);
}

// __adaInvoke 调用入口模块的具名导出。
//
// host 不是 undefined 时作为第一个参数插入(装配期的 apply(ctx, config) 就是这条路径)。
export function __adaInvoke(name, host, argsJSON, onOK, onErr) {
  const fn = entry[name];
  if (typeof fn !== "function") {
    clearPending();
    onErr('模块未导出 "' + name + '"');
    return;
  }
  let args;
  try {
    args = parseArgs(argsJSON);
  } catch (e) {
    clearPending();
    onErr("参数不是合法 JSON: " + text(e));
    return;
  }
  if (host !== undefined) args.unshift(host);
  call(fn, undefined, args, onOK, onErr);
}

// __adaInvokeFn 调用一个函数值:ctx.on 注册的处理器、服务方法、effect 清理函数。
//
// thisArg 用于服务方法(方法体里读 this.x 需要绑定到服务对象)。
export function __adaInvokeFn(fn, thisArg, argsJSON, onOK, onErr) {
  if (typeof fn !== "function") {
    clearPending();
    onErr("回调不是函数");
    return;
  }
  let args;
  try {
    args = parseArgs(argsJSON);
  } catch (e) {
    clearPending();
    onErr("参数不是合法 JSON: " + text(e));
    return;
  }
  call(fn, thisArg, args, onOK, onErr);
}

// 模块求值阶段(用户模块顶层)留下的定时器同样不该活到第一次调用。
// 此刻 boot 的模块体刚执行完:入口与它 import 的模块都已求值,而宿主还没开始调用。
clearPending();
