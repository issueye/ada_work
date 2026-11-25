// node:https —— 与 node:http 同一实现的 TLS 版本。
//
// 同步客户端只有一份代码,https 只是把默认协议换成 https:,
// 并在选项里接受 rejectUnauthorized:false(对应 Node 的同一选项)。
import { makeModule } from "node:http";

const https = makeModule(true);

export const request = https.request;
export const get = https.get;
export const ClientRequest = https.ClientRequest;
export const IncomingMessage = https.IncomingMessage;
export const Agent = https.Agent;
export const STATUS_CODES = https.STATUS_CODES;
export const METHODS = https.METHODS;
export const globalAgent = https.globalAgent;
export const maxHeaderSize = https.maxHeaderSize;
export const createServer = https.createServer;
export default https;
