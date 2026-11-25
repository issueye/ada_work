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
