// node:net —— 异步 TCP 客户端。
//
// connect() 立即返回，成功后 emit 'connect'；data 由宿主 readAsync 泵驱动，
// 写入在连接建立前排队。read(n) 保留为同步扩展（从内部缓冲取）。
// createServer / listen 在 LoopResident 下提供；connect 支持 AbortSignal。
import { Buffer } from "node:buffer";
import { EventEmitter } from "node:events";

const host = globalThis.__adaHost;

function abortErrorFrom(signal) {
  const reason = signal && signal.reason;
  if (reason instanceof Error) return reason;
  const err = new Error("The operation was aborted");
  err.name = "AbortError";
  err.code = "ABORT_ERR";
  return err;
}

const IPV4_PART = "(25[0-5]|2[0-4]\\d|1\\d\\d|[1-9]?\\d)";
const IPV4_RE = new RegExp("^" + IPV4_PART + "\\." + IPV4_PART + "\\." + IPV4_PART + "\\." + IPV4_PART + "$");

function netError(message, code) {
  const err = new Error(message);
  err.code = code || "ENET";
  return err;
}

// isIPv4 与 Node 一致:拒绝前导零、拒绝超出 255、拒绝空白。
export function isIPv4(input) {
  if (typeof input !== "string") return false;
  if (!IPV4_RE.test(input)) return false;
  return true;
}

// isIPv6 接受完整形式、::压缩、以及内嵌 IPv4(::ffff:1.2.3.4)。
export function isIPv6(input) {
  if (typeof input !== "string" || input.length === 0) return false;
  let text = input;
  const zone = text.indexOf("%");
  if (zone >= 0) text = text.slice(0, zone);
  if (!/^[0-9a-fA-F:.]+$/.test(text)) return false;
  if (text.indexOf(".") >= 0) {
    const lastColon = text.lastIndexOf(":");
    if (lastColon < 0 || !isIPv4(text.slice(lastColon + 1))) return false;
    text = text.slice(0, lastColon + 1) + "0:0";
  }
  const halves = text.split("::");
  if (halves.length > 2) return false;
  const groups = (part) => (part === "" ? [] : part.split(":"));
  const head = groups(halves[0]);
  const tail = halves.length === 2 ? groups(halves[1]) : [];
  const valid = (list) => list.every((g) => /^[0-9a-fA-F]{1,4}$/.test(g));
  if (!valid(head) || !valid(tail)) return false;
  const total = head.length + tail.length;
  if (halves.length === 2) return total < 8;
  return total === 8;
}

// isIP 返回 0 / 4 / 6,与 Node 相同。
export function isIP(input) {
  if (isIPv4(input)) return 4;
  if (isIPv6(input)) return 6;
  return 0;
}

// SocketAddress 解析 host:port / [ipv6]:port / 纯端口三种写法。
export class SocketAddress {
  constructor(options) {
    if (!options || typeof options !== "object") throw new TypeError("SocketAddress 需要 options");
    this.address = options.address || "localhost";
    this.family = options.family || (isIP(this.address) === 6 ? "IPv6" : "IPv4");
    this.port = Number(options.port) || 0;
    if (this.port <= 0 || this.port > 65535) throw netError("端口必须在 1-65535 之间", "ERR_SOCKET_BAD_PORT");
    this.flowlabel = options.flowlabel || 0;
  }
  static parse(input) {
    if (typeof input !== "string") throw new TypeError("SocketAddress.parse 需要字符串");
    let address = input;
    let port = 0;
    const bracket = /^\[([^\]]+)\]:(\d+)$/.exec(input);
    if (bracket) {
      address = bracket[1];
      port = Number(bracket[2]);
    } else {
      const idx = input.lastIndexOf(":");
      if (idx > 0 && input.indexOf(":") === idx) {
        address = input.slice(0, idx);
        port = Number(input.slice(idx + 1));
      } else if (/^\d+$/.test(input)) {
        return new SocketAddress({ address: "localhost", port: Number(input) });
      }
    }
    if (!Number.isFinite(port) || port <= 0) throw netError("无法解析地址: " + input, "ERR_INVALID_ARG_VALUE");
    return new SocketAddress({ address, port });
  }
}

function hostPort(hostname, port) {
  const addr = hostname || "localhost";
  return (isIP(addr) === 6 && addr.indexOf("[") !== 0 ? "[" + addr + "]" : addr) + ":" + port;
}

function normalizeConnectArgs(args) {
  if (args[0] && typeof args[0] === "object") {
    return { options: args[0], callback: typeof args[1] === "function" ? args[1] : undefined };
  }
  const port = Number(args[0]);
  let hostname = "localhost";
  let callback;
  if (typeof args[1] === "string") {
    hostname = args[1];
    if (typeof args[2] === "function") callback = args[2];
  } else if (typeof args[1] === "function") {
    callback = args[1];
  }
  return { options: { port, host: hostname }, callback };
}

// Socket 是 Node 形状的异步客户端：connect 返回后由事件循环结算，
// data 事件由宿主 readAsync 泵驱动；read(n) 仍保留为同步扩展（从内部缓冲取）。
export class Socket extends EventEmitter {
  constructor(options) {
    super();
    this.connecting = false;
    this.pending = false;
    this.destroyed = false;
    this.readyState = "closed";
    this.remoteAddress = undefined;
    this.remoteFamily = undefined;
    this.remotePort = undefined;
    this.localAddress = undefined;
    this.localPort = undefined;
    this.bytesRead = 0;
    this.bytesWritten = 0;
    this.bufferSize = 0;
    this.timeout = 0;
    this._handle = 0;
    this._encoding = null;
    this._options = options || {};
    this._buffer = [];
    this._queue = [];
    this._paused = false;
    this._reading = false;
    this._eof = false;
    this._ending = false;
    this._timeoutTimer = null;
  }

  connect(...args) {
    const { options, callback } = normalizeConnectArgs(args);
    if (typeof callback === "function") this.once("connect", callback);
    const port = Number(options.port);
    if (!Number.isFinite(port) || port <= 0) throw netError("connect 需要合法端口", "ERR_SOCKET_BAD_PORT");
    if (!host || !host.net || (typeof host.net.dial !== "function" && typeof host.net.dialAsync !== "function" && typeof host.net.tlsDialAsync !== "function")) {
      throw netError("Cannot import \"node:net\": 宿主未安装网络能力", "ERR_ACCESS_DENIED");
    }
    if (typeof options.timeout === "number") this.timeout = options.timeout;
    const signal = options.signal;
    if (signal) {
      if (signal.aborted) {
        this._abortSignal = signal;
        queueMicrotask(() => {
          if (this.destroyed) return;
          this.readyState = "closed";
          const err = abortErrorFrom(signal);
          if (this.listenerCount("error") === 0) queueMicrotask(() => { throw err; });
          else this.emit("error", err);
          this.emit("close");
        });
        return this;
      }
      this._abortSignal = signal;
      this._abortHandler = () => this.destroy(abortErrorFrom(signal));
      signal.addEventListener("abort", this._abortHandler, { once: true });
    }
    this.connecting = true;
    this.pending = true;
    this.readyState = "opening";
    const address = hostPort(options.host, port);
    this._armTimeout();

    const ok = (id) => {
      if (this.destroyed) {
        try { host.net.close(id); } catch (_) {}
        return;
      }
      this._handle = id;
      this.readyState = "open";
      this.connecting = false;
      this.pending = false;
      try {
        this._applyAddr(JSON.parse(host.net.addr(id)));
      } catch (_) {}
      this.emit("connect");
      this.emit("ready");
      this._flushQueue();
      this._startReadLoop();
    };

    const fail = (err) => {
      if (this.destroyed) return;
      this.connecting = false;
      this.pending = false;
      this.readyState = "closed";
      const cause = err && err.message ? err.message : String(err);
      const code = /ERR_ACCESS_DENIED|拒绝/.test(cause) ? "ERR_ACCESS_DENIED" : "ECONNREFUSED";
      const failure = netError("TCP 连接 " + address + " 失败: " + cause, code);
      if (this.listenerCount("error") === 0) {
        queueMicrotask(() => { throw failure; });
      } else {
        this.emit("error", failure);
      }
      this.emit("close");
    };

    let pending;
    try {
      pending = this._dial(address, this.timeout);
    } catch (err) {
      // 权限拒绝等同步错误也要异步投递，调用方才有机会先挂 error 监听。
      queueMicrotask(() => fail(err));
      return this;
    }
    Promise.resolve(pending).then(ok, fail);
    return this;
  }

  // _dial 由 net.Socket 与 tls.TLSSocket 各自实现。
  _dial(address, timeout) {
    if (typeof host.net.dialAsync === "function") return host.net.dialAsync(address, timeout);
    if (typeof host.net.dial === "function") return Promise.resolve(host.net.dial(address, timeout));
    return Promise.reject(netError("宿主未安装 TCP 连接能力", "ERR_ACCESS_DENIED"));
  }

  _adopt(info) {
    this._handle = Number(info.id);
    this.readyState = "open";
    this.connecting = false;
    this.pending = false;
    try {
      this._applyAddr(info);
    } catch (_) {}
    this.emit("connect");
    this.emit("ready");
    this._startReadLoop();
    return this;
  }

  _applyAddr(info) {
    const remote = splitAddr(info.remoteAddress);
    if (remote) {
      this.remoteAddress = remote.address;
      this.remotePort = remote.port;
      this.remoteFamily = isIP(remote.address) === 6 ? "IPv6" : "IPv4";
    }
    const local = splitAddr(info.localAddress);
    if (local) {
      this.localAddress = local.address;
      this.localPort = local.port;
    }
  }

  _armTimeout() {
    this._clearTimeout();
    if (this.timeout > 0) {
      this._timeoutTimer = setTimeout(() => {
        if (!this.destroyed) this.emit("timeout");
      }, this.timeout);
      if (this._timeoutTimer && typeof this._timeoutTimer.unref === "function") this._timeoutTimer.unref();
    }
  }

  _clearTimeout() {
    if (this._timeoutTimer !== null) {
      try { clearTimeout(this._timeoutTimer); } catch (_) {}
      this._timeoutTimer = null;
    }
  }

  _flushQueue() {
    if (this._handle === 0 || this.destroyed) return;
    const queue = this._queue;
    this._queue = [];
    for (const buf of queue) {
      try {
        host.net.write(this._handle, buf.toString("base64"));
      } catch (err) {
        this.destroy(err);
        return;
      }
    }
    if (this._ending) this.destroy();
  }

  on(name, listener) {
    super.on(name, listener);
    if (name === "data" && this._handle !== 0 && !this._reading) this._startReadLoop();
    return this;
  }

  once(name, listener) {
    super.once(name, listener);
    if (name === "data" && this._handle !== 0 && !this._reading) this._startReadLoop();
    return this;
  }

  _startReadLoop() {
    if (this.destroyed || this._handle === 0 || this._reading) return;
    const rawRead = host.net.readAsync;
    if (typeof rawRead !== "function") return;
    // Scoped 模式没有 data 监听时保留旧的同步 read(n) 路径，避免一个未消费的
    // 读把调用边界的事件循环一直撑住；Resident 模式则保持 Node 的常驻语义。
    if (this.listenerCount("data") === 0 && globalThis.__adaResident !== true) return;
    this._reading = true;
    const pump = () => {
      if (this.destroyed || this._handle === 0 || this._paused) {
        this._reading = false;
        return;
      }
      Promise.resolve(rawRead(this._handle, 64 * 1024)).then(
        (raw) => {
          if (this.destroyed) {
            this._reading = false;
            return;
          }
          let result;
          try {
            result = JSON.parse(raw);
          } catch (err) {
            this._reading = false;
            this.destroy(err);
            return;
          }
          if (result.eof) {
            this._eof = true;
            this._reading = false;
            this.emit("end");
            this.destroy();
            return;
          }
          const buf = Buffer.from(result.data || "", "base64");
          if (buf.length > 0) {
            this.bytesRead += buf.length;
            if (this.listenerCount("data") > 0) {
              this.emit("data", this._encoding ? buf.toString(this._encoding) : buf);
            } else {
              this._buffer.push(buf);
              this.bufferSize += buf.length;
            }
          }
          if (this._paused) {
            this._reading = false;
            return;
          }
          pump();
        },
        (err) => {
          this._reading = false;
          if (!this.destroyed) this.destroy(err);
        },
      );
    };
    pump();
  }

  setEncoding(encoding) {
    this._encoding = encoding || "utf8";
    return this;
  }

  setTimeout(ms, callback) {
    this.timeout = Number(ms) || 0;
    if (typeof callback === "function") this.once("timeout", callback);
    this._armTimeout();
    return this;
  }

  setNoDelay() {
    return this;
  }

  setKeepAlive() {
    return this;
  }

  ref() {
    return this;
  }

  unref() {
    return this;
  }

  pause() {
    this._paused = true;
    return this;
  }

  resume() {
    if (!this._paused) return this;
    this._paused = false;
    this._startReadLoop();
    return this;
  }

  isPaused() {
    return this._paused;
  }

  write(data, encoding, callback) {
    if (this.destroyed) throw netError("socket 已关闭", "ERR_SOCKET_CLOSED");
    const buf = Buffer.isBuffer(data) ? data : Buffer.from(String(data), typeof encoding === "string" ? encoding : "utf8");
    this.bytesWritten += buf.length;
    if (typeof encoding === "function") callback = encoding;
    if (this._handle === 0 || this.connecting) {
      this._queue.push(buf);
      this.pending = true;
      if (typeof callback === "function") queueMicrotask(callback);
      return true;
    }
    try {
      host.net.write(this._handle, buf.toString("base64"));
    } catch (err) {
      this.destroy(err);
      return false;
    }
    if (typeof callback === "function") queueMicrotask(callback);
    return true;
  }

  // read(n) 是本运行时的同步扩展：从内部缓冲取数据，不阻塞事件循环。
  read(n) {
    if (this.destroyed) return null;
    const want = Number(n) > 0 ? Number(n) : 4096;
    if (this._buffer.length === 0) {
      if (typeof host.net.read === "function" && this._handle !== 0 && !this._reading) {
        const result = JSON.parse(host.net.read(this._handle, want));
        if (result.eof) {
          this.emit("end");
          this.destroy();
          return null;
        }
        const buf = Buffer.from(result.data || "", "base64");
        if (buf.length === 0) return null;
        this.bytesRead += buf.length;
        return this._encoding ? buf.toString(this._encoding) : buf;
      }
      return null;
    }
    let out = Buffer.alloc(0);
    while (this._buffer.length > 0 && out.length < want) {
      const chunk = this._buffer[0];
      const take = Math.min(chunk.length, want - out.length);
      out = Buffer.concat([out, chunk.subarray(0, take)]);
      if (take === chunk.length) this._buffer.shift();
      else this._buffer[0] = chunk.subarray(take);
    }
    this.bufferSize = Math.max(0, this.bufferSize - out.length);
    if (out.length === 0) return null;
    return this._encoding ? out.toString(this._encoding) : out;
  }

  address() {
    return { address: this.localAddress, family: this.remoteFamily, port: this.localPort };
  }

  end(data, encoding) {
    if (data !== undefined && data !== null) this.write(data, encoding);
    this._ending = true;
    if (this._handle !== 0 && this._queue.length === 0) this.destroy();
    return this;
  }

  destroy(err) {
    if (this.destroyed) return this;
    this.destroyed = true;
    this.readyState = "closed";
    if (this._abortSignal && this._abortHandler) {
      try { this._abortSignal.removeEventListener("abort", this._abortHandler); } catch (_) {}
      this._abortSignal = null;
      this._abortHandler = null;
    }
    this._clearTimeout();
    if (this._handle !== 0) {
      try {
        host.net.close(this._handle);
      } catch (_) {}
      this._handle = 0;
    }
    this._buffer = [];
    this.bufferSize = 0;
    if (err) this.emit("error", err);
    this.emit("close");
    return this;
  }

  resetAndDestroy() {
    return this.destroy();
  }
}
function splitAddr(value) {
  if (typeof value !== "string" || value === "") return null;
  const idx = value.lastIndexOf(":");
  if (idx < 0) return null;
  return { address: value.slice(0, idx).replace(/^\[|\]$/g, ""), port: Number(value.slice(idx + 1)) };
}

export function connect(...args) {
  const socket = new Socket();
  return socket.connect(...args);
}

export const createConnection = connect;

export function createServer(options, connectionListener) {
  if (typeof options === "function") {
    connectionListener = options;
    options = {};
  }
  if (globalThis.__adaResident !== true) {
    throw netError("net.createServer 需要 LoopResident:服务端依赖常驻事件循环", "ERR_NOT_SUPPORTED");
  }
  return new Server(options, connectionListener);
}

export class Server extends EventEmitter {
  constructor(options, connectionListener) {
    super();
    this._options = options || {};
    this._handle = 0;
    this._listening = false;
    this._closed = false;
    this._accepting = false;
    this._addr = null;
    this.connections = 0;
    this.maxConnections = Number(this._options.maxConnections) || 0;
    if (typeof connectionListener === "function") this.on("connection", connectionListener);
  }

  listen(...args) {
    let port;
    let hostname = "0.0.0.0";
    let callback;
    if (args[0] && typeof args[0] === "object") {
      port = Number(args[0].port);
      hostname = args[0].host || hostname;
      callback = typeof args[1] === "function" ? args[1] : undefined;
    } else {
      port = Number(args[0]);
      if (typeof args[1] === "string") {
        hostname = args[1];
        if (typeof args[2] === "function") callback = args[2];
        else if (typeof args[3] === "function") callback = args[3];
      } else if (typeof args[1] === "function") {
        callback = args[1];
      }
    }
    if (!Number.isFinite(port) || port < 0 || port > 65535) {
      const err = netError("listen 需要合法端口", "ERR_SOCKET_BAD_PORT");
      queueMicrotask(() => this.emit("error", err));
      return this;
    }
    if (!host || !host.net) {
      const err = netError("Cannot import \"node:net\": 宿主未安装网络能力", "ERR_ACCESS_DENIED");
      queueMicrotask(() => this.emit("error", err));
      return this;
    }
    this._closed = false;
    this._listening = true;
    const address = hostPort(hostname, port);
    Promise.resolve(this._listenAsync(address)).then(
      (id) => {
        if (this._closed) {
          try { host.net.closeListener(id); } catch (_) {}
          return;
        }
        this._handle = id;
        try {
          const info = JSON.parse(host.net.listenerAddr(id));
          const parsed = splitAddr(info.address);
          this._addr = parsed || { address: info.address, port: port };
        } catch (_) {
          this._addr = { address: hostname, port: port };
        }
        this._addr = this._addr || { address: hostname, port: port };
        this.emit("listening");
        if (typeof callback === "function") callback();
        this._acceptLoop();
      },
      (err) => {
        this._listening = false;
        const cause = err && err.message ? err.message : String(err);
        const code = /ERR_ACCESS_DENIED|拒绝/.test(cause) ? "ERR_ACCESS_DENIED" : "EADDRINUSE";
        this.emit("error", netError("监听 " + address + " 失败: " + cause, code));
      },
    );
    return this;
  }

  _listenAsync(address) {
    if (typeof host.net.listenAsync === "function") return host.net.listenAsync(address);
    return Promise.reject(netError("宿主未安装监听能力", "ERR_ACCESS_DENIED"));
  }

  _acceptLoop() {
    if (!this._listening || this._accepting || this._closed) return;
    this._accepting = true;
    const next = () => {
      if (!this._listening || this._closed || this._handle === 0) {
        this._accepting = false;
        return;
      }
      Promise.resolve(host.net.acceptAsync(this._handle)).then(
        (raw) => {
          this._accepting = false;
          if (this._closed) return;
          let info;
          try {
            info = JSON.parse(raw);
          } catch (err) {
            this.emit("error", err);
            this._acceptLoop();
            return;
          }
          const sock = new Socket();
          sock._adopt(info);
          this.connections += 1;
          sock.once("close", () => { this.connections = Math.max(0, this.connections - 1); });
          if (this.maxConnections > 0 && this.connections > this.maxConnections) {
            sock.destroy();
            this._acceptLoop();
            return;
          }
          this.emit("connection", sock);
          this._acceptLoop();
        },
        (err) => {
          this._accepting = false;
          if (!this._listening || this._closed) return;
          if (this.listenerCount("error") > 0) this.emit("error", err);
          this.close();
        },
      );
    };
    next();
  }

  address() {
    return this._addr;
  }

  getConnections(callback) {
    if (typeof callback === "function") {
      queueMicrotask(() => callback(null, this.connections));
      return undefined;
    }
    return Promise.resolve(this.connections);
  }

  close(callback) {
    if (this._closed) {
      if (typeof callback === "function") queueMicrotask(callback);
      return this;
    }
    this._closed = true;
    this._listening = false;
    if (this._handle !== 0) {
      try { host.net.closeListener(this._handle); } catch (_) {}
      this._handle = 0;
    }
    if (typeof callback === "function") this.once("close", callback);
    this.emit("close");
    return this;
  }

  ref() {
    return this;
  }

  unref() {
    return this;
  }
}

export const Stream = Socket;

export default {
  isIP,
  isIPv4,
  isIPv6,
  SocketAddress,
  Socket,
  connect,
  createConnection,
  createServer,
  Server,
  Stream,
};
