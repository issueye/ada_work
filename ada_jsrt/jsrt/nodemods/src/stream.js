// node:stream —— 最小但真实的流实现(Readable / Writable / Duplex / Transform / PassThrough)。
//
// 为什么是「真实」而不是桩:插件里最常见的用法是「把若干块数据串起来 + pipe + for await」,
// 这些语义可以在同步运行时上如实实现:push 立即派发(data)或缓存(readable),
// write 立即调用 _write,pipe 按顺序串接。
//
// 已实现：_read/_write/_flush/_destroy、按字节或对象的高水位、drain 背压、pipe、pipeline/finished、
// Readable.from 的惰性拉取、asyncIterator、Web Streams toWeb/fromWeb。
// 仍未实现：完整 WHATWG 语义、objectMode 之外的编码协商细节。
import { EventEmitter } from "node:events";
import { ReadableStream as WebReadableStream, WritableStream as WebWritableStream } from "node:stream/web";

const STATE = "__adaStreamState";

function st(stream) {
  if (stream[STATE] === undefined) {
    stream[STATE] = {
      buffer: [],
      reading: false,
      flushing: false,
      ended: false,
      endEmitted: false,
      flowing: false,
      destroyed: false,
      error: null,
      highWaterMark: 16384,
      writableEnded: false,
      writableFinished: false,
      busy: false,
      pending: [],
      bufferedBytes: 0,
      readableBytes: 0,
      objectMode: false,
      needDrain: false,
      endCb: null,
    };
  }
  return stream[STATE];
}

function streamError(code, message) {
  const err = new Error(message);
  err.code = code;
  return err;
}

// —— Readable ——

export class Readable extends EventEmitter {
  constructor(options) {
    super();
    const s = st(this);
    if (options && options.highWaterMark !== undefined) s.highWaterMark = options.highWaterMark;
    if (options && options.objectMode) s.objectMode = true;
    if (options && typeof options.read === "function") this._read = options.read;
    if (options && typeof options.destroy === "function") this._destroy = options.destroy;
    this.readable = true;
    this.readableFlowing = null;
  }

  // on/addListener 在挂上 data 监听时进入流动模式:这是 Node 的行为,
  // 也是 pipe() 能收到数据的唯一入口(运行时没有 _read 回调)。
  on(event, listener) {
    super.on(event, listener);
    if (event === "data") {
      st(this).flowing = true;
      this._maybeRead();
      this._flow();
    }
    return this;
  }

  // _read 是 Node 子类的扩展点：缓冲不足时请求更多数据。
  _read() {}

  _maybeRead(size) {
    const s = st(this);
    if (s.reading || s.ended || s.destroyed) return;
    s.reading = true;
    try {
      this._read(size || s.highWaterMark);
    } finally {
      s.reading = false;
    }
  }

  addListener(event, listener) {
    return this.on(event, listener);
  }

  push(chunk) {
    const s = st(this);
    if (s.destroyed) return false;
    if (chunk === null) {
      s.ended = true;
      this.readable = false;
      this.emit("readable");
      if (s.buffer.length === 0 && s.flowing) this._flow();
      return false;
    }
    s.buffer.push(chunk);
    s.readableBytes += s.objectMode ? 1 : chunkSize(chunk);
    this.emit("readable");
    if (s.flowing) this._flow();
    return s.readableBytes < s.highWaterMark;
  }

  unshift(chunk) {
    const s = st(this);
    if (chunk !== null && chunk !== undefined) {
      s.buffer.unshift(chunk);
      s.readableBytes += s.objectMode ? 1 : chunkSize(chunk);
    }
    return true;
  }

  read(size) {
    const s = st(this);
    if (s.buffer.length === 0) {
      if (s.error) throw s.error;
      this._maybeRead(size);
      if (s.buffer.length === 0) return null;
    }
    const chunk = s.buffer.shift();
    s.readableBytes = Math.max(0, s.readableBytes - (s.objectMode ? 1 : chunkSize(chunk)));
    this.emit("readable");
    return chunk;
  }

  resume() {
    const s = st(this);
    s.flowing = true;
    this.readableFlowing = true;
    this._maybeRead();
    this._flow();
    return this;
  }

  pause() {
    const s = st(this);
    s.flowing = false;
    this.readableFlowing = false;
    return this;
  }

  isPaused() {
    return st(this).flowing === false;
  }

  // _flow 只在流动模式派发 data;end 只在缓冲清空后发一次。
  _flow() {
    const s = st(this);
    if (!s.flowing || s.destroyed || s.flushing) return;
    s.flushing = true;
    try {
      for (;;) {
        while (s.buffer.length > 0) {
          const chunk = s.buffer.shift();
          s.readableBytes = Math.max(0, s.readableBytes - (s.objectMode ? 1 : chunkSize(chunk)));
          this.emit("data", chunk);
        }
        if (s.ended && !s.endEmitted) {
          s.endEmitted = true;
          this.emit("end");
          return;
        }
        if (!s.flowing || s.destroyed) return;
        const before = s.buffer.length;
        this._maybeRead();
        // _read 同步 push 过数据：回到循环派发；否则等待下一次 push/resume。
        if (s.buffer.length === before) {
          if (s.ended && !s.endEmitted) {
            s.endEmitted = true;
            this.emit("end");
          }
          return;
        }
      }
    } finally {
      s.flushing = false;
    }
  }

  pipe(dest, options) {
    const self = this;
    let paused = false;
    const onDrain = function () {
      if (!paused) return;
      paused = false;
      self.resume();
    };
    const onData = function (chunk) {
      let ok = true;
      try {
        ok = dest.write(chunk);
      } catch (err) {
        self.destroy(err);
        return;
      }
      if (ok === false && !paused) {
        paused = true;
        self.pause();
        dest.once("drain", onDrain);
      }
    };
    const onEnd = function () {
      self.removeListener("data", onData);
      dest.removeListener("drain", onDrain);
      if (!options || options.end !== false) {
        if (typeof dest.end === "function") dest.end();
      }
    };
    const onError = function (err) {
      dest.removeListener("drain", onDrain);
      if (typeof dest.destroy === "function") dest.destroy(err);
      else if (typeof dest.emit === "function") dest.emit("error", err);
    };
    this.on("data", onData);
    this.once("end", onEnd);
    this.once("error", onError);
    return dest;
  }

  destroy(err) {
    const s = st(this);
    if (s.destroyed) return this;
    s.destroyed = true;
    this.readable = false;
    let called = false;
    const done = (error) => {
      if (called) return;
      called = true;
      if (error) {
        s.error = error;
        this.emit("error", error);
      }
      this.emit("close");
    };
    try {
      this._destroy(err || null, done);
    } catch (hookErr) {
      done(hookErr);
    }
    return this;
  }

  // _destroy 是 Node 的清理扩展点；默认把错误原样交给 destroy 的收尾逻辑。
  _destroy(err, cb) {
    cb(err);
  }

  // 异步迭代:与 Node 的语义一致——有缓存先取缓存,否则等 readable,结束返回 done。
  [Symbol.asyncIterator]() {
    const self = this;
    const s = st(this);
    s.flowing = false;
    let finished = false;
    const next = function () {
      if (finished) return Promise.resolve({ value: undefined, done: true });
      if (s.buffer.length > 0) {
        const chunk = s.buffer.shift();
        s.readableBytes = Math.max(0, s.readableBytes - (s.objectMode ? 1 : chunkSize(chunk)));
        return Promise.resolve({ value: chunk, done: false });
      }
      if (!s.ended && !s.error) {
        self._maybeRead();
        if (s.buffer.length > 0) {
          const chunk = s.buffer.shift();
          s.readableBytes = Math.max(0, s.readableBytes - (s.objectMode ? 1 : chunkSize(chunk)));
          return Promise.resolve({ value: chunk, done: false });
        }
      }
      if (s.ended) {
        finished = true;
        return Promise.resolve({ value: undefined, done: true });
      }
      if (s.error) return Promise.reject(s.error);
      return new Promise(function (resolve, reject) {
        const onReadable = function () {
          cleanup();
          next().then(resolve, reject);
        };
        const onError = function (err) {
          cleanup();
          reject(err);
        };
        const cleanup = function () {
          self.removeListener("readable", onReadable);
          self.removeListener("error", onError);
        };
        self.on("readable", onReadable);
        self.on("error", onError);
      });
    };
    return {
      next: next,
      return: function () {
        finished = true;
        return Promise.resolve({ value: undefined, done: true });
      },
      throw: function (err) {
        finished = true;
        return Promise.reject(err);
      },
      [Symbol.asyncIterator]: function () {
        return this;
      },
    };
  }

  // Web Streams 互操作：最小 reader/writer 形状，见 node:stream/web。
  static toWeb(readable) {
    return new WebReadableStream(readable);
  }

  static fromWeb(web, options) {
    if (!web || typeof web.getReader !== "function") {
      throw streamError("ERR_INVALID_ARG_TYPE", "Readable.fromWeb 需要 Web ReadableStream");
    }
    const reader = web.getReader();
    const iterable = {
      [Symbol.asyncIterator]() {
        return {
          next: async function () {
            const result = await reader.read();
            return { value: result.value, done: result.done === true };
          },
          return: async function () {
            if (typeof reader.cancel === "function") await reader.cancel();
            return { value: undefined, done: true };
          },
        };
      },
    };
    return this.from(iterable, options);
  }

  // from 接受同步/异步可迭代对象;用 Promise 链推进,不依赖异步生成器。
  static from(iterable, options) {
    const opts = Object.assign({}, options || {});
    if (opts.objectMode === undefined) opts.objectMode = true;
    const readable = new this(opts);
    const iterator =
      typeof iterable[Symbol.asyncIterator] === "function"
        ? iterable[Symbol.asyncIterator]()
        : iterable[Symbol.iterator]();
    let reading = false;
    let finished = false;
    readable._destroy = function (err, cb) {
      finished = true;
      if (typeof iterator.return === "function") {
        Promise.resolve(iterator.return()).then(function () { cb(err); }, cb);
      } else {
        cb(err);
      }
    };
    const step = function () {
      if (reading || finished) return;
      reading = true;
      Promise.resolve(iterator.next()).then(
        function (res) {
          reading = false;
          if (finished) return;
          if (res.done) {
            finished = true;
            readable.push(null);
            return;
          }
          if (readable.push(res.value)) {
            step();
          }
          // push 返回 false：等待消费者 read/resume 触发 _read 再继续。
        },
        function (err) {
          reading = false;
          finished = true;
          readable.destroy(err);
        },
      );
    };
    readable._read = step;
    step();
    return readable;
  }
}

// —— Writable ——

// chunkSize 是背压的计量单位:字符串按字符长度(近似字节),二进制按 byteLength。
function chunkSize(chunk) {
  if (typeof chunk === "string") return chunk.length;
  if (chunk !== null && chunk !== undefined && typeof chunk.byteLength === "number") return chunk.byteLength;
  return 0;
}

// runWrite 执行一次真实写入;完成后再取队首,队列空且曾越过高水位才发 drain。
//
// 为什么要有队列:上一次 _write 还没回调时又来的写必须排在后面,否则
// (1) 写入顺序无法保证;(2) write() 的返回值永远骗人——它永远说「没积压」。
function runWrite(stream, entry) {
  const s = st(stream);
  s.busy = true;
  let called = false;
  const done = function (err) {
    if (called) return;
    called = true;
    s.busy = false;
    if (err) {
      s.pending.length = 0;
      s.bufferedBytes = 0;
      if (typeof entry.cb === "function") entry.cb(err);
      stream.emit("error", err);
      return;
    }
    if (typeof entry.cb === "function") entry.cb();
    if (s.pending.length > 0) {
      const next = s.pending.shift();
      s.bufferedBytes -= next.size;
      runWrite(stream, next);
      return;
    }
    if (s.needDrain) {
      s.needDrain = false;
      stream.emit("drain");
    }
    finishIfDrained(stream);
  };
  if (typeof stream._write === "function") stream._write(entry.chunk, entry.enc, done);
  else done();
}

// finishIfDrained 在「已 end 且队列排空」时发 finish。
function finishIfDrained(stream) {
  const s = st(stream);
  if (!s.writableEnded || s.writableFinished) return;
  if (s.busy || s.pending.length > 0) return;
  s.writableFinished = true;
  stream.writable = false;
  const cb = s.endCb;
  s.endCb = null;
  if (typeof cb === "function") cb();
  stream.emit("finish");
}

function writeImpl(stream, chunk, enc, cb) {
  if (typeof enc === "function") {
    cb = enc;
    enc = undefined;
  }
  const s = st(stream);
  if (s.writableEnded) {
    const err = streamError("ERR_STREAM_WRITE_AFTER_END", "write after end");
    if (typeof cb === "function") cb(err);
    stream.emit("error", err);
    return false;
  }
  const entry = {
    chunk: chunk,
    enc: enc === undefined ? "utf8" : enc,
    cb: typeof cb === "function" ? cb : null,
    size: chunkSize(chunk),
  };
  if (s.busy || s.pending.length > 0) {
    s.pending.push(entry);
    s.bufferedBytes += entry.size;
  } else {
    runWrite(stream, entry);
  }
  if (s.bufferedBytes >= s.highWaterMark) s.needDrain = true;
  return s.bufferedBytes < s.highWaterMark;
}

function endImpl(stream, chunk, enc, cb) {
  if (typeof chunk === "function") {
    cb = chunk;
    chunk = undefined;
  }
  if (typeof enc === "function") {
    cb = enc;
    enc = undefined;
  }
  const s = st(stream);
  if (chunk !== undefined && chunk !== null) writeImpl(stream, chunk, enc);
  if (s.writableEnded) {
    if (typeof cb === "function") cb();
    return stream;
  }
  s.writableEnded = true;
  s.endCb = typeof cb === "function" ? cb : null;
  finishIfDrained(stream);
  return stream;
}

export class Writable extends EventEmitter {
  constructor(options) {
    super();
    st(this);
    if (options && options.highWaterMark !== undefined) st(this).highWaterMark = options.highWaterMark;
    if (options && typeof options.write === "function") this._write = options.write;
    if (options && typeof options.destroy === "function") this._destroy = options.destroy;
    this.writable = true;
  }

  write(chunk, enc, cb) {
    return writeImpl(this, chunk, enc, cb);
  }

  end(chunk, enc, cb) {
    return endImpl(this, chunk, enc, cb);
  }

  destroy(err) {
    const s = st(this);
    if (s.destroyed) return this;
    s.destroyed = true;
    this.writable = false;
    s.pending.length = 0;
    s.bufferedBytes = 0;
    let called = false;
    const done = (error) => {
      if (called) return;
      called = true;
      if (error) this.emit("error", error);
      this.emit("close");
    };
    try {
      this._destroy(err || null, done);
    } catch (hookErr) {
      done(hookErr);
    }
    return this;
  }

  _destroy(err, cb) {
    cb(err);
  }

  static toWeb(writable) {
    return new WebWritableStream(writable);
  }

  static fromWeb(web, options) {
    if (!web || typeof web.getWriter !== "function") {
      throw streamError("ERR_INVALID_ARG_TYPE", "Writable.fromWeb 需要 Web WritableStream");
    }
    const writer = web.getWriter();
    const node = new this(options);
    node._write = function (chunk, enc, cb) {
      Promise.resolve(writer.write(chunk)).then(function () { cb(); }, cb);
    };
    node.on("finish", function () {
      Promise.resolve(writer.close()).catch(function () {});
    });
    return node;
  }
}

// —— Duplex ——

export class Duplex extends Readable {
  constructor(options) {
    super(options);
    this.writable = true;
  }

  write(chunk, enc, cb) {
    return writeImpl(this, chunk, enc, cb);
  }

  end(chunk, enc, cb) {
    return endImpl(this, chunk, enc, cb);
  }

  destroy(err) {
    if (st(this).destroyed) return this;
    this.writable = false;
    return Readable.prototype.destroy.call(this, err);
  }
}

// —— Transform / PassThrough ——

export class Transform extends Duplex {
  constructor(options) {
    super(options);
    // 可写侧结束后先执行 _flush，再把可读侧结束；否则下游收不到尾部数据。
    this.on("finish", () => {
      let done = false;
      const end = (err, out) => {
        if (done) return;
        done = true;
        if (err) {
          this.destroy(err);
          return;
        }
        if (out !== undefined && out !== null) this.push(out);
        this.push(null);
      };
      try {
        this._flush(end);
      } catch (err) {
        end(err);
      }
    });
  }

  // 默认与 PassThrough 一致:原样输出。子类覆盖 _transform。
  _transform(chunk, enc, cb) {
    cb(null, chunk);
  }

  // _flush 在可写侧 finish 时执行；子类可覆盖以输出尾部数据。
  _flush(cb) {
    cb();
  }

  // 写入走父类的排队路径,只是把 chunk 先交给 _transform,再把产物 push 到可读侧。
  _write(chunk, enc, cb) {
    const self = this;
    this._transform(chunk, enc, function (err, out) {
      if (err) {
        cb(err);
        return;
      }
      if (out !== undefined && out !== null) self.push(out);
      cb();
    });
  }
}

export class PassThrough extends Transform {}

// —— 工具函数 ——

export function finished(stream, callback) {
  if (typeof callback !== "function") return stream;
  let done = false;
  const finish = function (err) {
    if (done) return;
    done = true;
    callback(err || null);
  };
  stream.on("finish", function () {
    finish(null);
  });
  stream.on("end", function () {
    finish(null);
  });
  stream.on("error", function (err) {
    finish(err);
  });
  stream.on("close", function () {
    finish(null);
  });
  return stream;
}

export function pipeline() {
  const args = Array.prototype.slice.call(arguments);
  const callback = typeof args[args.length - 1] === "function" ? args.pop() : null;
  if (args.length < 2) {
    const err = streamError("ERR_INVALID_ARG_TYPE", "pipeline 至少需要两个流");
    if (callback) callback(err);
    return null;
  }
  const streams = args;
  let called = false;
  const done = function (err) {
    if (called) return;
    called = true;
    if (err) {
      for (const stream of streams) {
        if (typeof stream.destroy === "function") {
          try { stream.destroy(err); } catch (_) {}
        }
      }
    }
    if (callback) callback(err || null);
  };
  for (const stream of streams) {
    if (typeof stream.on === "function") stream.on("error", done);
  }
  for (let i = 0; i < streams.length - 1; i++) streams[i].pipe(streams[i + 1]);
  const last = streams[streams.length - 1];
  last.on("finish", function () { done(null); });
  last.on("end", function () { done(null); });
  return last;
}

export default {
  Readable,
  Writable,
  Duplex,
  Transform,
  PassThrough,
  finished,
  pipeline,
};
