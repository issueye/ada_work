// node:zlib —— 宿主自建的同步压缩子集(gzip / deflate / raw deflate)。
//
// 与 node:crypto 一样,这里只补插件真正需要、且宿主能纯计算完成的部分:
// 同步的 gzipSync/gunzipSync/deflateSync/inflateSync/deflateRawSync/
// inflateRawSync/unzipSync。流式接口(createGzip/管道)、brotli、zip 归档
// 都不实现——用近似实现冒充 Node 的字节语义,调用方会以为拿到的是完整支持。
//
// 流式接口（createGzip 等）优先走宿主流式句柄：写入即产生可用输出，
// 解压通过 pipe 增量读取；受 maxOutputLength/8MiB 输出上限保护。
// 宿主能力来自 __adaHost.zlib(op, base64),数据跨边界一律是 base64;
// 返回值优先用全局 Buffer(moejs node 包安装),没有 Buffer 时明确报错。
import { Transform } from "node:stream";

const host = globalThis.__adaHost

function requireHost() {
  if (!host || typeof host.zlib !== "function") {
    throw new Error("node:zlib 不可用:宿主没有安装 zlib")
  }
  return host.zlib
}

function bufferOf(value) {
  const B = globalThis.Buffer
  if (!B || typeof B.from !== "function" || typeof B.from(value).toString !== "function") {
    throw new Error("node:zlib 需要 Buffer 支持")
  }
  if (typeof value === "string") return B.from(value, "utf8")
  if (Array.isArray(value)) return B.from(value)
  if (value instanceof Uint8Array || (value && typeof value.length === "number" && value.buffer)) {
    return B.from(value)
  }
  if (value && typeof value === "object" && typeof value.toString === "function") {
    // moejs 的 Buffer 与浏览器 Buffer 都认这里;字符串之外的对象形态不做猜测。
    const text = value.toString()
    if (typeof text === "string" && text !== "[object Object]") return B.from(text, "utf8")
  }
  throw new TypeError("node:zlib 的输入必须是字符串或 Buffer")
}

function apply(op, input) {
  const buf = bufferOf(input)
  const host64 = requireHost()(op, buf.toString("base64"))
  const B = globalThis.Buffer
  return B.from(host64, "base64")
}

export function gzipSync(input) {
  return apply("gzip", input)
}

export function gunzipSync(input) {
  return apply("gunzip", input)
}

export function deflateSync(input) {
  return apply("deflate", input)
}

export function inflateSync(input) {
  return apply("inflate", input)
}

export function deflateRawSync(input) {
  return apply("deflateRaw", input)
}

export function inflateRawSync(input) {
  return apply("inflateRaw", input)
}

export function unzipSync(input) {
  return apply("unzip", input)
}

// ZlibStream 优先使用宿主流式句柄：每次 write 产生当前可用的压缩/解压字节。
// 宿主没有流式句柄时回退到缓冲实现（兼容旧宿主）。
class ZlibStream extends Transform {
  constructor(op, options) {
    super(options);
    this._op = op;
    this._chunks = [];
    this._handle = 0;
    this._outputBytes = 0;
    this._maxOutputLength = options && Number(options.maxOutputLength) > 0 ? Number(options.maxOutputLength) : 0;
    if (host && typeof host.zlibStreamNew === "function") {
      try {
        this._handle = Number(host.zlibStreamNew(op)) || 0;
      } catch (_) {
        this._handle = 0;
      }
    }
  }

  _pushOutput(encoded) {
    if (!encoded) return true;
    const out = globalThis.Buffer.from(encoded, "base64");
    if (out.length === 0) return true;
    this._outputBytes += out.length;
    if (this._maxOutputLength > 0 && this._outputBytes > this._maxOutputLength) {
      const err = new Error("zlib 输出超过 maxOutputLength");
      err.code = "ERR_BUFFER_TOO_LARGE";
      this.destroy(err);
      return false;
    }
    this.push(out);
    return true;
  }

  _transform(chunk, enc, cb) {
    try {
      const input = bufferOf(chunk);
      if (this._handle) {
        this._pushOutput(host.zlibStreamWrite(this._handle, input.toString("base64")));
      } else {
        this._chunks.push(input);
      }
      cb();
    } catch (err) {
      cb(err);
    }
  }

  _flush(cb) {
    if (this._handle) {
      const id = this._handle;
      this._handle = 0;
      try {
        this._pushOutput(host.zlibStreamEnd(id));
        cb();
      } catch (err) {
        cb(err);
      }
      return;
    }
    try {
      const input = globalThis.Buffer.concat(this._chunks);
      this._chunks = [];
      const out = apply(this._op, input);
      if (this._maxOutputLength > 0 && out.length > this._maxOutputLength) {
        const err = new Error("zlib 输出超过 maxOutputLength");
        err.code = "ERR_BUFFER_TOO_LARGE";
        cb(err);
        return;
      }
      this.push(out);
      cb();
    } catch (err) {
      cb(err);
    }
  }

  _destroy(err, cb) {
    if (this._handle) {
      try { host.zlibStreamClose(this._handle); } catch (_) {}
      this._handle = 0;
    }
    cb(err);
  }
}

export function createGzip(options) {
  return new ZlibStream("gzip", options);
}

export function createGunzip(options) {
  return new ZlibStream("gunzip", options);
}

export function createDeflate(options) {
  return new ZlibStream("deflate", options);
}

export function createInflate(options) {
  return new ZlibStream("inflate", options);
}

export function createDeflateRaw(options) {
  return new ZlibStream("deflateRaw", options);
}

export function createInflateRaw(options) {
  return new ZlibStream("inflateRaw", options);
}

export function createUnzip(options) {
  return new ZlibStream("unzip", options);
}

// constants 只给最常见的几个档位:宿主用的是 Go 的默认压缩级别,
// 不暴露逐档调节,避免调用方以为设置了却没有任何效果。
export const constants = {
  Z_NO_COMPRESSION: 0,
  Z_BEST_SPEED: 1,
  Z_BEST_COMPRESSION: 9,
  Z_DEFAULT_COMPRESSION: -1,
  Z_DEFAULT_STRATEGY: 0,
}

export default {
  gzipSync,
  gunzipSync,
  deflateSync,
  inflateSync,
  deflateRawSync,
  inflateRawSync,
  unzipSync,
  createGzip,
  createGunzip,
  createDeflate,
  createInflate,
  createDeflateRaw,
  createInflateRaw,
  createUnzip,
  constants,
}
