// node:crypto —— 宿主自建的 Node 20 子集(虚拟 ES 模块，moejs 装载)。
//
// 实现:摘要(md5/sha1/sha256/sha384/sha512)、HMAC、PBKDF2、随机字节/随机整数/UUID、
// 定长比较。全部走宿主 __adaHost.crypto,跨边界只传 base64,不传二进制对象。
//
// 不做(不是「以后再说」,而是引擎/依赖的边界):
//   - 非对称加密与签名(RSA/ECDSA/Ed25519)、证书(X509)、TLS:需要宿主 PKI 与密钥生命周期;
//   - 对称加密的 createCipheriv/createDecipheriv:它们是有状态流(update 多次),需要跨调用句柄;
//   - createHash 的 .copy() 之外的高级用法(如 Hmac 复用、流式 transform)。
// 需要以上能力时请用 Go 插件(同进程、无跨边界),而不是用近似实现冒充。
import { Buffer } from "node:buffer";

const host = globalThis.__adaHost;

const ALGORITHMS = ["md5", "sha1", "sha256", "sha384", "sha512"];

function cryptoHost(name) {
  if (!host || !host.crypto || typeof host.crypto[name] !== "function") {
    const err = new Error("node:crypto 不可用:宿主没有安装 " + name);
    err.code = "ERR_CRYPTO_UNAVAILABLE";
    throw err;
  }
  return host.crypto[name];
}

function codeError(Ctor, code, message) {
  const err = new Ctor(message);
  err.code = code;
  return err;
}

// normAlg 把 OpenSSL 风格的名字归一:大小写不敏感,允许 "sha-256" 这种带连字符的写法。
function normAlg(algorithm) {
  const raw = String(algorithm === undefined || algorithm === null ? "" : algorithm)
    .toLowerCase()
    .replace(/-/g, "");
  if (ALGORITHMS.indexOf(raw) < 0) {
    throw codeError(Error, "ERR_CRYPTO_INVALID_DIGEST", "不支持的摘要算法: " + JSON.stringify(algorithm));
  }
  return raw;
}

function toBuffer(data, encoding) {
  if (typeof data === "string") {
    return Buffer.from(data, encoding === undefined || encoding === null ? "utf8" : String(encoding));
  }
  if (data instanceof Uint8Array) return Buffer.from(data);
  throw codeError(TypeError, "ERR_INVALID_ARG_TYPE", "输入必须是 string / Buffer / Uint8Array");
}

function concat(chunks) {
  if (chunks.length === 0) return Buffer.alloc(0);
  if (chunks.length === 1) return chunks[0];
  if (typeof Buffer.concat === "function") return Buffer.concat(chunks);
  let total = 0;
  for (const chunk of chunks) total += chunk.length;
  const out = Buffer.alloc(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

function digestOf(alg, buffer) {
  const out = cryptoHost("hash")(alg, buffer.toString("base64"));
  return Buffer.from(out, "base64");
}

// encodeDigest 按 Node 的编码约定返回;未给编码时返回 Buffer。
function encodeDigest(buffer, encoding) {
  if (encoding === undefined || encoding === null) return buffer;
  const enc = String(encoding).toLowerCase();
  if (enc === "buffer") return buffer;
  if (enc === "hex" || enc === "base64" || enc === "base64url") return buffer.toString(enc);
  throw codeError(TypeError, "ERR_INVALID_ARG_VALUE", "不支持的输出编码: " + JSON.stringify(encoding));
}

class Hash {
  constructor(algorithm) {
    this.algorithm = normAlg(algorithm);
    this.chunks = [];
    this.finalized = false;
  }

  update(data, encoding) {
    if (this.finalized) throw codeError(Error, "ERR_CRYPTO_HASH_FINALIZED", "Hash 已经调用过 digest");
    this.chunks.push(toBuffer(data, encoding));
    return this;
  }

  digest(encoding) {
    if (this.finalized) throw codeError(Error, "ERR_CRYPTO_HASH_FINALIZED", "Hash 已经调用过 digest");
    this.finalized = true;
    const buffer = digestOf(this.algorithm, concat(this.chunks));
    this.chunks = [];
    return encodeDigest(buffer, encoding);
  }

  copy() {
    const clone = new Hash(this.algorithm);
    clone.chunks = this.chunks.slice();
    clone.finalized = this.finalized;
    return clone;
  }
}

class Hmac {
  constructor(algorithm, key) {
    this.algorithm = normAlg(algorithm);
    this.key = toBuffer(key);
    this.chunks = [];
    this.finalized = false;
  }

  update(data, encoding) {
    if (this.finalized) throw codeError(Error, "ERR_CRYPTO_HASH_FINALIZED", "Hmac 已经调用过 digest");
    this.chunks.push(toBuffer(data, encoding));
    return this;
  }

  digest(encoding) {
    if (this.finalized) throw codeError(Error, "ERR_CRYPTO_HASH_FINALIZED", "Hmac 已经调用过 digest");
    this.finalized = true;
    const out = cryptoHost("hmac")(this.algorithm, this.key.toString("base64"), concat(this.chunks).toString("base64"));
    this.chunks = [];
    return encodeDigest(Buffer.from(out, "base64"), encoding);
  }
}

export function createHash(algorithm) {
  return new Hash(algorithm);
}

export function createHmac(algorithm, key) {
  return new Hmac(algorithm, key);
}

export function getHashes() {
  return ALGORITHMS.slice();
}

function pbkdf2Params(password, salt, iterations, keylen, digest) {
  const alg = digest === undefined || digest === null || digest === "" ? "sha1" : normAlg(digest);
  const n = Number(iterations);
  const len = Number(keylen);
  if (!Number.isInteger(n) || n < 1) throw codeError(RangeError, "ERR_OUT_OF_RANGE", "iterations 必须是正整数");
  if (!Number.isInteger(len) || len < 1) throw codeError(RangeError, "ERR_OUT_OF_RANGE", "keylen 必须是正整数");
  return {
    pass: toBuffer(password).toString("base64"),
    salt: toBuffer(salt).toString("base64"),
    iterations: n,
    keylen: len,
    alg,
  };
}

export function pbkdf2Sync(password, salt, iterations, keylen, digest) {
  const p = pbkdf2Params(password, salt, iterations, keylen, digest);
  const out = cryptoHost("pbkdf2")(p.pass, p.salt, p.iterations, p.keylen, p.alg);
  return Buffer.from(out, "base64");
}

// pbkdf2 是回调形态:计算在宿主 goroutine 里做,结算后回事件循环(不卡住调用)。
export function pbkdf2(password, salt, iterations, keylen, digest, callback) {
  if (typeof digest === "function") {
    callback = digest;
    digest = "sha1";
  }
  if (typeof callback !== "function") {
    throw codeError(TypeError, "ERR_INVALID_ARG_TYPE", "pbkdf2 需要一个回调");
  }
  let p;
  try {
    p = pbkdf2Params(password, salt, iterations, keylen, digest);
  } catch (err) {
    callback(err);
    return;
  }
  cryptoHost("pbkdf2Async")(p.pass, p.salt, p.iterations, p.keylen, p.alg).then(
    function (out) {
      callback(null, Buffer.from(out, "base64"));
    },
    function (err) {
      callback(err);
    },
  );
}

export function randomBytes(size, callback) {
  const n = Number(size);
  if (!Number.isInteger(n) || n < 0) {
    throw codeError(RangeError, "ERR_OUT_OF_RANGE", "randomBytes 的 size 必须是非负整数");
  }
  const buffer = Buffer.from(cryptoHost("randomBytes")(n), "base64");
  if (typeof callback === "function") {
    Promise.resolve().then(function () {
      callback(null, buffer);
    });
  }
  return buffer;
}

function fillInto(buf, offset, size) {
  if (!(buf instanceof Uint8Array)) {
    throw codeError(TypeError, "ERR_INVALID_ARG_TYPE", "目标必须是 Buffer / Uint8Array");
  }
  const off = offset === undefined || offset === null ? 0 : Number(offset);
  const len = size === undefined || size === null ? buf.length - off : Number(size);
  if (!Number.isInteger(off) || !Number.isInteger(len) || off < 0 || len < 0 || off + len > buf.length) {
    throw codeError(RangeError, "ERR_OUT_OF_RANGE", "offset/size 越界");
  }
  buf.set(randomBytes(len), off);
  return buf;
}

export function randomFillSync(buf, offset, size) {
  return fillInto(buf, offset, size);
}

export function randomFill(buf, offset, size, callback) {
  // Node 允许 (buf, cb) / (buf, offset, cb) / (buf, offset, size, cb)。
  if (typeof offset === "function") {
    callback = offset;
    offset = undefined;
    size = undefined;
  } else if (typeof size === "function") {
    callback = size;
    size = undefined;
  }
  if (typeof callback !== "function") {
    throw codeError(TypeError, "ERR_INVALID_ARG_TYPE", "randomFill 需要一个回调");
  }
  try {
    fillInto(buf, offset, size);
  } catch (err) {
    callback(err);
    return;
  }
  Promise.resolve().then(function () {
    callback(null, buf);
  });
}

function randomUint32() {
  const bytes = randomBytes(4);
  return ((bytes[0] * 0x1000000) + (bytes[1] << 16) + (bytes[2] << 8) + bytes[3]) >>> 0;
}

export function randomInt(min, max, callback) {
  let lo = 0;
  let hi = 0;
  if (max === undefined || typeof max === "function") {
    if (typeof max === "function") callback = max;
    hi = Number(min);
  } else {
    lo = Number(min);
    hi = Number(max);
  }
  if (!Number.isInteger(lo) || !Number.isInteger(hi) || hi <= lo) {
    throw codeError(RangeError, "ERR_OUT_OF_RANGE", "randomInt 需要一个合法的区间");
  }
  const range = hi - lo;
  const limit = Math.floor(0x100000000 / range) * range;
  let value = 0;
  for (let i = 0; i < 100; i++) {
    value = randomUint32();
    if (value < limit) break;
  }
  const out = lo + (value % range);
  if (typeof callback === "function") {
    Promise.resolve().then(function () {
      callback(null, out);
    });
  }
  return out;
}

export function timingSafeEqual(a, b) {
  const left = toBuffer(a);
  const right = toBuffer(b);
  if (left.length !== right.length) {
    throw codeError(RangeError, "ERR_CRYPTO_TIMING_SAFE_EQUAL_LENGTH", "timingSafeEqual 的两个输入长度必须相同");
  }
  return cryptoHost("timingSafeEqual")(left.toString("base64"), right.toString("base64"));
}

// randomUUID 生成 RFC 4122 v4 UUID;随机源仍是宿主 crypto/rand。
export function randomUUID() {
  const bytes = randomBytes(16);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = bytes.toString("hex");
  return (
    hex.slice(0, 8) + "-" + hex.slice(8, 12) + "-" + hex.slice(12, 16) + "-" +
    hex.slice(16, 20) + "-" + hex.slice(20)
  );
}

export default {
  createHash,
  createHmac,
  getHashes,
  pbkdf2,
  pbkdf2Sync,
  randomBytes,
  randomFill,
  randomFillSync,
  randomInt,
  randomUUID,
  timingSafeEqual,
  Hash,
  Hmac,
};
