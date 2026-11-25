// node:fs/promises 的包装模块:在 moejs 的 promises 模块之上补齐缺失的 Promise 形态。
//
// 只补 Promise 形态(Node 的 promises 模块没有 Sync/回调形态),名字与同步模块一致。
import * as base from "ada:nodefs/promises";
import * as ext from "ada:fsx";

export * from "ada:nodefs/promises";

// 逐个具名导出:不能 `export * from "ada:fsx"`(它带 promises 等其它名字,会与上游重名)。
export const chmod = ext.promises.chmod;
export const chown = ext.promises.chown;
export const close = ext.promises.close;
export const fchmod = ext.promises.fchmod;
export const fchown = ext.promises.fchown;
export const fstat = ext.promises.fstat;
export const fsync = ext.promises.fsync;
export const ftruncate = ext.promises.ftruncate;
export const futimes = ext.promises.futimes;
export const link = ext.promises.link;
export const mkdtemp = ext.promises.mkdtemp;
export const open = ext.promises.open;
export const opendir = ext.promises.opendir;
export const read = ext.promises.read;
export const readv = ext.promises.readv;
export const symlink = ext.promises.symlink;
export const truncate = ext.promises.truncate;
export const utimes = ext.promises.utimes;
export const write = ext.promises.write;
export const writev = ext.promises.writev;

const promisesModule = Object.assign({}, base);
promisesModule.readFile = readFile;
promisesModule.writeFile = writeFile;
promisesModule.appendFile = appendFile;
promisesModule.readdir = readdir;
export default promisesModule;

// 核心 fs/promises 的配额包装；fsx promises 路径已由 Go 侧强制。
const fsQuota = globalThis.__adaFSLimits || {};
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

export async function readFile(path, options) {
  const data = await base.readFile(path, options);
  if (maxFileBytes > 0) {
    const size = typeof data === "string" ? utf8Length(data) : data.length;
    checkFileSize(size);
  }
  return data;
}

export async function writeFile(path, data, options) {
  checkFileSize(dataByteLength(data));
  return base.writeFile(path, data, options);
}

export async function appendFile(path, data, options) {
  if (maxFileBytes > 0) {
    let size = dataByteLength(data);
    try { size += (await base.stat(path)).size; } catch (_) {}
    checkFileSize(size);
  }
  return base.appendFile(path, data, options);
}

export async function readdir(path, options) {
  const list = await base.readdir(path, options);
  if (maxDirEntries > 0 && list.length > maxDirEntries) {
    throw quotaError("EFBIG", "目录条目超过 MaxDirEntries(" + maxDirEntries + ")");
  }
  return list;
}