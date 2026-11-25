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
// 不提供 createServer / listen:同步运行时无法接收连接(建立连接需要等待,
// 等待就会阻塞掉唯一的 worker)。用法与 Node 一致的部分:request / get /
// ClientRequest / IncomingMessage / headers / statusCode / 超时 / 重定向原样返回。
import { Buffer } from "node:buffer";
import { URL } from "node:url";

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
    this.destroy();
  }

  destroy(err) {
    if (this._destroyed) return this;
    this._destroyed = true;
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
  return new ClientRequest(options, callback, false);
}

function getPlain(...args) {
  const req = requestPlain(...args);
  req.end();
  return req;
}

function requestSecure(...args) {
  const { options, callback } = normalizeArgs(args, true);
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
    createServer: () => unsupported("createServer"),
    Server: function Server() {
      unsupported("Server");
    },
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
