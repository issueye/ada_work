// node:tls —— TLS 客户端（宿主实现握手，JS 只做 Node 形状）。
//
// 连接复用 node:net 的 Socket：不同之处是 _dial 走宿主 tlsDialAsync，
// 并在 connect 之后补一个 secureConnect 事件。服务端 createServer 仍不提供。
import { Socket, Server as NetServer } from "node:net";

const host = globalThis.__adaHost;

function tlsError(message, code) {
  const err = new Error(message);
  err.code = code || "ERR_TLS";
  return err;
}

function normalizeALPN(value) {
  if (value === undefined || value === null) return [];
  if (Array.isArray(value)) return value.map(String);
  return [String(value)];
}

function normalizeArgs(args) {
  if (args[0] && typeof args[0] === "object") {
    return {
      options: Object.assign({}, args[0]),
      callback: typeof args[1] === "function" ? args[1] : undefined,
    };
  }
  const port = Number(args[0]);
  let hostname = "localhost";
  let options = {};
  let callback;
  let i = 1;
  if (typeof args[i] === "string") {
    hostname = args[i];
    i += 1;
  }
  if (args[i] && typeof args[i] === "object") {
    options = Object.assign({}, args[i]);
    i += 1;
  }
  if (typeof args[i] === "function") callback = args[i];
  options.port = port;
  options.host = hostname;
  return { options, callback };
}

export class TLSSocket extends Socket {
  constructor(options) {
    super(options || {});
    this._tls = options || {};
    this.encrypted = true;
    this.authorized = this._tls.rejectUnauthorized !== false;
    this.alpnProtocol = false;
  }

  _dial(address, timeout) {
    if (!host || !host.net || typeof host.net.tlsDialAsync !== "function") {
      return Promise.reject(tlsError("Cannot import \"node:tls\": 宿主未安装 TLS 能力", "ERR_ACCESS_DENIED"));
    }
    const cfg = {
      serverName: this._tls.servername || this._tls.host || "",
      insecure: this._tls.rejectUnauthorized === false,
      alpn: normalizeALPN(this._tls.ALPNProtocols),
    };
    return host.net.tlsDialAsync(address, timeout, JSON.stringify(cfg));
  }

  connect(...args) {
    const normalized = normalizeArgs(args);
    if (typeof normalized.callback === "function") {
      this.once("secureConnect", normalized.callback);
    }
    super.connect(normalized.options);
    this.once("connect", () => {
      this.emit("secureConnect");
    });
    return this;
  }

  setServername(name) {
    this._tls.servername = String(name);
    return this;
  }

  getServername() {
    return this._tls.servername || "";
  }

  getPeerCertificate() {
    return {};
  }

  getProtocol() {
    return null;
  }
}

export function connect(...args) {
  const normalized = normalizeArgs(args);
  const socket = new TLSSocket(normalized.options);
  return socket.connect(normalized.options, normalized.callback);
}

export class Server extends NetServer {
  constructor(options, connectionListener) {
    super(options, connectionListener);
    this._tlsOptions = options || {};
  }

  _listenAsync(address) {
    if (!host || !host.net || typeof host.net.tlsListenAsync !== "function") {
      return Promise.reject(tlsError("Cannot import \"node:tls\": 宿主未安装 TLS 监听能力", "ERR_ACCESS_DENIED"));
    }
    const cfg = {
      serverName: this._tlsOptions.servername || "",
      insecure: this._tlsOptions.rejectUnauthorized === false,
      alpn: normalizeALPN(this._tlsOptions.ALPNProtocols),
    };
    return host.net.tlsListenAsync(address, JSON.stringify(cfg));
  }
}

export function createServer(options, connectionListener) {
  if (typeof options === "function") {
    connectionListener = options;
    options = {};
  }
  if (globalThis.__adaResident !== true) {
    throw tlsError("tls.createServer 需要 LoopResident:服务端依赖常驻事件循环", "ERR_NOT_SUPPORTED");
  }
  return new Server(options, connectionListener);
}

export const DEFAULT_MIN_VERSION = "TLSv1.2";
export const DEFAULT_MAX_VERSION = "TLSv1.3";

export function getCiphers() {
  return [];
}

export const rootCertificates = [];

export default {
  TLSSocket,
  Server,
  connect,
  createServer,
  DEFAULT_MIN_VERSION,
  DEFAULT_MAX_VERSION,
  getCiphers,
  rootCertificates,
};
