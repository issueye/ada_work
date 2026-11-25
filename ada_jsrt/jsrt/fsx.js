// node:fs 的补齐层:补上 moejs 的 node 包没有实现、但在 Node 里常用的元数据类 API。
//
// 三种形态都在这里:
//   - xxxSync   同步;
//   - xxx(cb)   Node 的回调形态(错误优先);
//   - promises.xxx  Promise 形态(也被 node:fs/promises 的包装模块复用)。
//
// 为什么形态在 JS、操作在 Go:形态是纯编排;真正的文件操作必须由宿主用 os.Root 做,
// 路径才不会越出工作区(与 moejs 自带的 node:fs 同一条边界),也才能复用写权限开关。
// 这一层只负责把宿主的结果包成 Node 的形状与错误码。
import { Buffer } from "node:buffer";
import { Readable, Writable } from "node:stream";

const host = globalThis.__adaHost;

// unwrap 解宿主的结果信封:{ok,value} 或 {ok:false,code,message,path}。
function unwrap(raw, op) {
  const res = JSON.parse(raw);
  if (res.ok) return res.value;
  const err = new Error(res.message);
  err.code = res.code;
  err.syscall = "fs." + op;
  if (res.path !== undefined) err.path = res.path;
  throw err;
}

function sync(op, args) {
  return unwrap(host.fsx.call(op, JSON.stringify(args)), op);
}

function asyncOp(op, args) {
  return Promise.resolve(host.fsx.callAsync(op, JSON.stringify(args))).then(function (raw) {
    return unwrap(raw, op);
  });
}

function callback(op, args, cb) {
  if (typeof cb !== "function") throw new TypeError("callback 必须是函数");
  asyncOp(op, args).then(
    function (v) {
      cb(null, v);
    },
    function (e) {
      cb(e);
    },
  );
}

// pathOf 只挡最明显的坏参数:详细的路径约束在宿主侧(os.Root + 工作区检查)。
function pathOf(v, what) {
  if (typeof v !== "string" || v === "") throw new TypeError(what + " 必须是非空字符串");
  if (v.indexOf("\u0000") >= 0) throw new TypeError(what + " 不能包含 NUL");
  return v;
}

function modeOf(v) {
  const n = Number(v);
  if (!isFinite(n) || n < 0) throw new TypeError("mode 必须是数字");
  return Math.floor(n);
}

function intOf(v, what) {
  const n = Number(v);
  if (!isFinite(n)) throw new TypeError(what + " 必须是数字");
  return Math.floor(n);
}

// secondsOf 把 Node 接受的时间形态(秒数 / Date)统一成秒数。
function secondsOf(v, what) {
  if (v instanceof Date) return v.getTime() / 1000;
  const n = Number(v);
  if (!isFinite(n)) throw new TypeError(what + " 必须是数字或 Date");
  return n;
}

// —— mkdtemp ——

export function mkdtempSync(prefix) {
  return sync("mkdtemp", [pathOf(prefix, "prefix")]);
}

export function mkdtemp(prefix, options, cb) {
  if (typeof options === "function") {
    cb = options;
  }
  callback("mkdtemp", [pathOf(prefix, "prefix")], cb);
}

// —— symlink ——

export function symlinkSync(target, path, type) {
  return sync("symlink", [pathOf(target, "target"), pathOf(path, "path"), type === undefined ? "" : String(type)]);
}

export function symlink(target, path, type, cb) {
  if (typeof type === "function") {
    cb = type;
    type = undefined;
  }
  callback("symlink", [pathOf(target, "target"), pathOf(path, "path"), type === undefined ? "" : String(type)], cb);
}

// —— link ——

export function linkSync(existingPath, newPath) {
  return sync("link", [pathOf(existingPath, "existingPath"), pathOf(newPath, "newPath")]);
}

export function link(existingPath, newPath, cb) {
  callback("link", [pathOf(existingPath, "existingPath"), pathOf(newPath, "newPath")], cb);
}

// —— chmod ——

export function chmodSync(path, mode) {
  return sync("chmod", [pathOf(path, "path"), modeOf(mode)]);
}

export function chmod(path, mode, cb) {
  callback("chmod", [pathOf(path, "path"), modeOf(mode)], cb);
}

// —— chown ——

export function chownSync(path, uid, gid) {
  return sync("chown", [pathOf(path, "path"), intOf(uid, "uid"), intOf(gid, "gid")]);
}

export function chown(path, uid, gid, cb) {
  callback("chown", [pathOf(path, "path"), intOf(uid, "uid"), intOf(gid, "gid")], cb);
}

// —— truncate ——

export function truncateSync(path, len) {
  return sync("truncate", [pathOf(path, "path"), len === undefined ? 0 : intOf(len, "len")]);
}

export function truncate(path, len, cb) {
  if (typeof len === "function") {
    cb = len;
    len = 0;
  }
  callback("truncate", [pathOf(path, "path"), len === undefined ? 0 : intOf(len, "len")], cb);
}

// —— utimes ——

export function utimesSync(path, atime, mtime) {
  return sync("utimes", [pathOf(path, "path"), secondsOf(atime, "atime"), secondsOf(mtime, "mtime")]);
}

export function utimes(path, atime, mtime, cb) {
  callback("utimes", [pathOf(path, "path"), secondsOf(atime, "atime"), secondsOf(mtime, "mtime")], cb);
}

// —— 文件描述符族 ——
//
// 句柄跨调用存活,由宿主(platform/jsrt/nodefsfd.go)的 fd 表管理;这里只做
// Node 的调用形状:fd 是数字,Buffer 在调用方原地填充/读取。

function fdOf(value) {
  const n = Number(value);
  if (!Number.isInteger(n) || n < 0) throw new TypeError("fd 必须是非负整数");
  return n;
}

function flagsOf(value) {
  return value === undefined || value === null ? "r" : String(value);
}

function posOf(value) {
  if (value === undefined || value === null) return -1;
  const n = Number(value);
  return Number.isInteger(n) ? n : -1;
}

function bufferOf(value, what) {
  if (value instanceof Uint8Array) return Buffer.from(value);
  throw new TypeError(what + " 必须是 Buffer / Uint8Array");
}

export function openSync(path, flags, mode) {
  return sync("open", [pathOf(path, "path"), flagsOf(flags), mode === undefined ? 438 : modeOf(mode)]);
}

export function open(path, flags, mode, cb) {
  if (typeof flags === "function") {
    cb = flags;
    flags = "r";
    mode = undefined;
  } else if (typeof mode === "function") {
    cb = mode;
    mode = undefined;
  }
  callback("open", [pathOf(path, "path"), flagsOf(flags), mode === undefined ? 438 : modeOf(mode)], cb);
}

export function closeSync(fd) {
  return sync("close", [fdOf(fd)]);
}

export function close(fd, cb) {
  callback("close", [fdOf(fd)], cb);
}

export function readSync(fd, buffer, offset, length, position) {
  const target = bufferOf(buffer, "buffer");
  const off = offset === undefined ? 0 : Number(offset);
  const len = length === undefined ? target.length - off : Number(length);
  const res = sync("read", [fdOf(fd), len, posOf(position)]);
  const bytes = Buffer.from(res.data, "base64");
  target.set(bytes, off);
  buffer.set(bytes, off);
  return res.bytesRead;
}

export function read(fd, buffer, offset, length, position, cb) {
  // Node 允许省略 offset/length/position,回调是最后一个参数。
  if (typeof offset === "function") {
    cb = offset;
    offset = 0;
    length = undefined;
    position = undefined;
  } else if (typeof length === "function") {
    cb = length;
    length = undefined;
    position = undefined;
  } else if (typeof position === "function") {
    cb = position;
    position = undefined;
  }
  if (typeof cb !== "function") throw new TypeError("read 需要一个回调");
  const target = bufferOf(buffer, "buffer");
  const off = offset === undefined ? 0 : Number(offset);
  const len = length === undefined ? target.length - off : Number(length);
  asyncOp("read", [fdOf(fd), len, posOf(position)]).then(
    function (res) {
      const bytes = Buffer.from(res.data, "base64");
      target.set(bytes, off);
      buffer.set(bytes, off);
      cb(null, res.bytesRead, buffer);
    },
    function (err) {
      cb(err, 0, buffer);
    },
  );
}

// writeSync 支持 Node 的两种重载:(fd, buffer, offset, length, position) 与
// (fd, string, position, encoding)。
export function writeSync(fd, data, offset, length, position) {
  const id = fdOf(fd);
  if (typeof data === "string") {
    const pos = offset === undefined || offset === null ? -1 : Number(offset);
    const enc = typeof length === "string" ? length : "utf8";
    return sync("write", [id, Buffer.from(data, enc).toString("base64"), Number.isInteger(pos) ? pos : -1]);
  }
  const source = bufferOf(data, "data");
  const off = offset === undefined ? 0 : Number(offset);
  const len = length === undefined ? source.length - off : Number(length);
  const slice = Buffer.from(source.subarray(off, off + len));
  return sync("write", [id, slice.toString("base64"), posOf(position)]);
}

export function write(fd, data, offset, length, position, cb) {
  if (typeof offset === "function") {
    cb = offset;
    offset = undefined;
    length = undefined;
    position = undefined;
  } else if (typeof length === "function") {
    cb = length;
    length = undefined;
    position = undefined;
  } else if (typeof position === "function") {
    cb = position;
    position = undefined;
  }
  if (typeof cb !== "function") throw new TypeError("write 需要一个回调");
  const id = fdOf(fd);
  const isString = typeof data === "string";
  let payload;
  let bytes;
  try {
    if (isString) {
      const pos = offset === undefined || offset === null ? -1 : Number(offset);
      const enc = typeof length === "string" ? length : "utf8";
      bytes = Buffer.from(data, enc);
      payload = [id, bytes.toString("base64"), Number.isInteger(pos) ? pos : -1];
    } else {
      const source = bufferOf(data, "data");
      const off = offset === undefined ? 0 : Number(offset);
      const len = length === undefined ? source.length - off : Number(length);
      bytes = Buffer.from(source.subarray(off, off + len));
      payload = [id, bytes.toString("base64"), posOf(position)];
    }
  } catch (err) {
    cb(err, 0, data);
    return;
  }
  asyncOp("write", payload).then(
    function (written) {
      cb(null, written, data);
    },
    function (err) {
      cb(err, 0, data);
    },
  );
}

export function fsyncSync(fd) {
  return sync("fsync", [fdOf(fd)]);
}

export function fsync(fd, cb) {
  callback("fsync", [fdOf(fd)], cb);
}

export function ftruncateSync(fd, len) {
  return sync("ftruncate", [fdOf(fd), len === undefined ? 0 : intOf(len, "len")]);
}

export function ftruncate(fd, len, cb) {
  if (typeof len === "function") {
    cb = len;
    len = 0;
  }
  callback("ftruncate", [fdOf(fd), len === undefined ? 0 : intOf(len, "len")], cb);
}

export function fstatSync(fd) {
  return sync("fstat", [fdOf(fd)]);
}

export function fstat(fd, cb) {
  callback("fstat", [fdOf(fd)], cb);
}

// —— fd 级元数据与向量 I/O ——

export function fchmodSync(fd, mode) {
  return sync("fchmod", [fdOf(fd), modeOf(mode)]);
}

export function fchmod(fd, mode, cb) {
  callback("fchmod", [fdOf(fd), modeOf(mode)], cb);
}

export function fchownSync(fd, uid, gid) {
  return sync("fchown", [fdOf(fd), intOf(uid, "uid"), intOf(gid, "gid")]);
}

export function fchown(fd, uid, gid, cb) {
  callback("fchown", [fdOf(fd), intOf(uid, "uid"), intOf(gid, "gid")], cb);
}

export function futimesSync(fd, atime, mtime) {
  return sync("futimes", [fdOf(fd), secondsOf(atime, "atime"), secondsOf(mtime, "mtime")]);
}

export function futimes(fd, atime, mtime, cb) {
  callback("futimes", [fdOf(fd), secondsOf(atime, "atime"), secondsOf(mtime, "mtime")], cb);
}

// buffersOf 只校验、不拷贝:readv 必须把数据写进调用方**原来**的 Buffer,
// 复制一份再填会让调用方永远看到零字节。
function buffersOf(list) {
  if (!Array.isArray(list)) throw new TypeError("buffers 必须是数组");
  return list.map(function (item) {
    if (item instanceof Uint8Array) return item;
    throw new TypeError("buffers[i] 必须是 Buffer / Uint8Array");
  });
}

function concatBuffers(bufs) {
  return Buffer.concat(bufs.map(function (buf) {
    return Buffer.isBuffer(buf) ? buf : Buffer.from(buf);
  }));
}

// readvAsync 依次把数据读进每个 buffer,读不满就停(与 Node 的 readv 相同)。
function readvAsync(id, bufs, startPos) {
  return new Promise(function (resolve, reject) {
    let total = 0;
    let pos = startPos;
    const step = function (index) {
      if (index >= bufs.length) {
        resolve(total);
        return;
      }
      const buf = bufs[index];
      if (buf.length === 0) {
        step(index + 1);
        return;
      }
      asyncOp("read", [id, buf.length, pos]).then(function (res) {
        buf.set(Buffer.from(res.data, "base64"), 0);
        total += res.bytesRead;
        if (pos >= 0) pos += res.bytesRead;
        if (res.bytesRead < buf.length) {
          resolve(total);
          return;
        }
        step(index + 1);
      }, reject);
    };
    step(0);
  });
}

export function readvSync(fd, buffers, position) {
  const bufs = buffersOf(buffers);
  const id = fdOf(fd);
  let total = 0;
  let pos = posOf(position);
  for (const buf of bufs) {
    if (buf.length === 0) continue;
    const res = sync("read", [id, buf.length, pos]);
    buf.set(Buffer.from(res.data, "base64"), 0);
    total += res.bytesRead;
    if (pos >= 0) pos += res.bytesRead;
    if (res.bytesRead < buf.length) break;
  }
  return total;
}

export function readv(fd, buffers, position, cb) {
  if (typeof position === "function") {
    cb = position;
    position = -1;
  }
  if (typeof cb !== "function") throw new TypeError("readv 需要一个回调");
  const bufs = buffersOf(buffers);
  readvAsync(fdOf(fd), bufs, posOf(position)).then(
    function (n) {
      cb(null, n, buffers);
    },
    function (err) {
      cb(err, 0, buffers);
    },
  );
}

export function writevSync(fd, buffers, position) {
  const all = concatBuffers(buffersOf(buffers));
  return sync("write", [fdOf(fd), all.toString("base64"), posOf(position)]);
}

export function writev(fd, buffers, position, cb) {
  if (typeof position === "function") {
    cb = position;
    position = -1;
  }
  if (typeof cb !== "function") throw new TypeError("writev 需要一个回调");
  let all;
  try {
    all = Buffer.concat(buffersOf(buffers));
  } catch (err) {
    cb(err, 0, buffers);
    return;
  }
  asyncOp("write", [fdOf(fd), all.toString("base64"), posOf(position)]).then(
    function (n) {
      cb(null, n, buffers);
    },
    function (err) {
      cb(err, 0, buffers);
    },
  );
}

// —— 文件流 ——
//
// 两个流都建立在 fd 族之上:读按块 await,写按块 await,结束时关句柄。
// 注意:Readable.from 的回压只在缓冲层面(没有跨块暂停),大文件会被整段读进内存;
// 需要分块消费时用 for await 逐块处理,而不是一次 .on("data") 全收。

// readStreamFrom 用「打开句柄」的闭包构造可读流:
// path 版自己开/关;FileHandle 版复用已有 fd、结束时**不**关(与 Node 一致)。
function readStreamFrom(openFd, options, ownsFD) {
  const opts = options || {};
  const chunkSize = opts.highWaterMark === undefined ? 64 * 1024 : Number(opts.highWaterMark);
  const start = opts.start === undefined ? 0 : Number(opts.start);
  const end = opts.end === undefined ? -1 : Number(opts.end);
  let fd = -1;
  let pos = start;

  const closeQuiet = function () {
    if (fd >= 0 && ownsFD) {
      try {
        closeSync(fd);
      } catch (_) {}
    }
    fd = -1;
  };
  const iterable = {
    next: function () {
      if (fd < 0) {
        try {
          fd = openFd();
        } catch (err) {
          return Promise.reject(err);
        }
      }
      let want = chunkSize;
      if (end >= 0) {
        const left = end - pos + 1;
        if (left <= 0) {
          closeQuiet();
          return Promise.resolve({ value: undefined, done: true });
        }
        if (left < want) want = left;
      }
      if (want <= 0) {
        closeQuiet();
        return Promise.resolve({ value: undefined, done: true });
      }
      return asyncOp("read", [fd, want, pos]).then(
        function (res) {
          if (res.bytesRead <= 0) {
            closeQuiet();
            return { value: undefined, done: true };
          }
          pos += res.bytesRead;
          return { value: Buffer.from(res.data, "base64"), done: false };
        },
        function (err) {
          closeQuiet();
          throw err;
        },
      );
    },
    return: function () {
      closeQuiet();
      return Promise.resolve({ value: undefined, done: true });
    },
  };
  iterable[Symbol.asyncIterator] = function () {
    return iterable;
  };
  const stream = Readable.from(iterable, { highWaterMark: chunkSize });
  stream.on("close", closeQuiet);
  stream.on("error", closeQuiet);
  return stream;
}

export function createReadStream(path, options) {
  return readStreamFrom(function () {
    return openSync(path, "r");
  }, options, true);
}

// writeStreamFrom 同理:path 版自己开关,FileHandle 版复用句柄。
function writeStreamFrom(openFd, options, ownsFD) {
  const opts = options || {};
  let fd = -1;
  const closeQuiet = function () {
    if (fd >= 0 && ownsFD) {
      try {
        closeSync(fd);
      } catch (_) {}
    }
    fd = -1;
  };
  const stream = new Writable({ highWaterMark: opts.highWaterMark });
  stream._write = function (chunk, enc, cb) {
    try {
      if (fd < 0) fd = openFd();
    } catch (err) {
      cb(err);
      return;
    }
    let bytes;
    try {
      bytes = typeof chunk === "string" ? Buffer.from(chunk, enc || "utf8") : Buffer.from(chunk);
    } catch (err) {
      cb(err);
      return;
    }
    asyncOp("write", [fd, bytes.toString("base64"), -1]).then(
      function () {
        cb();
      },
      function (err) {
        cb(err);
      },
    );
  };
  stream.on("finish", closeQuiet);
  stream.on("error", closeQuiet);
  return stream;
}

export function createWriteStream(path, options) {
  const opts = options || {};
  const flags = opts.flags === undefined ? "w" : String(opts.flags);
  return writeStreamFrom(function () {
    return openSync(path, flags);
  }, options, true);
}

// promises 是给 node:fs/promises 与 fs.promises 用的 Promise 形态。
export const promises = {
  mkdtemp: function mkdtemp(prefix) {
    return asyncOp("mkdtemp", [pathOf(prefix, "prefix")]);
  },
  symlink: function symlink(target, path, type) {
    return asyncOp("symlink", [pathOf(target, "target"), pathOf(path, "path"), type === undefined ? "" : String(type)]);
  },
  link: function link(existingPath, newPath) {
    return asyncOp("link", [pathOf(existingPath, "existingPath"), pathOf(newPath, "newPath")]);
  },
  chmod: function chmod(path, mode) {
    return asyncOp("chmod", [pathOf(path, "path"), modeOf(mode)]);
  },
  chown: function chown(path, uid, gid) {
    return asyncOp("chown", [pathOf(path, "path"), intOf(uid, "uid"), intOf(gid, "gid")]);
  },
  truncate: function truncate(path, len) {
    return asyncOp("truncate", [pathOf(path, "path"), len === undefined ? 0 : intOf(len, "len")]);
  },
  utimes: function utimes(path, atime, mtime) {
    return asyncOp("utimes", [pathOf(path, "path"), secondsOf(atime, "atime"), secondsOf(mtime, "mtime")]);
  },
  // 文件描述符族的 Promise 形态。read/write 返回 Node 的 {bytesRead|bytesWritten, buffer}
  // (fsp 的形状),不做 FileHandle 包装(Node 的 fsp.open 返回 FileHandle,那是另一套 API)。
  open: function open(path, flags, mode) {
    return asyncOp("open", [pathOf(path, "path"), flagsOf(flags), mode === undefined ? 438 : modeOf(mode)]);
  },
  close: function close(fd) {
    return asyncOp("close", [fdOf(fd)]).then(function () {
      return undefined;
    });
  },
  read: function read(fd, buffer, offset, length, position) {
    const target = bufferOf(buffer, "buffer");
    const off = offset === undefined ? 0 : Number(offset);
    const len = length === undefined ? target.length - off : Number(length);
    return asyncOp("read", [fdOf(fd), len, posOf(position)]).then(function (res) {
      const bytes = Buffer.from(res.data, "base64");
      target.set(bytes, off);
      buffer.set(bytes, off);
      return { bytesRead: res.bytesRead, buffer: buffer };
    });
  },
  write: function write(fd, data, offset, length, position) {
    const id = fdOf(fd);
    if (typeof data === "string") {
      const pos = offset === undefined || offset === null ? -1 : Number(offset);
      const enc = typeof length === "string" ? length : "utf8";
      return asyncOp("write", [id, Buffer.from(data, enc).toString("base64"), Number.isInteger(pos) ? pos : -1]).then(
        function (written) {
          return { bytesWritten: written, buffer: data };
        },
      );
    }
    const source = bufferOf(data, "data");
    const off = offset === undefined ? 0 : Number(offset);
    const len = length === undefined ? source.length - off : Number(length);
    const slice = Buffer.from(source.subarray(off, off + len));
    return asyncOp("write", [id, slice.toString("base64"), posOf(position)]).then(function (written) {
      return { bytesWritten: written, buffer: data };
    });
  },
  fsync: function fsync(fd) {
    return asyncOp("fsync", [fdOf(fd)]).then(function () {
      return undefined;
    });
  },
  ftruncate: function ftruncate(fd, len) {
    return asyncOp("ftruncate", [fdOf(fd), len === undefined ? 0 : intOf(len, "len")]).then(function () {
      return undefined;
    });
  },
  fstat: function fstat(fd) {
    return asyncOp("fstat", [fdOf(fd)]);
  },
  fchmod: function fchmod(fd, mode) {
    return asyncOp("fchmod", [fdOf(fd), modeOf(mode)]).then(function () {
      return undefined;
    });
  },
  fchown: function fchown(fd, uid, gid) {
    return asyncOp("fchown", [fdOf(fd), intOf(uid, "uid"), intOf(gid, "gid")]).then(function () {
      return undefined;
    });
  },
  futimes: function futimes(fd, atime, mtime) {
    return asyncOp("futimes", [fdOf(fd), secondsOf(atime, "atime"), secondsOf(mtime, "mtime")]).then(function () {
      return undefined;
    });
  },
  readv: function readv(fd, buffers, position) {
    const bufs = buffersOf(buffers);
    return readvAsync(fdOf(fd), bufs, posOf(position)).then(function (n) {
      return { bytesRead: n, buffers: buffers };
    });
  },
  writev: function writev(fd, buffers, position) {
    const all = concatBuffers(buffersOf(buffers));
    return asyncOp("write", [fdOf(fd), all.toString("base64"), posOf(position)]).then(function (n) {
      return { bytesWritten: n, buffers: buffers };
    });
  },
};
// —— 目录 ——

function dirError() {
  const err = new Error("Dir 已关闭");
  err.code = "EBADF";
  return err;
}

class Dirent {
  constructor(name, parent, kind) {
    this.name = name;
    this.parentPath = parent;
    this.path = parent === "" ? name : parent + "/" + name;
    this._kind = kind;
  }
  isFile() {
    return this._kind === "file";
  }
  isDirectory() {
    return this._kind === "dir";
  }
  isSymbolicLink() {
    return this._kind === "symlink";
  }
  isBlockDevice() {
    return false;
  }
  isCharacterDevice() {
    return false;
  }
  isFIFO() {
    return false;
  }
  isSocket() {
    return false;
  }
}

export class Dir {
  constructor(fd, path) {
    this._fd = fd;
    this.path = path;
    this._closed = false;
    this._eof = false;
    this._buf = [];
  }

  _ensure() {
    if (this._closed) throw dirError();
  }

  // _fill 预取一批目录项;读完或已关闭时立即返回。
  _fill() {
    const self = this;
    if (this._closed || this._eof || this._buf.length > 0) return Promise.resolve();
    return asyncOp("readdir", [this._fd, 64]).then(function (res) {
      for (const item of res.entries) self._buf.push(new Dirent(item.name, self.path, item.type));
      if (res.eof) self._eof = true;
    });
  }

  read(cb) {
    const self = this;
    const p = this._fill().then(function () {
      self._ensure();
      if (self._buf.length > 0) return self._buf.shift();
      return null;
    });
    if (typeof cb === "function") {
      p.then(
        function (d) {
          cb(null, d);
        },
        function (err) {
          cb(err);
        },
      );
      return undefined;
    }
    return p;
  }

  readSync() {
    this._ensure();
    if (this._buf.length === 0 && !this._eof) {
      const res = sync("readdir", [this._fd, 64]);
      for (const item of res.entries) this._buf.push(new Dirent(item.name, this.path, item.type));
      if (res.eof) this._eof = true;
    }
    return this._buf.length > 0 ? this._buf.shift() : null;
  }

  close(cb) {
    const self = this;
    const p = Promise.resolve().then(function () {
      if (self._closed) return;
      self._closed = true;
      return asyncOp("close", [self._fd]).then(function () {
        return undefined;
      });
    });
    if (typeof cb === "function") {
      p.then(
        function () {
          cb(null);
        },
        function (err) {
          cb(err);
        },
      );
      return undefined;
    }
    return p;
  }

  closeSync() {
    if (this._closed) return;
    this._closed = true;
    closeSync(this._fd);
  }

  [Symbol.asyncIterator]() {
    const self = this;
    const iterator = {
      next: function () {
        return self.read().then(function (dirent) {
          if (dirent === null) return { value: undefined, done: true };
          return { value: dirent, done: false };
        });
      },
      return: function () {
        return self.close().then(function () {
          return { value: undefined, done: true };
        });
      },
    };
    iterator[Symbol.asyncIterator] = function () {
      return iterator;
    };
    return iterator;
  }
}

export function opendirSync(path, options) {
  const p = pathOf(path, "path");
  return new Dir(sync("opendir", [p]), p);
}

export function opendir(path, options, cb) {
  if (typeof options === "function") {
    cb = options;
  }
  const p = pathOf(path, "path");
  const promise = asyncOp("opendir", [p]).then(function (fd) {
    return new Dir(fd, p);
  });
  if (typeof cb === "function") {
    promise.then(
      function (dir) {
        cb(null, dir);
      },
      function (err) {
        cb(err);
      },
    );
    return undefined;
  }
  return promise;
}

// —— FileHandle ——
//
// fsp.open 返回它(Node 的形状);方法名与 Node 对齐,但没有 FileHandle 专属的
// chmod/chown/utimes(需要 fchmod/fchown/futimes,宿主未实现),也不提供 readLines/readv/writev。

export class FileHandle {
  constructor(fd, path) {
    this.fd = fd;
    this._path = path;
    this._closed = false;
  }

  _ensure() {
    if (this._closed) throw dirError();
  }

  async read(buffer, offset, length, position) {
    this._ensure();
    return await promises.read(this.fd, buffer, offset, length, position);
  }

  async readFile(options) {
    this._ensure();
    const st = await promises.fstat(this.fd);
    const size = Number(st.size);
    const buf = Buffer.alloc(size);
    let off = 0;
    while (off < size) {
      const res = await promises.read(this.fd, buf, off, size - off, off);
      if (!res.bytesRead) break;
      off += res.bytesRead;
    }
    const enc = typeof options === "string" ? options : options && options.encoding;
    return enc ? buf.toString(enc, 0, off) : buf.subarray(0, off);
  }

  async write(buffer, offset, length, position) {
    this._ensure();
    return await promises.write(this.fd, buffer, offset, length, position);
  }

  async writeFile(data, options) {
    this._ensure();
    const enc = options && options.encoding ? options.encoding : "utf8";
    const buf = typeof data === "string" ? Buffer.from(data, enc) : Buffer.from(data);
    await promises.ftruncate(this.fd, 0);
    let off = 0;
    while (off < buf.length) {
      const res = await promises.write(this.fd, buf, off, buf.length - off, off);
      if (!res.bytesWritten) break;
      off += res.bytesWritten;
    }
    return undefined;
  }

  async appendFile(data, options) {
    this._ensure();
    const enc = options && options.encoding ? options.encoding : "utf8";
    const buf = typeof data === "string" ? Buffer.from(data, enc) : Buffer.from(data);
    const st = await promises.fstat(this.fd);
    let off = Number(st.size);
    let written = 0;
    while (written < buf.length) {
      const res = await promises.write(this.fd, buf, written, buf.length - written, off + written);
      if (!res.bytesWritten) break;
      written += res.bytesWritten;
    }
    return undefined;
  }

  async stat() {
    this._ensure();
    return await promises.fstat(this.fd);
  }

  async sync() {
    this._ensure();
    await promises.fsync(this.fd);
  }

  async truncate(len) {
    this._ensure();
    await promises.ftruncate(this.fd, len === undefined ? 0 : Number(len));
  }

  // 句柄级 chmod/chown/utimes:走 fchmod/fchown/futimes,不经路径解析(没有 TOCTOU)。
  async chmod(mode) {
    this._ensure();
    await promises.fchmod(this.fd, mode);
  }

  async chown(uid, gid) {
    this._ensure();
    await promises.fchown(this.fd, uid, gid);
  }

  async utimes(atime, mtime) {
    this._ensure();
    await promises.futimes(this.fd, atime, mtime);
  }

  async readv(buffers, position) {
    this._ensure();
    return await promises.readv(this.fd, buffers, position);
  }

  async writev(buffers, position) {
    this._ensure();
    return await promises.writev(this.fd, buffers, position);
  }

  // readLines:按行读的异步可迭代对象(Node 18.4+ 的形状),不加载整文件。
  readLines(options) {
    this._ensure();
    return makeLinesIterator(this, options);
  }

  // readableWebStream:最小 WHATWG ReadableStream 形状(见下面的边界说明)。
  readableWebStream(options) {
    this._ensure();
    return makeWebReadableStream(this, options);
  }

  async close() {
    if (this._closed) return;
    this._closed = true;
    await promises.close(this.fd);
  }

  createReadStream(options) {
    this._ensure();
    const fd = this.fd;
    return readStreamFrom(function () {
      return fd;
    }, options, false);
  }

  createWriteStream(options) {
    this._ensure();
    const fd = this.fd;
    return writeStreamFrom(function () {
      return fd;
    }, options, false);
  }
}

// makeLinesIterator 按行读句柄:块读 + 余量拼接,不会把整文件读进内存。
function makeLinesIterator(fh, options) {
  const encoding = (options && options.encoding) || "utf8";
  const chunkSize = 64 * 1024;
  let leftover = "";
  let pos = 0;
  let finished = false;

  const readChunk = function () {
    return new Promise(function (resolve, reject) {
      const buf = Buffer.alloc(chunkSize);
      promises.read(fh.fd, buf, 0, chunkSize, pos).then(function (res) {
        if (!res.bytesRead) {
          resolve(null);
          return;
        }
        pos += res.bytesRead;
        resolve(buf.toString(encoding, 0, res.bytesRead));
      }, reject);
    });
  };

  const iterator = {
    next: function () {
      if (finished) return Promise.resolve({ value: undefined, done: true });
      const loop = function () {
        const idx = leftover.indexOf("\n");
        if (idx >= 0) {
          let line = leftover.slice(0, idx);
          leftover = leftover.slice(idx + 1);
          if (line.endsWith("\r")) line = line.slice(0, -1);
          return Promise.resolve({ value: line, done: false });
        }
        return readChunk().then(function (text) {
          if (text === null) {
            finished = true;
            if (leftover.length > 0) {
              const last = leftover;
              leftover = "";
              return { value: last, done: false };
            }
            return { value: undefined, done: true };
          }
          leftover += text;
          return loop();
        });
      };
      return loop();
    },
    return: function () {
      finished = true;
      return Promise.resolve({ value: undefined, done: true });
    },
  };
  iterator[Symbol.asyncIterator] = function () {
    return iterator;
  };
  return iterator;
}

// makeWebReadableStream 是一个最小 WHATWG ReadableStream 形状:
// getReader().read()/cancel()、[Symbol.asyncIterator];pipeTo/tee 不支持。
//
// 为什么是子集:运行时没有全局 ReadableStream(webapi 未安装),这里只提供
// 「惰性分块读文件」这一件事,不冒充完整的 Streams 标准。
function makeWebReadableStream(fh, options) {
  const chunkSize = (options && options.autoAllocateChunkSize) || 64 * 1024;
  let pos = 0;
  let done = false;

  const readOnce = function () {
    if (done) return Promise.resolve({ value: undefined, done: true });
    return new Promise(function (resolve, reject) {
      const buf = Buffer.alloc(chunkSize);
      promises.read(fh.fd, buf, 0, chunkSize, pos).then(function (res) {
        if (!res.bytesRead) {
          done = true;
          resolve({ value: undefined, done: true });
          return;
        }
        pos += res.bytesRead;
        resolve({ value: new Uint8Array(buf.subarray(0, res.bytesRead)), done: false });
      }, reject);
    });
  };

  const reader = {
    read: readOnce,
    cancel: function () {
      done = true;
      return Promise.resolve();
    },
    releaseLock: function () {},
    get closed() {
      return Promise.resolve();
    },
  };

  const stream = {
    getReader: function () {
      return reader;
    },
    cancel: function () {
      return reader.cancel();
    },
    pipeTo: function () {
      throw new Error("readableWebStream().pipeTo 未实现:运行时没有 WHATWG Streams");
    },
    tee: function () {
      throw new Error("readableWebStream().tee 未实现:运行时没有 WHATWG Streams");
    },
  };
  stream[Symbol.asyncIterator] = function () {
    const it = {
      next: readOnce,
      return: function () {
        done = true;
        return Promise.resolve({ value: undefined, done: true });
      },
    };
    it[Symbol.asyncIterator] = function () {
      return it;
    };
    return it;
  };
  return stream;
}
// promises.open 返回 FileHandle(Node 的形状);fs.open 的回调形态仍返回数字 fd。
promises.open = function open(path, flags, mode) {
  const p = pathOf(path, "path");
  return asyncOp("open", [p, flagsOf(flags), mode === undefined ? 438 : modeOf(mode)]).then(function (fd) {
    return new FileHandle(fd, p);
  });
};

promises.opendir = function opendir(path, options) {
  const p = pathOf(path, "path");
  return asyncOp("opendir", [p]).then(function (fd) {
    return new Dir(fd, p);
  });
};

// —— watch:如实拒绝 ——
//
// 文件变更通知需要「没有调用时也能把事件送进 JS」,这与 ADR 0014 的调用边界模型
// 冲突;而且宿主没有跨平台通知后端(引入 fsnotify 需要新依赖 + ADR)。
// 需要监视变更时:用 Go 插件 + 事件,或 platform/sched 的周期任务。

function watchUnsupported(api) {
  const err = new Error(api + " 不提供:文件变更通知需要后台事件循环与跨平台通知后端,见 ADR 0016");
  err.code = "ERR_NOT_SUPPORTED";
  return err;
}

export function watch() {
  throw watchUnsupported("fs.watch");
}

export function watchFile() {
  throw watchUnsupported("fs.watchFile");
}

export function unwatchFile() {
  throw watchUnsupported("fs.unwatchFile");
}