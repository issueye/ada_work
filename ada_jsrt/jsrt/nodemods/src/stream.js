// node:stream —— 最小但真实的流实现(Readable / Writable / Duplex / Transform / PassThrough)。
//
// 为什么是「真实」而不是桩:插件里最常见的用法是「把若干块数据串起来 + pipe + for await」,
// 这些语义可以在同步运行时上如实实现:push 立即派发(data)或缓存(readable),
// write 立即调用 _write,pipe 按顺序串接。
//
// 与 Node 的差别(有意,写在这里):
//   - 没有跨调用的暂停/恢复:背压只体现为 write()/push() 的布尔返回值,不会有 'drain' 延时;
//   - 字符串不做编码转换(chunk 可以是 string 或 Uint8Array,原样传递);
//   - 不提供 createReadStream/createWriteStream(需要跨调用的文件句柄,属 node:fs 的后续);
//   - 不提供 objectMode 之外的特殊模式、_read 的回压协商与 _destroy 语义细节。
import { EventEmitter } from "node:events";

const STATE = "__adaStreamState";

function st(stream) {
  if (stream[STATE] === undefined) {
    stream[STATE] = {
      buffer: [],
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
    this.readable = true;
    this.readableFlowing = null;
  }

  // on/addListener 在挂上 data 监听时进入流动模式:这是 Node 的行为,
  // 也是 pipe() 能收到数据的唯一入口(运行时没有 _read 回调)。
  on(event, listener) {
    super.on(event, listener);
    if (event === "data") {
      st(this).flowing = true;
      this._flush();
    }
    return this;
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
      if (s.buffer.length === 0 && s.flowing) this._flush();
      return false;
    }
    s.buffer.push(chunk);
    this.emit("readable");
    if (s.flowing) this._flush();
    return s.buffer.length < s.highWaterMark;
  }

  unshift(chunk) {
    const s = st(this);
    if (chunk !== null && chunk !== undefined) s.buffer.unshift(chunk);
    return true;
  }

  read(size) {
    const s = st(this);
    if (s.buffer.length === 0) {
      if (s.error) throw s.error;
      return null;
    }
    const chunk = s.buffer.shift();
    this.emit("readable");
    return chunk;
  }

  resume() {
    const s = st(this);
    s.flowing = true;
    this.readableFlowing = true;
    this._flush();
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

  // _flush 只在流动模式派发 data;end 只在缓冲清空后发一次。
  _flush() {
    const s = st(this);
    if (!s.flowing || s.destroyed) return;
    while (s.buffer.length > 0) this.emit("data", s.buffer.shift());
    if (s.ended && !s.endEmitted) {
      s.endEmitted = true;
      this.emit("end");
    }
  }

  pipe(dest, options) {
    this.on("data", function (chunk) {
      dest.write(chunk);
    });
    const endDest = !options || options.end !== false;
    this.on("end", function () {
      if (endDest && typeof dest.end === "function") dest.end();
    });
    this.on("error", function (err) {
      if (typeof dest.destroy === "function") dest.destroy(err);
      else if (typeof dest.emit === "function") dest.emit("error", err);
    });
    return dest;
  }

  destroy(err) {
    const s = st(this);
    if (s.destroyed) return this;
    s.destroyed = true;
    this.readable = false;
    if (err) {
      s.error = err;
      this.emit("error", err);
    }
    this.emit("close");
    return this;
  }

  // 异步迭代:与 Node 的语义一致——有缓存先取缓存,否则等 readable,结束返回 done。
  [Symbol.asyncIterator]() {
    const self = this;
    const s = st(this);
    s.flowing = false;
    let finished = false;
    const next = function () {
      if (finished) return Promise.resolve({ value: undefined, done: true });
      if (s.buffer.length > 0) return Promise.resolve({ value: s.buffer.shift(), done: false });
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

  // from 接受同步/异步可迭代对象;用 Promise 链推进,不依赖异步生成器。
  static from(iterable, options) {
    const readable = new this(options);
    const iterator =
      typeof iterable[Symbol.asyncIterator] === "function"
        ? iterable[Symbol.asyncIterator]()
        : iterable[Symbol.iterator]();
    const step = function () {
      Promise.resolve(iterator.next()).then(
        function (res) {
          if (res.done) {
            readable.push(null);
            return;
          }
          readable.push(res.value);
          step();
        },
        function (err) {
          readable.destroy(err);
        },
      );
    };
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
    if (err) this.emit("error", err);
    this.emit("close");
    return this;
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
    Readable.prototype.destroy.call(this, err);
    return this;
  }
}

// —— Transform / PassThrough ——

export class Transform extends Duplex {
  constructor(options) {
    super(options);
    // 写完就结束可读侧:产物已在 _write 里 push 出去了,不补 push(null)
    // 的话下游永远等不到 'end'(Node 由 _flush 负责这件事)。
    this.on("finish", () => this.push(null));
  }

  // 默认与 PassThrough 一致:原样输出。子类覆盖 _transform。
  _transform(chunk, enc, cb) {
    cb(null, chunk);
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
  return stream;
}

export function pipeline() {
  const args = Array.prototype.slice.call(arguments);
  const callback = typeof args[args.length - 1] === "function" ? args.pop() : null;
  let called = false;
  const done = function (err) {
    if (called) return;
    called = true;
    if (callback) callback(err || null);
  };
  if (args.length < 2) return done(streamError("ERR_INVALID_ARG_TYPE", "pipeline 至少需要两个流"));
  for (let i = 0; i < args.length - 1; i++) args[i].pipe(args[i + 1]);
  const last = args[args.length - 1];
  last.on("finish", function () {
    done(null);
  });
  last.on("end", function () {
    done(null);
  });
  last.on("error", function (err) {
    done(err);
  });
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
