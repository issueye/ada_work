// node:http / node:https —— 同步 HTTP(S) 客户端。
//
// 为什么是同步的:本运行时没有事件循环,插件 apply 与工具调用都在 worker 协程上
// 同步执行。请求在 req.end()(http.get 则是调用时)立即完成,response / data /
// end 回调按注册顺序同步触发。因此下面两种写法都能工作:
//
//	const body = await new Promise((resolve, reject) => {
//	  http.get(url, (res) => {
//	    let text = "";
//	    res.on("data", (chunk) => { text += chunk });
//	    res.on("end", () => resolve(text));
//	  }).on("error", reject);
//	});
//
// 客户端：Scoped 整包，Resident 流式；服务端 createServer 依赖 net.Server（需要 LoopResident），
// 提供最小 HTTP/1.1 请求解析与响应写出，暂不支持 keep-alive 与 chunked。
import { Buffer } from "node:buffer";
import { URL } from "node:url";
import { createServer as createNetServer, Socket } from "node:net";
import { createServer as createTLSServer } from "node:tls";

const host = globalThis.__adaHost;

// 最小事件基类。这里不 import node:events,是为了让宿主模块保持自包含。
class Emitter {
  constructor() {
    this._events = Object.create(null);
  }
  on(name, fn) {
    if (typeof fn !== "function") throw new TypeError("监听器必须是函数");
    (this._events[name] || (this._events[name] = [])).push(fn);
    return this;
  }
  once(name, fn) {
    const self = this;
    function wrapper(...args) {
      self.off(name, wrapper);
      fn.apply(self, args);
    }
    wrapper.listener = fn;
    return this.on(name, wrapper);
  }
  off(name, fn) {
    const list = this._events[name];
    if (!list) return this;
    if (fn === undefined) {
      delete this._events[name];
      return this;
    }
    for (let i = list.length - 1; i >= 0; i--) {
      if (list[i] === fn || list[i].listener === fn) list.splice(i, 1);
    }
    return this;
  }
  removeListener(name, fn) {
    return this.off(name, fn);
  }
  removeAllListeners(name) {
    if (name === undefined) this._events = Object.create(null);
    else delete this._events[name];
    return this;
  }
  emit(name, ...args) {
    const list = this._events[name];
    if (!list || list.length === 0) return false;
    for (const fn of list.slice()) fn.apply(this, args);
    return true;
  }
  listeners(name) {
    return (this._events[name] || []).slice();
  }
  listenerCount(name) {
    return (this._events[name] || []).length;
  }
}
Emitter.prototype.addListener = Emitter.prototype.on;

function httpError(message, code) {
  const err = new Error(message);
  err.code = code || "EHTTP";
  return err;
}

function abortErrorFrom(signal) {
  const reason = signal && signal.reason;
  if (reason instanceof Error) return reason;
  const err = new Error("The operation was aborted");
  err.name = "AbortError";
  err.code = "ABORT_ERR";
  return err;
}

function expose(code, message, err) {
  if (err && err.code && err.code !== "EHTTP") return err;
  return httpError(message + (err && err.message ? ": " + err.message : ""), code);
}

function toBuffer(chunk, encoding) {
  if (chunk === null || chunk === undefined) return Buffer.alloc(0);
  if (Buffer.isBuffer(chunk)) return chunk;
  if (typeof chunk === "string") return Buffer.from(chunk, encoding || "utf8");
  if (chunk instanceof Uint8Array) return Buffer.from(chunk);
  return Buffer.from(String(chunk), encoding || "utf8");
}

function buildURL(options, secure) {
  if (typeof options === "string") return options;
  if (!options || typeof options !== "object") throw httpError("http 请求缺少 options", "ERR_INVALID_ARG_TYPE");
  if (typeof options.href === "string" && options.protocol) return options.href;
  const protocol = options.protocol || (secure ? "https:" : "http:");
  const hostname = options.hostname || options.host || "";
  if (!hostname) throw httpError("http 请求缺少 hostname", "ERR_INVALID_ARG_VALUE");
  const port = options.port ? ":" + options.port : "";
  let path = options.path || "";
  if (!path) {
    const pathname = options.pathname || "/";
    const search = options.search || (options.query ? "?" + options.query : "");
    path = pathname + search;
  }
  if (!path.startsWith("/")) path = "/" + path;
  const auth = options.auth ? options.auth + "@" : "";
  return protocol + "//" + auth + hostname + port + path;
}

// IncomingMessage:响应体在请求完成时已经全部到手,所以这里只做「按注册顺序投递」。
// 与 Node 的差别:没有真正的 flowing/paused 状态,resume() 等价于开始投递。
export class IncomingMessage extends Emitter {
  constructor(payload) {
    super();
    this.statusCode = payload.status;
    this.statusMessage = payload.statusText || "";
    this.httpVersion = String(payload.httpVersion || "HTTP/1.1").replace(/^HTTP\//, "");
    this.headers = payload.headers || {};
    this.headersDistinct = {};
    this.rawHeaders = payload.rawHeaders || [];
    this.trailers = {};
    this.complete = true;
    this.aborted = false;
    this.readable = true;
    this.readableEnded = false;
    this.destroyed = false;
    this.url = "";
    this.method = null;
    this._body = Buffer.from(payload.body || "", "base64");
    this._truncated = !!payload.truncated;
    this._encoding = null;
    this._delivered = false;
    this._ended = false;
    this._endPending = false;
  }

  get truncated() {
    return this._truncated;
  }

  setEncoding(encoding) {
    this._encoding = encoding || "utf8";
    return this;
  }

  on(name, fn) {
    super.on(name, fn);
    if (name === "data") this._deliver();
    else if (name === "end" || name === "close" || name === "readable") {
      this._endPending = true;
      this._maybeEnd();
    }
    return this;
  }

  _deliver() {
    if (this._delivered || this._ended) return;
    this._delivered = true;
    if (this._body.length > 0) {
      const chunk = this._encoding ? this._body.toString(this._encoding) : this._body;
      this.emit("data", chunk);
    }
    this._endPending = true;
    this._maybeEnd();
  }

  _maybeEnd() {
    if (this._ended || !this._endPending) return;
    // 没有 end 监听器就先挂着:让调用方把 data 与 end 都注册完再触发,
    // 否则 end 会在 data 之后立刻丢失。
    if (this.listenerCount("end") === 0 && this.listenerCount("close") === 0) return;
    this._ended = true;
    this.readable = false;
    this.readableEnded = true;
    this.emit("end");
    this.emit("close");
  }

  resume() {
    this._deliver();
    this._maybeEnd();
    return this;
  }

  pause() {
    return this;
  }

  isPaused() {
    return false;
  }

  read() {
    if (this._delivered) return null;
    this._delivered = true;
    const chunk = this._encoding ? this._body.toString(this._encoding) : this._body;
    this._endPending = true;
    this._maybeEnd();
    return chunk;
  }

  pipe(dest, options) {
    const end = !options || options.end !== false;
    this.on("data", (chunk) => dest.write(chunk));
    this.on("end", () => {
      if (end && typeof dest.end === "function") dest.end();
    });
    return dest;
  }

  destroy(err) {
    if (this.destroyed) return this;
    this.destroyed = true;
    this.readable = false;
    if (err) this.emit("error", err);
    this.emit("close");
    return this;
  }
}
IncomingMessage.prototype.addListener = IncomingMessage.prototype.on;

// StreamIncomingMessage 是 Resident 流式客户端的响应流：data 到达即派发。
class StreamIncomingMessage extends Emitter {
  constructor(statusCode, statusMessage, headers, rawHeaders, method) {
    super();
    this.statusCode = statusCode;
    this.statusMessage = statusMessage || "";
    this.httpVersion = "1.1";
    this.headers = headers;
    this.rawHeaders = rawHeaders;
    this.trailers = {};
    this.complete = false;
    this.aborted = false;
    this.readable = true;
    this.readableEnded = false;
    this.destroyed = false;
    this.url = "";
    this.method = method;
    this._chunks = [];
    this._flowing = false;
    this._ended = false;
    this._encoding = null;
  }

  setEncoding(encoding) {
    this._encoding = encoding || "utf8";
    return this;
  }

  on(name, fn) {
    super.on(name, fn);
    if (name === "data") {
      this._flowing = true;
      this._flush();
    }
    return this;
  }

  addListener(name, fn) {
    return this.on(name, fn);
  }

  _flush() {
    if (!this._flowing) return;
    while (this._chunks.length > 0) {
      const chunk = this._chunks.shift();
      this.emit("data", this._encoding ? chunk.toString(this._encoding) : chunk);
    }
    if (this._ended && !this.readableEnded) this._emitEnd();
  }

  _push(chunk) {
    if (this.destroyed || this._ended) return;
    const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk));
    if (buf.length === 0) return;
    if (this._flowing) this.emit("data", this._encoding ? buf.toString(this._encoding) : buf);
    else this._chunks.push(buf);
  }

  _end() {
    if (this._ended) return;
    this._ended = true;
    this.complete = true;
    if (this._flowing) this._flush();
    else if (this.listenerCount("end") > 0) this._emitEnd();
  }

  _emitEnd() {
    if (this.readableEnded) return;
    this.readableEnded = true;
    this.readable = false;
    this.emit("end");
    this.emit("close");
  }

  pause() {
    this._flowing = false;
    return this;
  }

  resume() {
    this._flowing = true;
    this._flush();
    return this;
  }

  isPaused() {
    return !this._flowing;
  }

  read() {
    if (this._chunks.length === 0) {
      if (this._ended && !this.readableEnded) this._emitEnd();
      return null;
    }
    const chunk = this._chunks.shift();
    return this._encoding ? chunk.toString(this._encoding) : chunk;
  }

  pipe(dest, options) {
    this.on("data", (chunk) => dest.write(chunk));
    this.on("end", () => {
      if ((!options || options.end !== false) && typeof dest.end === "function") dest.end();
    });
    return dest;
  }

  destroy(err) {
    if (this.destroyed) return this;
    this.destroyed = true;
    this.readable = false;
    if (err) this.emit("error", err);
    this.emit("close");
    return this;
  }
}

// StreamingClientRequest 是 Resident 下的 HTTP/1.1 流式客户端：
// 请求体可增量写入，响应 data 按到达顺序派发。Scoped 仍使用整包 ClientRequest。
class StreamingClientRequest extends Emitter {
  constructor(options, callback, secure) {
    super();
    this._secure = secure;
    this._options = options;
    this.method = String(options.method || "GET").toUpperCase();
    this._url = new URL(buildURL(options, secure));
    this.path = options.path || this._url.pathname + this._url.search || "/";
    if (!this.path.startsWith("/")) this.path = "/" + this.path;
    this._headers = {};
    const headers = options.headers || {};
    for (const key of Object.keys(headers)) this.setHeader(key, headers[key]);
    this._chunks = [];
    this._ended = false;
    this._connected = false;
    this._headersSent = false;
    this._chunked = false;
    this._bodyFinished = false;
    this._destroyed = false;
    this._timeout = Number(options.timeout) || 0;
    this._signal = options.signal || null;
    const limits = globalThis.__adaHTTPLimits || {};
    this._maxRequestBytes = Number(limits.maxRequestBytes) || 0;
    this._maxResponseBytes = Number(limits.maxResponseBytes) || 0;
    this._requestBytes = 0;
    this._responseBytes = 0;
    this._abortHandler = null;
    this._sock = null;
    this._buffer = Buffer.alloc(0);
    this._response = null;
    this._resEnded = false;
    this._mode = null;
    this._remaining = 0;
    this._chunkState = "size";
    this._chunkRemaining = 0;
    this.res = null;
    this.socket = null;
    this.aborted = false;
    this.reusedSocket = false;
    if (this._signal) {
      if (this._signal.aborted) this._aborted = true;
      else {
        this._abortHandler = () => {
          this.aborted = true;
          this.destroy(abortErrorFrom(this._signal));
        };
        this._signal.addEventListener("abort", this._abortHandler, { once: true });
      }
    }
    if (typeof callback === "function") this.once("response", callback);
    queueMicrotask(() => this._start());
  }

  setHeader(name, value) {
    this._headers[String(name)] = value;
    return this;
  }
  getHeader(name) {
    const lower = String(name).toLowerCase();
    for (const key of Object.keys(this._headers)) if (key.toLowerCase() === lower) return this._headers[key];
    return undefined;
  }
  getHeaders() {
    const out = {};
    for (const key of Object.keys(this._headers)) out[key.toLowerCase()] = this._headers[key];
    return out;
  }
  getHeaderNames() {
    return Object.keys(this._headers).map((k) => k.toLowerCase());
  }
  hasHeader(name) {
    return this.getHeader(name) !== undefined;
  }
  removeHeader(name) {
    const lower = String(name).toLowerCase();
    for (const key of Object.keys(this._headers)) if (key.toLowerCase() === lower) delete this._headers[key];
  }

  setTimeout(ms, callback) {
    this._timeout = Number(ms) || 0;
    if (this._sock && typeof this._sock.setTimeout === "function") this._sock.setTimeout(this._timeout);
    if (typeof callback === "function") this.once("timeout", callback);
    return this;
  }

  write(chunk, encoding, callback) {
    if (this._ended) throw httpError("write after end", "ERR_STREAM_WRITE_AFTER_END");
    const buf = toBuffer(chunk, typeof encoding === "string" ? encoding : undefined);
    this._requestBytes += buf.length;
    if (this._maxRequestBytes > 0 && this._requestBytes > this._maxRequestBytes) {
      this.destroy(httpError("请求体超过 MaxRequestBytes", "ERR_HTTP_REQUEST_TOO_LARGE"));
      return false;
    }
    this._chunks.push(buf);
    if (this._connected && this._headersSent) this._writeBody(buf);
    if (typeof encoding === "function") encoding();
    else if (typeof callback === "function") callback();
    return true;
  }

  end(chunk, encoding, callback) {
    if (chunk !== undefined && chunk !== null && typeof chunk !== "function") {
      this.write(chunk, typeof encoding === "string" ? encoding : undefined);
    }
    if (typeof chunk === "function") callback = chunk;
    else if (typeof encoding === "function") callback = encoding;
    if (this._ended) return this;
    this._ended = true;
    if (this._connected) this._finishBody();
    if (typeof callback === "function") queueMicrotask(callback);
    return this;
  }

  flushHeaders() {
    if (this._connected) this._sendHead();
    return this;
  }

  abort() {
    this.aborted = true;
    this.destroy(abortErrorFrom(this._signal));
  }

  destroy(err) {
    if (this._destroyed) return this;
    this._destroyed = true;
    if (this._signal && this._abortHandler) {
      try { this._signal.removeEventListener("abort", this._abortHandler); } catch (_) {}
      this._signal = null;
      this._abortHandler = null;
    }
    if (this._sock) {
      try { this._sock.destroy(); } catch (_) {}
    }
    if (this._response) this._response.destroy(err);
    if (this.aborted) this.emit("abort");
    if (err) this.emit("error", err);
    this.emit("close");
    return this;
  }

  _start() {
    if (this._destroyed) return;
    if (this._aborted) {
      const err = abortErrorFrom(this._signal);
      queueMicrotask(() => {
        if (!this._destroyed) this.destroy(err);
      });
      return;
    }
    if (!host || !host.net) {
      this.destroy(httpError("Cannot import \"node:http\": 宿主未安装网络能力", "ERR_ACCESS_DENIED"));
      return;
    }
    const sock = new Socket();
    const secure = this._secure;
    const url = this._url;
    sock._dial = (address, timeout) => {
      if (secure) {
        if (typeof host.net.tlsDialAsync !== "function") {
          return Promise.reject(httpError("宿主未安装 TLS 能力", "ERR_ACCESS_DENIED"));
        }
        return host.net.tlsDialAsync(address, timeout, JSON.stringify({
          serverName: url.hostname,
          insecure: this._options.rejectUnauthorized === false,
          scheme: "https",
        }));
      }
      if (typeof host.net.dialAsync !== "function") {
        return Promise.reject(httpError("宿主未安装网络能力", "ERR_ACCESS_DENIED"));
      }
      return host.net.dialAsync(address, timeout, "http");
    };
    sock.on("connect", () => this._onConnect());
    sock.on("data", (chunk) => this._onData(chunk));
    sock.on("end", () => this._onSocketEnd());
    sock.on("error", (err) => this.destroy(err));
    sock.on("close", () => {
      if (this._response && !this._resEnded) this._finishResponse();
    });
    sock.on("timeout", () => this.emit("timeout"));
    this._sock = sock;
    this.socket = sock;
    const port = Number(url.port) || (secure ? 443 : 80);
    sock.connect({
      host: url.hostname,
      port: port,
      signal: this._signal,
      timeout: this._timeout,
    });
  }

  _onConnect() {
    if (this._destroyed) return;
    this._connected = true;
    this._sendHead();
  }

  _sendHead() {
    if (this._headersSent || this._destroyed) return;
    let contentLength = this.getHeader("content-length");
    let transfer = this.getHeader("transfer-encoding");
    if (contentLength === undefined && transfer === undefined) {
      if (this._ended) {
        let total = 0;
        for (const chunk of this._chunks) total += chunk.length;
        this.setHeader("Content-Length", String(total));
        contentLength = String(total);
      } else {
        this._chunked = true;
        this.setHeader("Transfer-Encoding", "chunked");
        transfer = "chunked";
      }
    }
    if (this.getHeader("host") === undefined) this.setHeader("Host", this._url.host);
    if (this.getHeader("connection") === undefined) this.setHeader("Connection", "close");
    let head = this.method + " " + this.path + " HTTP/1.1\r\n";
    for (const key of Object.keys(this._headers)) {
      const value = this._headers[key];
      if (value === undefined || value === null) continue;
      head += key + ": " + (Array.isArray(value) ? value.join(", ") : String(value)) + "\r\n";
    }
    head += "\r\n";
    this._headersSent = true;
    this._sock.write(head);
    for (const chunk of this._chunks) this._writeBody(chunk);
    if (this._ended) this._finishBody();
  }

  _writeBody(buf) {
    if (this._destroyed || !this._sock) return;
    if (this._chunked) {
      this._sock.write(buf.length.toString(16) + "\r\n");
      this._sock.write(buf);
      this._sock.write("\r\n");
    } else {
      this._sock.write(buf);
    }
  }

  _finishBody() {
    if (this._bodyFinished) return;
    this._bodyFinished = true;
    if (this._chunked && this._sock) this._sock.write("0\r\n\r\n");
  }

  _onData(chunk) {
    if (this._destroyed || this._resEnded) return;
    this._buffer = Buffer.concat([this._buffer, Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk))]);
    this._consume();
  }

  _consume() {
    if (!this._response) {
      const end = this._buffer.indexOf("\r\n\r\n");
      if (end < 0) {
        if (this._buffer.length > maxHeaderSize) this.destroy(httpError("响应头过大", "ERR_HTTP_HEADERS_TOO_LARGE"));
        return;
      }
      const head = this._buffer.subarray(0, end).toString("latin1");
      this._buffer = this._buffer.subarray(end + 4);
      const lines = head.split("\r\n");
      const statusLine = lines.shift() || "";
      const parts = statusLine.split(" ");
      const statusCode = Number(parts[1]) || 0;
      const statusMessage = parts.slice(2).join(" ");
      const headers = {};
      const rawHeaders = [];
      for (const line of lines) {
        const i = line.indexOf(":");
        if (i <= 0) continue;
        const name = line.slice(0, i).trim();
        const value = line.slice(i + 1).trim();
        rawHeaders.push(name, value);
        const key = name.toLowerCase();
        headers[key] = headers[key] === undefined ? value : headers[key] + ", " + value;
      }
      const res = new StreamIncomingMessage(statusCode, statusMessage, headers, rawHeaders, this.method);
      res.url = this.path;
      this._response = res;
      this.res = res;
      this.emit("response", res);
      const noBody = this.method === "HEAD" || statusCode === 204 || statusCode === 304 || statusCode < 200;
      if (noBody) {
        this._finishResponse();
        return;
      }
      const transfer = String(headers["transfer-encoding"] || "").toLowerCase();
      if (transfer.includes("chunked")) {
        this._mode = "chunked";
        this._chunkState = "size";
      } else if (headers["content-length"] !== undefined) {
        this._mode = "length";
        this._remaining = Math.max(0, Number(headers["content-length"]) || 0);
        if (this._remaining === 0) {
          this._finishResponse();
          return;
        }
      } else {
        this._mode = "close";
      }
    }
    this._consumeBody();
  }

  _consumeBody() {
    if (!this._response || this._resEnded) return;
    if (this._mode === "length") {
      if (this._buffer.length > 0 && this._remaining > 0) {
        const take = Math.min(this._remaining, this._buffer.length);
        const chunk = this._buffer.subarray(0, take);
        this._buffer = this._buffer.subarray(take);
        this._remaining -= take;
        this._emitBody(chunk);
      }
      if (this._remaining === 0) this._finishResponse();
      return;
    }
    if (this._mode === "close") {
      if (this._buffer.length > 0) {
        const chunk = this._buffer;
        this._buffer = Buffer.alloc(0);
        this._emitBody(chunk);
      }
      return;
    }
    if (this._mode === "chunked") {
      for (;;) {
        if (this._chunkState === "size") {
          const end = this._buffer.indexOf("\r\n");
          if (end < 0) return;
          const line = this._buffer.subarray(0, end).toString("latin1").split(";")[0].trim();
          this._buffer = this._buffer.subarray(end + 2);
          const size = parseInt(line, 16);
          if (!Number.isFinite(size) || size < 0) {
            this.destroy(httpError("chunked 响应格式错误", "ERR_INVALID_CHUNKED_ENCODING"));
            return;
          }
          if (size === 0) {
            this._chunkState = "trailers";
            continue;
          }
          this._chunkRemaining = size;
          this._chunkState = "data";
        }
        if (this._chunkState === "data") {
          if (this._buffer.length < this._chunkRemaining + 2) return;
          const chunk = this._buffer.subarray(0, this._chunkRemaining);
          this._buffer = this._buffer.subarray(this._chunkRemaining + 2);
          this._chunkRemaining = 0;
          this._chunkState = "size";
          this._emitBody(chunk);
          continue;
        }
        if (this._chunkState === "trailers") {
          const end = this._buffer.indexOf("\r\n");
          if (end < 0) return;
          if (end === 0) {
            this._buffer = this._buffer.subarray(2);
            this._finishResponse();
            return;
          }
          this._buffer = this._buffer.subarray(end + 2);
          continue;
        }
        return;
      }
    }
  }

  _onSocketEnd() {
    if (this._resEnded) return;
    if (this._mode === "close") {
      this._finishResponse();
      return;
    }
    if (this._response && !this._resEnded) this._finishResponse();
  }

  _emitBody(chunk) {
    if (this._destroyed || !this._response || chunk.length === 0) return;
    this._responseBytes += chunk.length;
    if (this._maxResponseBytes > 0 && this._responseBytes > this._maxResponseBytes) {
      this.destroy(httpError("响应体超过 MaxResponseBytes", "ERR_HTTP_RESPONSE_TOO_LARGE"));
      return;
    }
    this._response._push(chunk);
  }

  _finishResponse() {
    if (this._resEnded) return;
    this._resEnded = true;
    if (this._response) this._response._end();
    if (this._sock) {
      try { this._sock.destroy(); } catch (_) {}
    }
  }
}

// ClientRequest:缓存写入的请求体,在 end() 时一次性发出。
export class ClientRequest extends Emitter {
  constructor(options, callback, secure) {
    super();
    this._secure = secure;
    this._options = options;
    this.method = String(options.method || "GET").toUpperCase();
    this.path = options.path || "/";
    this._headers = {};
    const headers = options.headers || {};
    for (const key of Object.keys(headers)) this.setHeader(key, headers[key]);
    this._chunks = [];
    this._ended = false;
    this._destroyed = false;
    this._performed = false;
    this._timeout = Number(options.timeout) || 0;
    this.res = null;
    this.socket = null;
    this.aborted = false;
    this.reusedSocket = false;
    this._signal = options.signal || null;
    this._abortHandler = null;
    if (this._signal) {
      if (this._signal.aborted) {
        this._aborted = true;
      } else {
        this._abortHandler = () => {
          this.aborted = true;
          this.destroy(abortErrorFrom(this._signal));
        };
        this._signal.addEventListener("abort", this._abortHandler, { once: true });
      }
    }
    if (typeof callback === "function") this.once("response", callback);
  }

  setHeader(name, value) {
    this._headers[String(name)] = value;
    return this;
  }

  getHeader(name) {
    const lower = String(name).toLowerCase();
    for (const key of Object.keys(this._headers)) {
      if (key.toLowerCase() === lower) return this._headers[key];
    }
    return undefined;
  }

  getHeaders() {
    const out = {};
    for (const key of Object.keys(this._headers)) out[key.toLowerCase()] = this._headers[key];
    return out;
  }

  getHeaderNames() {
    return Object.keys(this._headers).map((k) => k.toLowerCase());
  }

  hasHeader(name) {
    return this.getHeader(name) !== undefined;
  }

  removeHeader(name) {
    const lower = String(name).toLowerCase();
    for (const key of Object.keys(this._headers)) {
      if (key.toLowerCase() === lower) delete this._headers[key];
    }
  }

  setTimeout(ms, callback) {
    this._timeout = Number(ms) || 0;
    if (typeof callback === "function") this.once("timeout", callback);
    return this;
  }

  write(chunk, encoding, callback) {
    if (this._ended) throw httpError("write after end", "ERR_STREAM_WRITE_AFTER_END");
    this._chunks.push(toBuffer(chunk, typeof encoding === "string" ? encoding : undefined));
    if (typeof encoding === "function") encoding();
    else if (typeof callback === "function") callback();
    return true;
  }

  end(chunk, encoding, callback) {
    if (chunk !== undefined && chunk !== null && typeof chunk !== "function") {
      this.write(chunk, typeof encoding === "string" ? encoding : undefined);
    }
    if (typeof chunk === "function") callback = chunk;
    else if (typeof encoding === "function") callback = encoding;
    if (this._ended) return this;
    this._ended = true;
    this._perform();
    if (typeof callback === "function") callback();
    return this;
  }

  abort() {
    this.aborted = true;
    this.destroy(abortErrorFrom(this._signal));
  }

  destroy(err) {
    if (this._destroyed) return this;
    this._destroyed = true;
    if (this._signal && this._abortHandler) {
      try { this._signal.removeEventListener("abort", this._abortHandler); } catch (_) {}
      this._signal = null;
      this._abortHandler = null;
    }
    if (this.aborted) this.emit("abort");
    if (err) this.emit("error", err);
    this.emit("close");
    return this;
  }

  flushHeaders() {
    return this;
  }

  _perform() {
    if (this._performed || this._destroyed) return;
    this._performed = true;
    if (this._aborted || (this._signal && this._signal.aborted)) {
      const err = abortErrorFrom(this._signal);
      queueMicrotask(() => {
        if (this._destroyed) return;
        this._destroyed = true;
        if (this.listenerCount("error") === 0) throw err;
        this.emit("error", err);
        this.emit("close");
      });
      return;
    }
    const spec = {
      url: buildURL(this._options, this._secure),
      method: this.method,
      headers: this._plainHeaders(),
      body: Buffer.concat(this._chunks).toString("base64"),
      timeoutMs: this._timeout,
      insecure: this._options.rejectUnauthorized === false,
      maxBody: Number(this._options.maxBody) || 0,
    };
    let payload;
    try {
      if (!host || typeof host.httpDo !== "function") {
        throw httpError("Cannot import \"node:http\": 宿主未安装网络能力", "ERR_ACCESS_DENIED");
      }
      payload = JSON.parse(host.httpDo(JSON.stringify(spec)));
    } catch (err) {
      const failure = expose("EHTTP", "HTTP 请求失败(" + spec.method + " " + spec.url + ")", err);
      if (this.listenerCount("error") === 0) throw failure;
      this.emit("error", failure);
      this._destroyed = true;
      return;
    }
    if (this._aborted) return;
    const res = new IncomingMessage(payload);
    res.url = this.path;
    res.method = this.method;
    this.res = res;
    this.emit("response", res);
  }

  _plainHeaders() {
    const out = {};
    for (const key of Object.keys(this._headers)) {
      const value = this._headers[key];
      if (value === undefined || value === null) continue;
      out[key] = Array.isArray(value) ? value.join(", ") : String(value);
    }
    if (out["Content-Length"] === undefined && out["content-length"] === undefined && this._chunks.length > 0) {
      out["Content-Length"] = String(Buffer.concat(this._chunks).length);
    }
    return out;
  }
}
ClientRequest.prototype.addListener = ClientRequest.prototype.on;

function normalizeArgs(args, secure) {
  let options = args[0];
  let callback = args[1];
  if (typeof options === "string") {
    const extra = args[1] && typeof args[1] === "object" ? args[1] : {};
    callback = typeof args[1] === "function" ? args[1] : args[2];
    const target = new URL(options);
    const merged = { ...extra };
    merged.protocol = merged.protocol || target.protocol || (secure ? "https:" : "http:");
    merged.hostname = merged.hostname || target.hostname;
    merged.port = merged.port || target.port;
    merged.path = merged.path || target.pathname + target.search;
    merged.href = target.href;
    return { options: merged, callback };
  }
  if (options === undefined || options === null) options = {};
  if (typeof callback !== "function") callback = undefined;
  return { options, callback };
}

function requestPlain(...args) {
  const { options, callback } = normalizeArgs(args, false);
  if (globalThis.__adaResident === true && host && host.net && typeof host.net.dialAsync === "function") {
    return new StreamingClientRequest(options, callback, false);
  }
  return new ClientRequest(options, callback, false);
}

function getPlain(...args) {
  const req = requestPlain(...args);
  req.end();
  return req;
}

function requestSecure(...args) {
  const { options, callback } = normalizeArgs(args, true);
  if (globalThis.__adaResident === true && host && host.net && typeof host.net.tlsDialAsync === "function") {
    return new StreamingClientRequest(options, callback, true);
  }
  return new ClientRequest(options, callback, true);
}

function getSecure(...args) {
  const req = requestSecure(...args);
  req.end();
  return req;
}

export const STATUS_CODES = {
  100: "Continue", 101: "Switching Protocols", 102: "Processing", 103: "Early Hints",
  200: "OK", 201: "Created", 202: "Accepted", 203: "Non-Authoritative Information",
  204: "No Content", 205: "Reset Content", 206: "Partial Content",
  300: "Multiple Choices", 301: "Moved Permanently", 302: "Found", 303: "See Other",
  304: "Not Modified", 307: "Temporary Redirect", 308: "Permanent Redirect",
  400: "Bad Request", 401: "Unauthorized", 402: "Payment Required", 403: "Forbidden",
  404: "Not Found", 405: "Method Not Allowed", 406: "Not Acceptable",
  407: "Proxy Authentication Required", 408: "Request Timeout", 409: "Conflict",
  410: "Gone", 411: "Length Required", 412: "Precondition Failed",
  413: "Payload Too Large", 414: "URI Too Long", 415: "Unsupported Media Type",
  416: "Range Not Satisfiable", 417: "Expectation Failed", 418: "I'm a Teapot",
  421: "Misdirected Request", 422: "Unprocessable Entity", 423: "Locked",
  424: "Failed Dependency", 425: "Too Early", 426: "Upgrade Required",
  428: "Precondition Required", 429: "Too Many Requests",
  431: "Request Header Fields Too Large", 451: "Unavailable For Legal Reasons",
  500: "Internal Server Error", 501: "Not Implemented", 502: "Bad Gateway",
  503: "Service Unavailable", 504: "Gateway Timeout", 505: "HTTP Version Not Supported",
  506: "Variant Also Negotiates", 507: "Insufficient Storage", 508: "Loop Detected",
  510: "Not Extended", 511: "Network Authentication Required",
};

export const METHODS = [
  "ACL", "BIND", "CHECKOUT", "CONNECT", "COPY", "DELETE", "GET", "HEAD", "LINK",
  "LOCK", "M-SEARCH", "MERGE", "MKACTIVITY", "MKCALENDAR", "MKCOL", "MOVE",
  "NOTIFY", "OPTIONS", "PATCH", "POST", "PROPFIND", "PROPPATCH", "PURGE", "PUT",
  "REBIND", "REPORT", "SEARCH", "SOURCE", "SUBSCRIBE", "TRACE", "UNBIND",
  "UNLINK", "UNLOCK", "UNSUBSCRIBE",
];

// Agent 只是占位:同步客户端没有连接池句柄可以暴露。
class ServerRequest extends Emitter {
  constructor(method, url, httpVersion, headers, rawHeaders, socket) {
    super();
    this.method = method;
    this.url = url;
    this.httpVersion = httpVersion;
    this.headers = headers;
    this.rawHeaders = rawHeaders;
    this.socket = socket;
    this.complete = true;
  }
}

class ServerResponse extends Emitter {
  constructor(socket, keepAlive, httpVersion) {
    super();
    this.socket = socket;
    this.statusCode = 200;
    this.statusMessage = "";
    this.headersSent = false;
    this.finished = false;
    this.writableEnded = false;
    this.shouldKeepAlive = keepAlive;
    this._httpVersion = httpVersion;
    this._headers = {};
    this._chunked = false;
    this._wrote = false;
    this._onFinish = null;
  }

  setHeader(name, value) {
    if (this.headersSent) throw httpError("响应头已发送", "ERR_HTTP_HEADERS_SENT");
    this._headers[String(name).toLowerCase()] = { name: String(name), value: String(value) };
    return this;
  }

  getHeader(name) {
    const item = this._headers[String(name).toLowerCase()];
    return item ? item.value : undefined;
  }

  removeHeader(name) {
    delete this._headers[String(name).toLowerCase()];
  }

  hasHeader(name) {
    return Object.prototype.hasOwnProperty.call(this._headers, String(name).toLowerCase());
  }

  writeHead(status, message, headers) {
    this.statusCode = Number(status) || 200;
    if (typeof message === "string") this.statusMessage = message;
    else if (message && typeof message === "object") headers = message;
    if (headers && typeof headers === "object") {
      for (const key of Object.keys(headers)) this.setHeader(key, headers[key]);
    }
    if (!this.hasHeader("content-length") && !this.hasHeader("transfer-encoding") && this._httpVersion === "HTTP/1.1") {
      this._chunked = true;
    }
    this._writeHead();
    return this;
  }

  _writeHead() {
    if (this.headersSent) return;
    const connection = this.getHeader("connection");
    if (connection !== undefined && String(connection).toLowerCase() === "close") {
      this.shouldKeepAlive = false;
    }
    if (!this.hasHeader("content-length") && !this.hasHeader("transfer-encoding")) {
      if (this._chunked) this.setHeader("Transfer-Encoding", "chunked");
      else this.setHeader("content-length", "0");
    }
    if (!this.hasHeader("connection")) {
      this.setHeader("Connection", this.shouldKeepAlive ? "keep-alive" : "close");
    }
    this.headersSent = true;
    const text = this.statusMessage || STATUS_CODES[this.statusCode] || "Unknown";
    let head = "HTTP/1.1 " + this.statusCode + " " + text + "\r\n";
    for (const key of Object.keys(this._headers)) {
      const item = this._headers[key];
      head += item.name + ": " + item.value + "\r\n";
    }
    head += "\r\n";
    this.socket.write(head);
  }

  write(chunk, encoding) {
    if (this.finished) return false;
    if (!this.headersSent) {
      if (!this.hasHeader("content-length") && !this.hasHeader("transfer-encoding")) {
        this._chunked = this._httpVersion === "HTTP/1.1";
      }
      this._writeHead();
    }
    const buf = typeof chunk === "string" ? Buffer.from(chunk, typeof encoding === "string" ? encoding : "utf8") : chunk;
    this._wrote = true;
    if (this._chunked) {
      this.socket.write(buf.length.toString(16) + "\r\n");
      this.socket.write(buf);
      this.socket.write("\r\n");
    } else {
      this.socket.write(buf);
    }
    return true;
  }

  end(chunk, encoding, callback) {
    if (typeof chunk === "function") {
      callback = chunk;
      chunk = undefined;
    }
    if (this.finished) {
      if (typeof callback === "function") queueMicrotask(callback);
      return this;
    }
    if (!this.headersSent) {
      if (chunk !== undefined && chunk !== null && !this._wrote) {
        const buf = typeof chunk === "string" ? Buffer.from(chunk, typeof encoding === "string" ? encoding : "utf8") : chunk;
        this.setHeader("content-length", String(buf.length));
        this._writeHead();
        this.socket.write(buf);
      } else {
        if (chunk !== undefined && chunk !== null) this.write(chunk, encoding);
        if (!this.headersSent) {
          if (!this.hasHeader("content-length") && !this.hasHeader("transfer-encoding")) {
            this.setHeader("content-length", "0");
          }
          this._writeHead();
        }
        if (this._chunked) this.socket.write("0\r\n\r\n");
      }
    } else {
      if (chunk !== undefined && chunk !== null) this.write(chunk, encoding);
      if (this._chunked) this.socket.write("0\r\n\r\n");
    }
    this.finished = true;
    this.writableEnded = true;
    this.emit("finish");
    if (typeof callback === "function") queueMicrotask(callback);
    if (typeof this._onFinish === "function") this._onFinish();
    else if (!this.shouldKeepAlive) this.socket.end();
    return this;
  }
}

// Server 是基于 net.Server 的最小 HTTP/1.1 服务端，支持 keep-alive 与 chunked 响应。
export class Server extends Emitter {
  constructor(options, requestListener, factory) {
    super();
    if (typeof options === "function") {
      requestListener = options;
      options = {};
    }
    this._options = options || {};
    this._requestListener = typeof requestListener === "function" ? requestListener : null;
    this._net = (typeof factory === "function" ? factory : createNetServer)((socket) => this._handleConnection(socket));
    this._net.on("error", (err) => this.emit("error", err));
    this._net.on("listening", () => this.emit("listening"));
    this._net.on("close", () => this.emit("close"));
    this.headersTimeout = Number(this._options.headersTimeout) || 30000;
    this.requestTimeout = Number(this._options.requestTimeout) || 60000;
    this.keepAliveTimeout = Number(this._options.keepAliveTimeout) || 5000;
    this.maxBodyBytes = Number(this._options.maxBodyBytes) || 0;
  }

  listen(...args) {
    this._net.listen(...args);
    return this;
  }

  close(callback) {
    this._net.close(callback);
    return this;
  }

  address() {
    return this._net.address();
  }

  _handleConnection(socket) {
    let buffer = Buffer.alloc(0);
    let pending = null;
    let busy = false;
    let headerTimer = null;
    let bodyTimer = null;
    let keepTimer = null;

    const clear = (timer) => {
      if (timer !== null) {
        try { clearTimeout(timer); } catch (_) {}
      }
      return null;
    };
    const clearAll = () => {
      headerTimer = clear(headerTimer);
      bodyTimer = clear(bodyTimer);
      keepTimer = clear(keepTimer);
    };
    const arm = (ms, fn) => {
      const timer = setTimeout(fn, ms);
      if (timer && typeof timer.unref === "function") timer.unref();
      return timer;
    };
    const fail = (status, text) => {
      clearAll();
      if (!socket.destroyed) socket.end("HTTP/1.1 " + status + " " + text + "\r\nConnection: close\r\n\r\n");
    };
    const finalize = (request, body) => {
      clearAll();
      const connection = String(request.headers.connection || "").toLowerCase();
      const keepAlive = request.version === "HTTP/1.1" ? connection !== "close" : connection === "keep-alive";
      const req = new ServerRequest(request.method, request.url, request.version, request.headers, request.rawHeaders, socket);
      const res = new ServerResponse(socket, keepAlive, request.version);
      busy = true;
      res._onFinish = () => {
        busy = false;
        if (res.shouldKeepAlive && !socket.destroyed) {
          if (buffer.length > 0) process();
          else keepTimer = arm(this.keepAliveTimeout, () => socket.end());
        } else {
          socket.end();
        }
      };
      if (this._requestListener) this._requestListener(req, res);
      this.emit("request", req, res);
      queueMicrotask(() => {
        if (body.length > 0) req.emit("data", body);
        req.emit("end");
      });
    };

    const readChunks = () => {
      for (;;) {
        if (pending.state === "size") {
          const end = buffer.indexOf("\r\n");
          if (end < 0) return false;
          const line = buffer.subarray(0, end).toString("latin1").split(";")[0].trim();
          buffer = buffer.subarray(end + 2);
          const size = parseInt(line, 16);
          if (!Number.isFinite(size) || size < 0) {
            fail(400, "Bad Request");
            return false;
          }
          if (size === 0) {
            pending.state = "trailers";
            continue;
          }
          pending.remaining = size;
          pending.state = "data";
        }
        if (pending.state === "data") {
          if (buffer.length < pending.remaining + 2) return false;
          pending.chunks.push(buffer.subarray(0, pending.remaining));
          pending.bodyBytes += pending.remaining;
          if (this.maxBodyBytes > 0 && pending.bodyBytes > this.maxBodyBytes) {
            fail(413, "Payload Too Large");
            return false;
          }
          buffer = buffer.subarray(pending.remaining + 2);
          pending.state = "size";
          continue;
        }
        if (pending.state === "trailers") {
          const end = buffer.indexOf("\r\n");
          if (end < 0) return false;
          if (end === 0) {
            buffer = buffer.subarray(2);
            pending.state = "done";
            return true;
          }
          buffer = buffer.subarray(end + 2);
          continue;
        }
        return pending.state === "done";
      }
    };

    const process = () => {
      if (busy || socket.destroyed) return;
      if (!pending) {
        const end = buffer.indexOf("\r\n\r\n");
        if (end < 0) {
          if (buffer.length > maxHeaderSize) {
            fail(431, "Request Header Fields Too Large");
            return;
          }
          if (headerTimer === null) headerTimer = arm(this.headersTimeout, () => socket.destroy());
          return;
        }
        headerTimer = clear(headerTimer);
        const head = buffer.subarray(0, end).toString("latin1");
        buffer = buffer.subarray(end + 4);
        const lines = head.split("\r\n");
        const requestLine = lines.shift() || "";
        const parts = requestLine.split(" ");
        if (parts.length < 3) {
          fail(400, "Bad Request");
          return;
        }
        const headers = {};
        const rawHeaders = [];
        for (const line of lines) {
          const i = line.indexOf(":");
          if (i <= 0) continue;
          const name = line.slice(0, i).trim();
          const value = line.slice(i + 1).trim();
          rawHeaders.push(name, value);
          const key = name.toLowerCase();
          headers[key] = headers[key] === undefined ? value : headers[key] + ", " + value;
        }
        const chunked = String(headers["transfer-encoding"] || "").toLowerCase().includes("chunked");
        let contentLength = 0;
        if (!chunked) {
          contentLength = Math.max(0, Number(headers["content-length"] || 0) || 0);
          if (this.maxBodyBytes > 0 && contentLength > this.maxBodyBytes) {
            fail(413, "Payload Too Large");
            return;
          }
        }
        pending = {
          method: parts[0],
          url: parts[1],
          version: parts[2],
          headers,
          rawHeaders,
          chunked,
          contentLength,
          need: contentLength,
          body: Buffer.alloc(0),
          chunks: [],
          remaining: 0,
          bodyBytes: 0,
          state: chunked ? "size" : "body",
        };
      }
      if (pending.chunked) {
        if (!readChunks()) {
          if (!socket.destroyed && bodyTimer === null) bodyTimer = arm(this.requestTimeout, () => socket.destroy());
          return;
        }
        const request = pending;
        pending = null;
        finalize(request, Buffer.concat(request.chunks));
        return;
      }
      if (pending.need > buffer.length) {
        pending.body = Buffer.concat([pending.body, buffer]);
        pending.need -= buffer.length;
        buffer = Buffer.alloc(0);
        if (bodyTimer === null) bodyTimer = arm(this.requestTimeout, () => socket.destroy());
        return;
      }
      const body = Buffer.concat([pending.body, buffer.subarray(0, pending.need)]);
      buffer = buffer.subarray(pending.need);
      const request = pending;
      pending = null;
      finalize(request, body);
    };

    socket.on("data", (chunk) => {
      keepTimer = clear(keepTimer);
      buffer = Buffer.concat([buffer, Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk))]);
      process();
    });
    socket.on("close", clearAll);
    socket.on("error", () => {});
    process();
  }
}
export class Agent {
  constructor(options) {
    this.options = options || {};
    this.maxSockets = this.options.maxSockets || Infinity;
  }
  destroy() {}
}

function unsupported(name) {
  throw httpError("http." + name + " 不提供:同步运行时无法接收连接", "ERR_NOT_SUPPORTED");
}

export function makeModule(secure) {
  const api = {
    request: secure ? requestSecure : requestPlain,
    get: secure ? getSecure : getPlain,
    ClientRequest,
    IncomingMessage,
    Agent,
    STATUS_CODES,
    METHODS,
    globalAgent: new Agent(),
    maxHeaderSize: 16384,
    createServer: secure
      ? (options, requestListener) => {
          if (typeof options === "function") {
            requestListener = options;
            options = {};
          }
          return new Server(options, requestListener, (cb) => createTLSServer(options, cb));
        }
      : (options, requestListener) => new Server(options, requestListener),
    Server: Server,
  };
  api.__secure = secure;
  return api;
}

const http = makeModule(false);

export const request = http.request;
export const get = http.get;
export const globalAgent = http.globalAgent;
export const maxHeaderSize = http.maxHeaderSize;
export const createServer = http.createServer;
export default http;
