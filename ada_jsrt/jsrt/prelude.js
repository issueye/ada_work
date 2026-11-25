// 运行时前置脚本：在入口模块求值之前定义 Node 风格的全局对象。
//
// 目前只补 AbortController / AbortSignal：moejs 没有安装 webapi，
// 而 timers/promises、fs、http 的 signal 选项都依赖它。这里是最小实现，
// 不冒充完整 WHATWG DOM 事件目标；AbortSignal 只提供 Node 常用面。
(function () {
  if (typeof globalThis.AbortController === "function" && typeof globalThis.AbortSignal === "function") {
    return;
  }

  function abortError(reason) {
    if (reason !== undefined) return reason;
    const err = new Error("This operation was aborted");
    err.name = "AbortError";
    err.code = "ABORT_ERR";
    return err;
  }

  class AbortSignal {
    constructor() {
      this.aborted = false;
      this.reason = undefined;
      this.onabort = null;
      this._listeners = new Set();
    }

    addEventListener(type, listener, options) {
      if (type !== "abort" || typeof listener !== "function") return;
      const entry = { listener, once: !!(options && options.once) };
      this._listeners.add(entry);
    }

    removeEventListener(type, listener) {
      if (type !== "abort") return;
      for (const entry of this._listeners) {
        if (entry.listener === listener) this._listeners.delete(entry);
      }
    }

    throwIfAborted() {
      if (this.aborted) throw this.reason;
    }

    _abort(reason) {
      if (this.aborted) return;
      this.aborted = true;
      this.reason = abortError(reason);
      const entries = Array.from(this._listeners);
      this._listeners.clear();
      if (typeof this.onabort === "function") {
        try {
          this.onabort.call(this, { type: "abort", target: this });
        } catch (_) {}
      }
      for (const entry of entries) {
        try {
          entry.listener.call(this, { type: "abort", target: this });
        } catch (_) {}
      }
    }

    static abort(reason) {
      const signal = new AbortSignal();
      signal._abort(reason);
      return signal;
    }

    static timeout(ms) {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(abortError()), Number(ms) || 0);
      if (timer && typeof timer.unref === "function") timer.unref();
      return controller.signal;
    }

    static any(signals) {
      const controller = new AbortController();
      for (const signal of signals || []) {
        if (!signal) continue;
        if (signal.aborted) {
          controller.abort(signal.reason);
          break;
        }
        signal.addEventListener("abort", () => controller.abort(signal.reason), { once: true });
      }
      return controller.signal;
    }
  }

  class AbortController {
    constructor() {
      this.signal = new AbortSignal();
    }

    abort(reason) {
      this.signal._abort(reason);
    }
  }

  globalThis.AbortController = AbortController;
  globalThis.AbortSignal = AbortSignal;

  if (typeof globalThis.queueMicrotask !== "function") {
    globalThis.queueMicrotask = function queueMicrotask(fn) {
      Promise.resolve().then(fn);
    };
  }
})();
