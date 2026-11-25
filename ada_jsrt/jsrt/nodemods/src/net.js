// node:net —— 同步 TCP 客户端。
//
// 与 Node 的差别集中在「没有事件循环」这一件事上:
//   - connect() 同步完成,成功后才返回,失败抛错;
//   - 没有自动的 'data' 事件,读取要用 socket.read(n)(本运行时唯一的扩展);
//   - createServer / listen 不提供:接收连接必须阻塞等待。
// 其余(Socket 的 write/end/destroy、isIP 系列、SocketAddress)保持 Node 形状。
import { Buffer } from "node:buffer";
import { EventEmitter } from "node:events";

const host = globalThis.__adaHost;

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

// Socket 只做同步 I/O:connect 返回时连接已建立,read 返回时数据已到手。
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
  }

  connect(...args) {
    const { options, callback } = normalizeConnectArgs(args);
    if (typeof callback === "function") this.once("connect", callback);
    const port = Number(options.port);
    if (!Number.isFinite(port) || port <= 0) throw netError("connect 需要合法端口", "ERR_SOCKET_BAD_PORT");
    if (!host || typeof host.net.dial !== "function") {
      throw netError("Cannot import \"node:net\": 宿主未安装网络能力", "ERR_ACCESS_DENIED");
    }
    if (typeof options.timeout === "number") this.timeout = options.timeout;
    let id;
    try {
      id = host.net.dial(hostPort(options.host, port), this.timeout);
    } catch (err) {
      const failure = netError("TCP 连接 " + hostPort(options.host, port) + " 失败" + (err && err.message ? ": " + err.message : ""), "ECONNREFUSED");
      if (this.listenerCount("error") === 0) throw failure;
      this.emit("error", failure);
      return this;
    }
    this._handle = id;
    this.readyState = "open";
    this.connecting = false;
    this.pending = false;
    const info = JSON.parse(host.net.addr(id));
    this._applyAddr(info);
    this.emit("connect");
    this.emit("ready");
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

  setEncoding(encoding) {
    this._encoding = encoding || "utf8";
    return this;
  }

  setTimeout(ms, callback) {
    this.timeout = Number(ms) || 0;
    if (typeof callback === "function") this.once("timeout", callback);
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

  write(data, encoding, callback) {
    if (this.destroyed || this._handle === 0) throw netError("socket 未连接或已关闭", "ERR_SOCKET_CLOSED");
    const buf = Buffer.isBuffer(data) ? data : Buffer.from(String(data), typeof encoding === "string" ? encoding : "utf8");
    host.net.write(this._handle, buf.toString("base64"));
    this.bytesWritten += buf.length;
    if (typeof encoding === "function") encoding();
    else if (typeof callback === "function") callback();
    return true;
  }

  // read 是本运行时的扩展:同步读最多 n 字节。
  // 返回 Buffer(或设置 encoding 后的字符串);EOF 返回 null 并触发 end。
  // 超时返回空 Buffer,由调用方决定重试。
  read(n) {
    if (this.destroyed || this._handle === 0) return null;
    const want = Number(n) > 0 ? Number(n) : 4096;
    const result = JSON.parse(host.net.read(this._handle, want));
    if (result.eof) {
      this.emit("end");
      this.destroy();
      return null;
    }
    const buf = Buffer.from(result.data || "", "base64");
    this.bytesRead += buf.length;
    if (buf.length === 0) return this._encoding ? "" : Buffer.alloc(0);
    return this._encoding ? buf.toString(this._encoding) : buf;
  }

  address() {
    return { address: this.localAddress, family: this.remoteFamily, port: this.localPort };
  }

  end(data, encoding) {
    if (data !== undefined && data !== null) this.write(data, encoding);
    this.destroy();
    return this;
  }

  destroy(err) {
    if (this.destroyed) return this;
    this.destroyed = true;
    this.readyState = "closed";
    if (this._handle !== 0) {
      try {
        host.net.close(this._handle);
      } catch (_) {
        // 关闭失败没有补救动作:句柄已经从运行时移除。
      }
      this._handle = 0;
    }
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

export function createServer() {
  throw netError("net.createServer 不提供:同步运行时无法接收连接", "ERR_NOT_SUPPORTED");
}

export class Server extends EventEmitter {
  constructor() {
    super();
    throw netError("net.Server 不提供:同步运行时无法接收连接", "ERR_NOT_SUPPORTED");
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
