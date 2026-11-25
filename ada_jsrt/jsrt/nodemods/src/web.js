// node:stream/web —— 最小 Web Streams 互操作层。
//
// 只实现 ReadableStream / WritableStream 的 reader/writer 形状，作为
// Readable.toWeb / Readable.fromWeb / Writable.toWeb / Writable.fromWeb 的载体。
// 不支持 BYOB、背压控制器、TransformStream 的完整 WHATWG 语义。

function readerFor(source) {
  if (source && typeof source.getReader === "function") {
    const inner = source.getReader();
    return {
      read: function () { return inner.read(); },
      cancel: function (reason) { return typeof inner.cancel === "function" ? inner.cancel(reason) : Promise.resolve(); },
      releaseLock: function () { if (typeof inner.releaseLock === "function") inner.releaseLock(); },
    };
  }
  const iterator = source && typeof source[Symbol.asyncIterator] === "function" ? source[Symbol.asyncIterator]() : null;
  let done = false;
  return {
    read: async function () {
      if (done || iterator === null) return { value: undefined, done: true };
      const result = await iterator.next();
      done = result.done === true;
      return { value: result.value, done: done };
    },
    cancel: async function (reason) {
      done = true;
      if (source && typeof source.destroy === "function") source.destroy(reason);
      else if (iterator && typeof iterator.return === "function") await iterator.return();
    },
    releaseLock: function () {},
  };
}

export class ReadableStream {
  constructor(source) {
    this._source = source;
    this._reader = null;
  }

  get locked() {
    return this._reader !== null;
  }

  getReader() {
    if (this._reader !== null) throw new Error("ReadableStream is locked");
    this._reader = readerFor(this._source);
    return this._reader;
  }

  cancel(reason) {
    if (this._reader !== null) return this._reader.cancel(reason);
    if (this._source && typeof this._source.destroy === "function") {
      this._source.destroy(reason);
    }
    return Promise.resolve();
  }

  [Symbol.asyncIterator]() {
    const reader = this.getReader();
    const self = this;
    return {
      next: function () { return reader.read(); },
      return: async function () {
        await reader.cancel();
        self._reader = null;
        return { value: undefined, done: true };
      },
      [Symbol.asyncIterator]: function () { return this; },
    };
  }
}

export class WritableStream {
  constructor(sink) {
    this._sink = sink;
    this._writer = null;
  }

  get locked() {
    return this._writer !== null;
  }

  getWriter() {
    if (this._writer !== null) throw new Error("WritableStream is locked");
    const sink = this._sink;
    this._writer = {
      write: function (chunk) {
        return new Promise(function (resolve, reject) {
          try {
            sink.write(chunk, function (err) { err ? reject(err) : resolve(); });
          } catch (err) {
            reject(err);
          }
        });
      },
      close: function () {
        return new Promise(function (resolve, reject) {
          try {
            sink.end(function (err) { err ? reject(err) : resolve(); });
          } catch (err) {
            reject(err);
          }
        });
      },
      abort: async function (reason) {
        if (sink && typeof sink.destroy === "function") sink.destroy(reason);
      },
      releaseLock: function () {},
    };
    return this._writer;
  }

  close() {
    return this._writer !== null ? this._writer.close() : Promise.resolve();
  }

  abort(reason) {
    return this._writer !== null ? this._writer.abort(reason) : Promise.resolve();
  }
}

export class TransformStream {
  constructor() {
    const err = new Error("TransformStream 不提供:请使用 node:stream 的 Transform");
    err.code = "ERR_NOT_SUPPORTED";
    throw err;
  }
}

export default { ReadableStream, WritableStream, TransformStream };
