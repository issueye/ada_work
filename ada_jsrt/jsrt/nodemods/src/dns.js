// node:dns —— 解析。
//
// 回调形态走宿主的异步解析（dnsLookupAsync → Promise），与 Node 一样在
// 事件循环上结算；省略回调时保留同步返回（本运行时的便利扩展，Node 没有），
// 供 dns.promises 之外的旧调用兼容。
const host = globalThis.__adaHost;

function ensure() {
  if (!host || typeof host.dnsLookup !== "function") {
    const err = new Error("Cannot import \"node:dns\": 宿主未安装解析能力");
    err.code = "ERR_ACCESS_DENIED";
    throw err;
  }
}

function dnsError(op, hostname, cause, code) {
  const err = new Error("dns " + op + " " + hostname + " 失败" + (cause && cause.message ? ": " + cause.message : ""));
  err.code = code || (op === "lookup" ? "ENOTFOUND" : "EDNS");
  err.hostname = hostname;
  err.syscall = op;
  return err;
}

function query(op, hostname, rrtype) {
  ensure();
  if (typeof hostname !== "string" || hostname.trim() === "") {
    const err = new TypeError("dns 域名必须是非空字符串");
    err.code = "ERR_INVALID_ARG_TYPE";
    throw err;
  }
  try {
    return JSON.parse(host.dnsLookup(op, hostname, rrtype || ""));
  } catch (cause) {
    throw dnsError(op, hostname, cause);
  }
}

function queryAsync(op, hostname, rrtype) {
  ensure();
  if (typeof hostname !== "string" || hostname.trim() === "") {
    const err = new TypeError("dns 域名必须是非空字符串");
    err.code = "ERR_INVALID_ARG_TYPE";
    return Promise.reject(err);
  }
  if (typeof host.dnsLookupAsync === "function") {
    return Promise.resolve(host.dnsLookupAsync(op, hostname, rrtype || "")).then(
      function (raw) {
        return JSON.parse(raw);
      },
      function (cause) {
        throw dnsError(op, hostname, cause);
      },
    );
  }
  try {
    return Promise.resolve(JSON.parse(host.dnsLookup(op, hostname, rrtype || "")));
  } catch (cause) {
    return Promise.reject(dnsError(op, hostname, cause));
  }
}

// normalizeLookupOptions 接受 Node 的三种写法:回调、数字族、对象。
function normalizeLookupOptions(options) {
  if (typeof options === "function") return { options: {}, callback: options };
  if (typeof options === "number") return { options: { family: options }, callback: undefined };
  return { options: options || {}, callback: undefined };
}

function lookup(hostname, options, callback) {
  const normalized = normalizeLookupOptions(options);
  const opts = normalized.options;
  const cb = typeof callback === "function" ? callback : normalized.callback;
  const op = opts.all ? "lookupAll" : "lookup";
  const family = String(opts.family || 0);
  if (cb) {
    queryAsync(op, hostname, family).then(
      function (result) {
        if (opts.all) cb(null, result);
        else cb(null, result.address, result.family);
      },
      function (err) {
        cb(err);
      },
    );
    return undefined;
  }
  const result = query(op, hostname, family);
  return result;
}

function lookupSync(hostname, options) {
  const opts = typeof options === "number" ? { family: options } : options || {};
  return opts.all
    ? query("lookupAll", hostname, String(opts.family || 0))
    : query("lookup", hostname, String(opts.family || 0));
}

// resolveWith 生成 resolve / resolve4 这类函数:有回调走异步解析,没有就直接返回。
function resolveWith(op, rrtype) {
  return function resolveOne(hostname, type, callback) {
    let recordType = rrtype;
    let cb = callback;
    if (typeof type === "function") cb = type;
    else if (typeof type === "string" && type !== "") recordType = type;
    else if (type && typeof type === "object") cb = typeof callback === "function" ? callback : undefined;
    if (cb) {
      queryAsync(op, hostname, recordType || "").then(
        function (records) {
          cb(null, records);
        },
        function (err) {
          cb(err);
        },
      );
      return undefined;
    }
    return query(op, hostname, recordType || "");
  };
}

function reverse(ip, callback) {
  if (callback) {
    queryAsync("reverse", ip, "").then(
      function (names) {
        callback(null, names);
      },
      function (err) {
        callback(err);
      },
    );
    return undefined;
  }
  return query("reverse", ip, "");
}

// 自定义 DNS 服务器需要 c-ares,宿主只使用系统解析器。
function setServers() {
  const err = new Error("dns.setServers 不提供:宿主只使用系统解析器");
  err.code = "ERR_NOT_SUPPORTED";
  throw err;
}

function getServers() {
  return [];
}

// Resolver 与模块级函数同源;自定义服务器同样不支持。
class Resolver {
  constructor(options) {
    this.options = options || {};
  }
  lookup(hostname, options, callback) {
    return lookup(hostname, options, callback);
  }
  resolve(hostname, rrtype, callback) {
    return resolve(hostname, rrtype, callback);
  }
  resolve4(hostname, options, callback) {
    return resolve4(hostname, options, callback);
  }
  resolve6(hostname, options, callback) {
    return resolve6(hostname, options, callback);
  }
  resolveCname(hostname, callback) {
    return resolveCname(hostname, callback);
  }
  resolveMx(hostname, callback) {
    return resolveMx(hostname, callback);
  }
  resolveNs(hostname, callback) {
    return resolveNs(hostname, callback);
  }
  resolveTxt(hostname, callback) {
    return resolveTxt(hostname, callback);
  }
  resolveSrv(hostname, callback) {
    return resolveSrv(hostname, callback);
  }
  reverse(ip, callback) {
    return reverse(ip, callback);
  }
  setServers() {
    return setServers();
  }
  getServers() {
    return getServers();
  }
}

const resolve = resolveWith("resolveCname", "");

// Node 的 resolve(hostname, rrtype, cb):按类型分派。
function resolveAny(hostname, rrtype, callback) {
  let type = rrtype;
  let cb = callback;
  if (typeof type === "function") {
    cb = type;
    type = "A";
  }
  const key = String(type || "A").toUpperCase();
  const map = {
    A: () => query("resolve4", hostname, "4"),
    AAAA: () => query("resolve6", hostname, "6"),
    CNAME: () => query("resolveCname", hostname, ""),
    MX: () => query("resolveMx", hostname, ""),
    NS: () => query("resolveNs", hostname, ""),
    TXT: () => query("resolveTxt", hostname, ""),
    SRV: () => query("resolveSrv", hostname, ""),
    PTR: () => query("reverse", hostname, ""),
  };
  const asyncMap = {
    A: () => queryAsync("resolve4", hostname, "4"),
    AAAA: () => queryAsync("resolve6", hostname, "6"),
    CNAME: () => queryAsync("resolveCname", hostname, ""),
    MX: () => queryAsync("resolveMx", hostname, ""),
    NS: () => queryAsync("resolveNs", hostname, ""),
    TXT: () => queryAsync("resolveTxt", hostname, ""),
    SRV: () => queryAsync("resolveSrv", hostname, ""),
    PTR: () => queryAsync("reverse", hostname, ""),
  };
  const unsupported = () => {
    const err = new Error("dns.resolve 不支持的类型: " + type);
    err.code = "ERR_INVALID_ARG_VALUE";
    return err;
  };
  if (cb) {
    const runAsync = asyncMap[key];
    if (!runAsync) {
      cb(unsupported());
      return undefined;
    }
    runAsync().then(
      function (records) {
        cb(null, records);
      },
      function (err) {
        cb(err);
      },
    );
    return undefined;
  }
  const run = map[key];
  if (!run) throw unsupported();
  return run();
}

const resolve4 = resolveWith("resolve4", "4");
const resolve6 = resolveWith("resolve6", "6");
const resolveCname = resolveWith("resolveCname", "");
const resolveMx = resolveWith("resolveMx", "");
const resolveNs = resolveWith("resolveNs", "");
const resolveTxt = resolveWith("resolveTxt", "");
const resolveSrv = resolveWith("resolveSrv", "");
const resolvePtr = resolveWith("reverse", "");

export {
  lookup,
  lookupSync,
  resolveAny as resolve,
  resolve4,
  resolve6,
  resolveCname,
  resolveMx,
  resolveNs,
  resolveTxt,
  resolveSrv,
  resolvePtr,
  reverse,
  setServers,
  getServers,
  Resolver,
};

export default {
  lookup,
  lookupSync,
  resolve: resolveAny,
  resolve4,
  resolve6,
  resolveCname,
  resolveMx,
  resolveNs,
  resolveTxt,
  resolveSrv,
  resolvePtr,
  reverse,
  setServers,
  getServers,
  Resolver,
  ADDRCONFIG: 32,
  V4MAPPED: 8,
  ALL: 16,
  NODATA: "ENODATA",
  FORMERR: "EFORMERR",
  SERVFAIL: "ESERVFAIL",
  NOTFOUND: "ENOTFOUND",
  NOTIMP: "ENOTIMP",
  REFUSED: "EREFUSED",
  BADQUERY: "EBADQUERY",
  BADNAME: "EBADNAME",
  BADFAMILY: "EBADFAMILY",
  BADRESP: "EBADRESP",
  CONNREFUSED: "ECONNREFUSED",
  TIMEOUT: "ETIMEOUT",
  EOF: "EOF",
  FILE: "EFILE",
  NOMEM: "ENOMEM",
  DESTRUCTION: "EDESTRUCTION",
  BADSTR: "EBADSTR",
  BADFLAGS: "EBADFLAGS",
  NONAME: "ENONAME",
  BADHINTS: "EBADHINTS",
  NOTINITIALIZED: "ENOTINITIALIZED",
  LOADIPHLPAPI: "ELOADIPHLPAPI",
  ADDRGETNETWORKPARAMS: "EADDRGETNETWORKPARAMS",
  CANCELLED: "ECANCELLED",
};
