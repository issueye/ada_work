// node:dns/promises —— 同步解析的 Promise 包装。
//
// Promise 在构造函数里就结算:调用方 await 时结果已经就绪,
// 不依赖事件循环或微任务队列。失败一律 reject,不抛同步异常。
import dns from "node:dns";

function wrap(fn) {
  return function (...args) {
    return new Promise((resolve, reject) => {
      try {
        resolve(fn(...args));
      } catch (err) {
        reject(err);
      }
    });
  };
}

export const lookup = wrap(dns.lookup);
export const lookupService = function lookupService() {
  const err = new Error("dns.promises.lookupService 不提供");
  err.code = "ERR_NOT_SUPPORTED";
  return Promise.reject(err);
};
export const resolve = wrap(dns.resolve);
export const resolve4 = wrap(dns.resolve4);
export const resolve6 = wrap(dns.resolve6);
export const resolveCname = wrap(dns.resolveCname);
export const resolveMx = wrap(dns.resolveMx);
export const resolveNs = wrap(dns.resolveNs);
export const resolveTxt = wrap(dns.resolveTxt);
export const resolveSrv = wrap(dns.resolveSrv);
export const resolvePtr = wrap(dns.resolvePtr);
export const reverse = wrap(dns.reverse);
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
