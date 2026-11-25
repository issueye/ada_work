// node:stream/promises —— pipeline / finished 的 Promise 形态。
//
// 形态是 Promise,语义仍是 node:stream 的同步实现:流水线在数据推完后立即 resolve。
import { pipeline as pipelineCb, finished as finishedCb } from "node:stream";

export function pipeline() {
  const args = Array.prototype.slice.call(arguments);
  return new Promise(function (resolve, reject) {
    args.push(function (err) {
      if (err) reject(err);
      else resolve();
    });
    pipelineCb.apply(null, args);
  });
}

export function finished(stream) {
  return new Promise(function (resolve, reject) {
    finishedCb(stream, function (err) {
      if (err) reject(err);
      else resolve();
    });
  });
}

export default { pipeline, finished };
