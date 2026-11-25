// node:dns/promises —— 基于回调形态的 Promise 包装。
//
// resolve* 通过回调形态走宿主的异步解析；lookup 也把回调包成 Promise，
// 因此 await 期间事件循环可以继续处理其它任务。
import dns from "node:dns";

function promisify(fn, ...args) {
  return new Promise((resolve, reject) => {
    fn(...args, (err, value) => {
      if (err) reject(err);
      else resolve(value);
    });
  });
}

export function lookup(hostname, options) {
  const opts = options && typeof options === "object" ? options : {};
  return new Promise((resolve, reject) => {
    dns.lookup(hostname, opts, (err, address, family) => {
      if (err) {
        reject(err);
        return;
      }
      if (opts.all) resolve(address);
      else resolve({ address, family });
    });
  });
}

export function lookupService() {
  const err = new Error("dns.promises.lookupService 不提供");
  err.code = "ERR_NOT_SUPPORTED";
  return Promise.reject(err);
}

export const resolve = (hostname, rrtype) => promisify(dns.resolve, hostname, rrtype || "A");
export const resolve4 = (hostname, options) => promisify(dns.resolve4, hostname, options || {});
export const resolve6 = (hostname, options) => promisify(dns.resolve6, hostname, options || {});
export const resolveCname = (hostname) => promisify(dns.resolveCname, hostname);
export const resolveMx = (hostname) => promisify(dns.resolveMx, hostname);
export const resolveNs = (hostname) => promisify(dns.resolveNs, hostname);
export const resolveTxt = (hostname) => promisify(dns.resolveTxt, hostname);
export const resolveSrv = (hostname) => promisify(dns.resolveSrv, hostname);
export const resolvePtr = (hostname) => promisify(dns.resolvePtr, hostname);
export const reverse = (ip) => promisify(dns.reverse, ip);

export const setServers = dns.setServers;
export const getServers = dns.getServers;
export const Resolver = dns.Resolver;

export default {
  lookup,
  lookupService,
  resolve,
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
