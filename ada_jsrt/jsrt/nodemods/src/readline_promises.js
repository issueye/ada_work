// node:readline/promises —— question 的 Promise 形态。
//
// 复用 node:readline 的 Interface(行切分、close、异步迭代),只把 question 改成 Promise。
import { Interface as BaseInterface } from "node:readline";

export class Interface extends BaseInterface {
  question(query) {
    const self = this;
    return new Promise(function (resolve) {
      BaseInterface.prototype.question.call(self, query, function (answer) {
        resolve(answer);
      });
    });
  }
}

export function createInterface(options) {
  return new Interface(options);
}

export default { Interface, createInterface };
