// node:tty —— 终端能力的最小如实实现。
//
// 运行时没有终端:所有 fd 都不是 TTY,也不存在可交付给插件的 stdin/stdout 流。
// 所以 isatty() 恒为 false(这是事实,不是桩);ReadStream/WriteStream 构造即抛
// ERR_NOT_SUPPORTED——它们必须包一个 fd,而我们没有可以包的东西。
//
// 需要输出时用 ctx.log(宿主日志),需要读行时用 node:readline 读一个可读流。
function unsupported(name) {
  const err = new Error("tty." + name + " 不提供:运行时没有终端文件描述符;输出请用 ctx.log");
  err.code = "ERR_NOT_SUPPORTED";
  return err;
}

export function isatty() {
  return false;
}

export class ReadStream {
  constructor() {
    throw unsupported("ReadStream");
  }
}

export class WriteStream {
  constructor() {
    throw unsupported("WriteStream");
  }
}

export default { isatty, ReadStream, WriteStream };
