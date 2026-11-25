// node:perf_hooks —— 轻量的性能度量。
//
// now() 走宿主的单调时钟(__adaHost.monoMs,相对运行时启动),不是 Date.now():
// 用墙钟做耗时统计会随系统时间调整跳变。
//
// 实现范围:performance.now/timeOrigin/mark/measure/getEntries*/clear*、PerformanceObserver
// (条目通过微任务投递,与 Node 的异步投递形状一致)、PerformanceEntry/PerformanceMark/
// PerformanceMeasure 三个数据形状。不做:User Timing 的 detail 序列化、GC/资源时序、
// eventLoopUtilization 与 histogram。
const host = globalThis.__adaHost;

// defer 优先用 queueMicrotask;引擎没装时退到 promise job(语义相同,都早于下一个宏任务)。
const defer =
  typeof queueMicrotask === "function"
    ? queueMicrotask
    : function (fn) {
        Promise.resolve().then(fn);
      };

const monoNow =
  host && typeof host.monoMs === "function"
    ? function () {
        return host.monoMs();
      }
    : function () {
        return Date.now() - timeOrigin;
      };

const timeOrigin = Date.now() - (host && typeof host.monoMs === "function" ? host.monoMs() : 0);

class PerformanceEntry {
  constructor(name, entryType, startTime, duration) {
    this.name = name;
    this.entryType = entryType;
    this.startTime = startTime;
    this.duration = duration;
  }
  toJSON() {
    return { name: this.name, entryType: this.entryType, startTime: this.startTime, duration: this.duration };
  }
}

class PerformanceMark extends PerformanceEntry {
  constructor(name, startTime) {
    super(name, "mark", startTime, 0);
  }
}

class PerformanceMeasure extends PerformanceEntry {
  constructor(name, startTime, duration) {
    super(name, "measure", startTime, duration);
  }
}

const entries = [];
const observers = [];

function record(entry) {
  entries.push(entry);
  for (const observer of observers) {
    if (observer.types.indexOf(entry.entryType) >= 0) {
      observer.queue.push(entry);
      defer(function () {
        observer.flush();
      });
    }
  }
}

function findLast(name, type) {
  for (let i = entries.length - 1; i >= 0; i--) {
    if (entries[i].name === name && entries[i].entryType === type) return entries[i];
  }
  return null;
}

class PerformanceObserver {
  constructor(callback) {
    if (typeof callback !== "function") throw new TypeError("PerformanceObserver 需要回调");
    this.callback = callback;
    this.types = [];
    this.queue = [];
    observers.push(this);
  }

  observe(options) {
    const list = options && options.entryTypes ? options.entryTypes : [];
    for (const t of list) if (this.types.indexOf(t) < 0) this.types.push(t);
    if (options && options.type && this.types.indexOf(options.type) < 0) this.types.push(options.type);
  }

  disconnect() {
    const i = observers.indexOf(this);
    if (i >= 0) observers.splice(i, 1);
    this.queue.length = 0;
  }

  takeRecords() {
    return this.queue.splice(0, this.queue.length);
  }

  flush() {
    const batch = this.takeRecords();
    if (batch.length === 0) return;
    this.callback({ getEntries: function () { return batch }, getEntriesByType: function (t) { return batch.filter(function (e) { return e.entryType === t }) } });
  }
}

export const performance = {
  now: monoNow,
  timeOrigin,
  mark: function (name, options) {
    const startTime = options && options.startTime !== undefined ? options.startTime : monoNow();
    const entry = new PerformanceMark(String(name), startTime);
    record(entry);
    return entry;
  },
  measure: function (name, startOrOptions, endMark) {
    let startTime = 0;
    let endTime = monoNow();
    if (typeof startOrOptions === "string") {
      const start = findLast(startOrOptions, "mark");
      if (start) startTime = start.startTime;
      if (typeof endMark === "string") {
        const end = findLast(endMark, "mark");
        if (end) endTime = end.startTime;
      }
    } else if (startOrOptions && typeof startOrOptions === "object") {
      if (startOrOptions.start !== undefined) startTime = startOrOptions.start;
      if (startOrOptions.end !== undefined) endTime = startOrOptions.end;
    }
    const entry = new PerformanceMeasure(String(name), startTime, Math.max(0, endTime - startTime));
    record(entry);
    return entry;
  },
  getEntries: function () {
    return entries.slice();
  },
  getEntriesByName: function (name, type) {
    return entries.filter(function (e) {
      return e.name === name && (type === undefined || e.entryType === type);
    });
  },
  getEntriesByType: function (type) {
    return entries.filter(function (e) {
      return e.entryType === type;
    });
  },
  clearMarks: function (name) {
    for (let i = entries.length - 1; i >= 0; i--) {
      if (entries[i].entryType === "mark" && (name === undefined || entries[i].name === name)) entries.splice(i, 1);
    }
  },
  clearMeasures: function (name) {
    for (let i = entries.length - 1; i >= 0; i--) {
      if (entries[i].entryType === "measure" && (name === undefined || entries[i].name === name)) entries.splice(i, 1);
    }
  },
  toJSON: function () {
    return { timeOrigin, entries: entries.slice() };
  },
};

export { PerformanceEntry, PerformanceMark, PerformanceMeasure, PerformanceObserver };
export const constants = {};
export default { performance, PerformanceObserver, PerformanceEntry, PerformanceMark, PerformanceMeasure };
