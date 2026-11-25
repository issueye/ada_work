// node:timers/promises —— Promise 形态的定时器。
//
// 语义与 Node 相同:setTimeout(delay, value) 在 delay 毫秒后 resolve 成 value;
// setImmediate(value) 在下一个立即阶段 resolve;setInterval 是一个异步可迭代对象。
//
// 与 Node 的差别(写在这里,免得作者以为能随便用):
//   - 没有事件循环常驻:定时器必须在一次宿主调用内被 await,否则会在调用结束时被清掉;
//   - options.signal 只支持带 addEventListener 的 AbortSignal(AbortController 本身没有全局);
//   - setInterval 的迭代器没有 ref/unref,break/return 时清定时器。
const g = globalThis;

function abortError() {
  const err = new Error("The operation was aborted");
  err.name = "AbortError";
  err.code = "ABORT_ERR";
  return err;
}

// watchSignal 在 signal 中止时回调 onAbort,返回取消订阅函数。
function watchSignal(signal, onAbort) {
  if (signal === undefined || signal === null) return function () {};
  if (typeof signal.addEventListener !== "function") return function () {};
  if (signal.aborted === true) {
    onAbort();
    return function () {};
  }
  signal.addEventListener("abort", onAbort);
  return function () {
    if (typeof signal.removeEventListener === "function") signal.removeEventListener("abort", onAbort);
  };
}

export function setTimeout(delay, value, options) {
  return new Promise(function (resolve, reject) {
    let timer = null;
    let settled = false;
    const finish = function (fn, payload) {
      if (settled) return;
      settled = true;
      off();
      fn(payload);
    };
    timer = g.setTimeout(function () {
      finish(resolve, value);
    }, delay);
    const off = watchSignal(options && options.signal, function () {
      if (settled) return;
      settled = true;
      g.clearTimeout(timer);
      reject(abortError());
    });
  });
}

export function setImmediate(value, options) {
  return new Promise(function (resolve, reject) {
    let timer = null;
    let settled = false;
    const finish = function (fn, payload) {
      if (settled) return;
      settled = true;
      off();
      fn(payload);
    };
    timer = g.setImmediate(function () {
      finish(resolve, value);
    });
    const off = watchSignal(options && options.signal, function () {
      if (settled) return;
      settled = true;
      g.clearImmediate(timer);
      reject(abortError());
    });
  });
}

// setInterval 返回一个异步可迭代对象(Node 的形状):每个间隔产出一个 value。
//
// 不用 async generator:引擎对异步生成器的支持面比 Promise 窄,用手写迭代器更稳。
export function setInterval(delay, value, options) {
  let queue = [];
  let waiter = null;
  let stopped = false;
  let failure = null;

  const wake = function () {
    if (waiter === null) return;
    const w = waiter;
    waiter = null;
    w();
  };
  const stop = function () {
    if (stopped) return;
    stopped = true;
    clearInterval(timer);
    wake();
  };
  const timer = g.setInterval(function () {
    queue.push(value);
    wake();
  }, delay);
  const off = watchSignal(options && options.signal, function () {
    failure = abortError();
    stop();
  });

  const iterable = {
    next: function () {
      if (failure !== null) return Promise.reject(failure);
      if (queue.length > 0) return Promise.resolve({ value: queue.shift(), done: false });
      if (stopped) return Promise.resolve({ value: undefined, done: true });
      return new Promise(function (resolve, reject) {
        waiter = function () {
          if (failure !== null) {
            reject(failure);
            return;
          }
          if (queue.length > 0) {
            resolve({ value: queue.shift(), done: false });
            return;
          }
          resolve({ value: undefined, done: true });
        };
      });
    },
    return: function () {
      stop();
      off();
      return Promise.resolve({ value: undefined, done: true });
    },
    throw: function (err) {
      stop();
      off();
      return Promise.reject(err);
    },
  };
  iterable[Symbol.asyncIterator] = function () {
    return iterable;
  };
  return iterable;
}

export const scheduler = {
  wait: function (delay, options) {
    return setTimeout(delay, undefined, options);
  },
  yield: function () {
    return setImmediate(undefined);
  },
};

export default { setTimeout, setImmediate, setInterval, scheduler };
