// node:fs 的包装模块:在 moejs 的 node:fs 之上补齐缺失的元数据类 API。
//
// 为什么必须包一层而不是改上游:moejs 把未实现的 50 个名字装成「调用即抛」的函数,
// 我们只在宿主侧补实现,名字必须出现在同一份导出面里(命名空间导入与默认导入都要能拿到)。
import base from "ada:nodefs";
import * as ext from "ada:fsx";

export * from "ada:nodefs";
export {
  chmod,
  chmodSync,
  chown,
  chownSync,
  close,
  closeSync,
  createReadStream,
  createWriteStream,
  Dir,
  opendir,
  opendirSync,
  read,
  readSync,
  readv,
  readvSync,
  unwatchFile,
  watch,
  watchFile,
  fchmod,
  fchmodSync,
  fchown,
  fchownSync,
  fstat,
  fstatSync,
  fsync,
  fsyncSync,
  futimes,
  futimesSync,
  ftruncate,
  ftruncateSync,
  link,
  linkSync,
  mkdtemp,
  mkdtempSync,
  open,
  openSync,
  symlink,
  symlinkSync,
  truncate,
  truncateSync,
  utimes,
  utimesSync,
  write,
  writeSync,
  writev,
  writevSync,
} from "ada:fsx";

// attach 覆盖 moejs 装上的「未实现」桩。
//
// 上游对象是普通对象(属性可写可配置),但这里仍然用 try 兜住:
// 万一将来上游改成冻结对象,导入不该因此炸掉——此时命名空间导入仍然可用。
function attach(obj, name, value) {
  if (obj === null || obj === undefined) return;
  try {
    obj[name] = value;
    if (obj[name] === value) return;
  } catch (_) {}
  try {
    Object.defineProperty(obj, name, { value: value, configurable: true, writable: true, enumerable: true });
  } catch (_) {}
}

const added = {
  chmod: ext.chmod,
  chmodSync: ext.chmodSync,
  chown: ext.chown,
  chownSync: ext.chownSync,
  close: ext.close,
  closeSync: ext.closeSync,
  createReadStream: ext.createReadStream,
  createWriteStream: ext.createWriteStream,
  fchmod: ext.fchmod,
  fchmodSync: ext.fchmodSync,
  fchown: ext.fchown,
  fchownSync: ext.fchownSync,
  fstat: ext.fstat,
  fstatSync: ext.fstatSync,
  futimes: ext.futimes,
  futimesSync: ext.futimesSync,
  fsync: ext.fsync,
  fsyncSync: ext.fsyncSync,
  ftruncate: ext.ftruncate,
  ftruncateSync: ext.ftruncateSync,
  link: ext.link,
  linkSync: ext.linkSync,
  mkdtemp: ext.mkdtemp,
  mkdtempSync: ext.mkdtempSync,
  opendir: ext.opendir,
  opendirSync: ext.opendirSync,
  Dir: ext.Dir,
  watch: ext.watch,
  watchFile: ext.watchFile,
  unwatchFile: ext.unwatchFile,
  open: ext.open,
  openSync: ext.openSync,
  read: ext.read,
  readSync: ext.readSync,
  readv: ext.readv,
  readvSync: ext.readvSync,
  symlink: ext.symlink,
  symlinkSync: ext.symlinkSync,
  truncate: ext.truncate,
  truncateSync: ext.truncateSync,
  utimes: ext.utimes,
  utimesSync: ext.utimesSync,
  write: ext.write,
  writeSync: ext.writeSync,
  writev: ext.writev,
  writevSync: ext.writevSync,
};

// 默认导入(`import fs from "node:fs"`)拿到的是上游的绑定对象,补齐的名字要挂到它上面。
for (const name of Object.keys(added)) {
  attach(base, name, added[name]);
}
// fs.promises 也补齐:同一份对象,`fs.promises.mkdtemp` 应该能用。
for (const name of Object.keys(ext.promises)) {
  attach(base.promises, name, ext.promises[name]);
}

export default base;

// —— 核心 fs 的配额包装：moejs 的 readFile/writeFile/readdir 不认 Max*，
// 这里在 JS 边界补上；fsx 路径（fd/流/opendir）已由 Go 侧强制。
const fsQuota = globalThis.__adaFSLimits || {};
const baseReadFileSync = base.readFileSync;
const baseWriteFileSync = base.writeFileSync;
const baseAppendFileSync = base.appendFileSync;
const baseReaddirSync = base.readdirSync;
const baseReadFile = base.readFile;
const baseWriteFile = base.writeFile;
const baseAppendFile = base.appendFile;
const baseReaddir = base.readdir;
const basePromisesReadFile = base.promises.readFile;
const basePromisesWriteFile = base.promises.writeFile;
const basePromisesAppendFile = base.promises.appendFile;
const basePromisesReaddir = base.promises.readdir;
const maxFileBytes = Number(fsQuota.maxFileBytes) || 0;
const maxDirEntries = Number(fsQuota.maxDirEntries) || 0;

function quotaError(code, message) {
  const err = new Error(message);
  err.code = code;
  return err;
}

function utf8Length(value) {
  let n = 0;
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c <= 0xdbff && i + 1 < value.length) { n += 4; i += 1; }
    else n += 3;
  }
  return n;
}

function dataByteLength(data) {
  if (typeof data === "string") return utf8Length(data);
  if (data && typeof data.byteLength === "number") return data.byteLength;
  if (data && typeof data.length === "number") return data.length;
  return 0;
}

function encodingOf(options) {
  if (typeof options === "string") return options;
  if (options && typeof options === "object" && typeof options.encoding === "string") return options.encoding;
  return undefined;
}

function checkFileSize(size) {
  if (maxFileBytes > 0 && size > maxFileBytes) {
    throw quotaError("EFBIG", "文件超过 MaxFileBytes(" + maxFileBytes + ")");
  }
}

function checkReadData(data, options) {
  if (maxFileBytes > 0) {
    const size = typeof data === "string" ? utf8Length(data) : data.length;
    checkFileSize(size);
  }
  return data;
}

function checkWriteData(path, data, append) {
  if (maxFileBytes <= 0) return;
  const add = dataByteLength(data);
  let size = add;
  if (append) {
    try { size = base.statSync(path).size + add; } catch (_) { size = add; }
  }
  checkFileSize(size);
}

function checkDirList(list) {
  if (maxDirEntries > 0 && list.length > maxDirEntries) {
    throw quotaError("EFBIG", "目录条目超过 MaxDirEntries(" + maxDirEntries + ")");
  }
  return list;
}

export function readFileSync(path, options) {
  return checkReadData(baseReadFileSync(path, options), options);
}

export function writeFileSync(path, data, options) {
  checkWriteData(path, data, false);
  return baseWriteFileSync(path, data, options);
}

export function appendFileSync(path, data, options) {
  checkWriteData(path, data, true);
  return baseAppendFileSync(path, data, options);
}

export function readdirSync(path, options) {
  return checkDirList(baseReaddirSync(path, options));
}

export function readFile(path, options, callback) {
  let opts = options;
  let cb = callback;
  if (typeof opts === "function") {
    cb = opts;
    opts = undefined;
  }
  if (typeof cb !== "function") throw quotaError("ERR_INVALID_ARG_TYPE", "readFile 需要 callback");
  baseReadFile(path, opts, function (err, data) {
    if (err) { cb(err); return; }
    try { checkReadData(data, opts); cb(null, data); } catch (e) { cb(e); }
  });
}

export function writeFile(path, data, options, callback) {
  let opts = options;
  let cb = callback;
  if (typeof opts === "function") {
    cb = opts;
    opts = undefined;
  }
  if (typeof cb !== "function") throw quotaError("ERR_INVALID_ARG_TYPE", "writeFile 需要 callback");
  try {
    checkWriteData(path, data, false);
  } catch (e) {
    cb(e);
    return;
  }
  baseWriteFile(path, data, opts, cb);
}

export function appendFile(path, data, options, callback) {
  let opts = options;
  let cb = callback;
  if (typeof opts === "function") {
    cb = opts;
    opts = undefined;
  }
  if (typeof cb !== "function") throw quotaError("ERR_INVALID_ARG_TYPE", "appendFile 需要 callback");
  try {
    checkWriteData(path, data, true);
  } catch (e) {
    cb(e);
    return;
  }
  baseAppendFile(path, data, opts, cb);
}

export function readdir(path, options, callback) {
  let opts = options;
  let cb = callback;
  if (typeof opts === "function") {
    cb = opts;
    opts = undefined;
  }
  if (typeof cb !== "function") throw quotaError("ERR_INVALID_ARG_TYPE", "readdir 需要 callback");
  baseReaddir(path, opts, function (err, list) {
    if (err) { cb(err); return; }
    try { checkDirList(list); cb(null, list); } catch (e) { cb(e); }
  });
}
// 默认导入拿到的是 base 对象；把核心配额包装也挂上去，否则 `import fs` 绕过。
const coreQuota = {
  readFileSync,
  writeFileSync,
  appendFileSync,
  readdirSync,
  readFile,
  writeFile,
  appendFile,
  readdir,
};
for (const name of Object.keys(coreQuota)) attach(base, name, coreQuota[name]);

async function quotaPromisesReadFile(path, options) {
  const data = await basePromisesReadFile(path, options);
  if (maxFileBytes > 0) {
    const size = typeof data === "string" ? utf8Length(data) : data.length;
    checkFileSize(size);
  }
  return data;
}
async function quotaPromisesWriteFile(path, data, options) {
  checkWriteData(path, data, false);
  return basePromisesWriteFile(path, data, options);
}
async function quotaPromisesAppendFile(path, data, options) {
  checkWriteData(path, data, true);
  return basePromisesAppendFile(path, data, options);
}
async function quotaPromisesReaddir(path, options) {
  return checkDirList(await basePromisesReaddir(path, options));
}
attach(base.promises, "readFile", quotaPromisesReadFile);
attach(base.promises, "writeFile", quotaPromisesWriteFile);
attach(base.promises, "appendFile", quotaPromisesAppendFile);
attach(base.promises, "readdir", quotaPromisesReaddir);