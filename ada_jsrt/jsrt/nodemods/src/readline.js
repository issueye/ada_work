// node:readline —— 按行读取一个可读流。
//
// 我们的运行时没有终端,所以这里做的是 Node readline 的「非 terminal 模式」:
// 从 input(任意 node:stream Readable)读数据、按 \r?\n 切行、发 line 事件,
// 并在 input 结束时补发最后一段(没有换行结尾的行),然后 close。
//
// 没有终端意味着这些能力不存在(不是桩,是如实拒绝):
//   - prompt()/setPrompt() 不渲染任何东西(只记下来,便于测试断言);
//   - terminal 模式的光标/历史/按键解析不实现;
//   - question() 只是「写 query + 等下一行」,不做行编辑。
import { EventEmitter } from "node:events";

function toText(chunk) {
  if (typeof chunk === "string") return chunk;
  if (chunk === null || chunk === undefined) return "";
  if (typeof Buffer !== "undefined" && typeof Buffer.from === "function") {
    try {
      return Buffer.from(chunk).toString("utf8");
    } catch (_) {}
  }
  return String(chunk);
}

function invalidArg(message) {
  const err = new TypeError(message);
  err.code = "ERR_INVALID_ARG_TYPE";
  return err;
}

export class Interface extends EventEmitter {
  constructor(options) {
    super();
    const opts = options || {};
    const input = opts.input;
    if (!input || typeof input.on !== "function") {
      throw invalidArg("readline.createInterface 需要 options.input(一个可读流)");
    }
    this.input = input;
    this.output = opts.output || null;
    this.terminal = Boolean(opts.terminal);
    this.crlfDelay = opts.crlfDelay === undefined ? 100 : opts.crlfDelay;
    this.closed = false;
    this.line = "";
    this._pending = [];
    this._waiters = [];
    this._prompt = "";
    this._onData = (chunk) => this._feed(toText(chunk));
    this._onEnd = () => this._finish();
    input.on("data", this._onData);
    input.on("end", this._onEnd);
    input.on("error", (err) => this._emitLineError(err));
  }

  _feed(text) {
    this.line += text;
    for (;;) {
      const idx = this.line.indexOf("\n");
      if (idx < 0) break;
      let line = this.line.slice(0, idx);
      this.line = this.line.slice(idx + 1);
      if (line.endsWith("\r")) line = line.slice(0, -1);
      this._emitLine(line);
    }
  }

  _finish() {
    if (this.line.length > 0) {
      let line = this.line;
      if (line.endsWith("\r")) line = line.slice(0, -1);
      this.line = "";
      this._emitLine(line);
    }
    this.close();
  }

  // _emitLine 的分发顺序:显式 line 监听 > 等待中的 question/迭代器 > 暂存。
  // 必须暂存:数据可能在调用方挂上迭代器之前就已经到齐(Readable.from 是微任务推进的),
  // 丢掉的行会让 `for await` 永远等不到。
  _emitLine(line) {
    const listeners = this.listeners("line");
    if (listeners.length > 0) {
      for (const fn of listeners) fn.call(this, line);
      return;
    }
    const waiters = this._waiters.splice(0);
    if (waiters.length > 0) {
      for (const waiter of waiters) waiter(line);
      return;
    }
    this._pending.push(line);
  }

  _emitLineError(err) {
    for (const fn of this.listeners("error")) fn.call(this, err);
  }

  _write(text) {
    if (this.output && typeof this.output.write === "function") this.output.write(text);
  }

  setPrompt(prompt) {
    this._prompt = String(prompt === undefined ? "" : prompt);
  }

  getPrompt() {
    return this._prompt;
  }

  prompt() {
    // 没有终端可渲染:保留接口形状,不做输出。
  }

  // question 写 query,然后等下一行;回调形态与 Node 一致。
  question(query, callback) {
    if (typeof query === "string") this._write(query);
    if (typeof callback !== "function") return;
    if (this.closed) {
      callback("");
      return;
    }
    this._waiters.push(function (line) {
      callback(line);
    });
  }

  write(data) {
    if (typeof data === "string") this._feed(data);
    return true;
  }

  pause() {
    if (typeof this.input.pause === "function") this.input.pause();
    return this;
  }

  resume() {
    if (typeof this.input.resume === "function") this.input.resume();
    return this;
  }

  close() {
    if (this.closed) return this;
    this.closed = true;
    if (typeof this.input.removeListener === "function") {
      this.input.removeListener("data", this._onData);
      this.input.removeListener("end", this._onEnd);
    }
    for (const waiter of this._waiters.splice(0)) waiter("");
    this.emit("close");
    return this;
  }

  [Symbol.asyncIterator]() {
    const self = this;
    return {
      next: function () {
        if (self._pending.length > 0) return Promise.resolve({ value: self._pending.shift(), done: false });
        if (self.closed) return Promise.resolve({ value: undefined, done: true });
        return new Promise(function (resolve) {
          self._waiters.push(function (line) {
            resolve({ value: line, done: false });
          });
        });
      },
      return: function () {
        self.close();
        return Promise.resolve({ value: undefined, done: true });
      },
      [Symbol.asyncIterator]: function () {
        return this;
      },
    };
  }
}

export function createInterface(options) {
  return new Interface(options);
}

export default { Interface, createInterface };
