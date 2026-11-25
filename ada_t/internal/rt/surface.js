// surface.js — ada:surface 运行时(moejs 内执行,纯 ESM,无 eval)。
//
// 职责(ui-framework.md §6.1 内核边界的 native 版):
//   - 细粒度响应式:reactive(Proxy 深代理)/ ref / computed / watch / effect
//   - Plan 解释器:渲染 = 遍历编译期 Plan,求值动态槽,产出 widget 树(JSON)
//   - 事件分发:handler 注册表;fire 时求值动作 AST(裸名/调用/赋值)
//   - 组件实例:setup 只跑一次;props 响应式更新;子组件实例复用与卸载
//
// 明确不做:eval/new Function、innerHTML、任意 import()(导入只经 Go 的 Link);
// 渲染输出是纯数据树,由 Go 侧后端重放为原生控件。
//
// 与宿主(Go)的边界:
//   - 宿主注入全局 __adatHost = { notify(), log(...args), t(instId, key, argsJSON) }
//   - 本模块导出 registerComponent / mountWindow / renderWindow / fireHandler
//   - 渲染由 Go 驱动:notify → Go 在 loop 上排队渲染 pass;fire 后 Go 同样排渲染

// ============================================================
// 一、响应式
// ============================================================

let activeEffect = null
let flushScheduled = false
// 渲染深度:渲染 pass 期间的 props 同步只收集依赖,不调度新的渲染,
// 避免「渲染 → 写 props → notify → 再渲染」的自激;渲染期其它状态写入
// (如子组件 onMount 改父状态)仍会在渲染结束后补一次渲染。
let renderDepth = 0
let syncingProps = 0
let renderStateDirty = false
let stateDirty = false // 自上次 notify 以来是否发生过真实状态写入
const dirtyQueue = new Set()
const targetMap = new WeakMap() // raw target → Map(key → Set<effect>)

function scheduleFlush() {
  if (flushScheduled) return
  flushScheduled = true
  queueMicrotask(flushEffects)
  // 兜底:任务回调上下文里排队的微任务不被 eventloop drain 时,由定时器补一次。
  // 若微任务已 flush 过,stateDirty 已被消费,这里的二次 flush 不再 notify,
  // 因此不会产生「一次变更两次整树渲染」。
  setTimeout(flushEffects, 1)
}

// flushEffects 执行排队的 effect;notifyHost=false 用于「渲染期收集到的依赖」:
// 当前帧已经包含新值,不需要再通知宿主安排渲染。
// computed 失效会把下游 effect 重新入队,这里循环到静止再统一 notify,
// 保证一次状态变更只安排一个渲染 pass。
function flushEffects(notifyHost = true) {
  flushScheduled = false
  for (let round = 0; dirtyQueue.size > 0 && round < 100; round++) {
    const list = Array.from(dirtyQueue)
    dirtyQueue.clear()
    for (const e of list) {
      try {
        e.run()
      } catch (err) {
        __adatHost.log('surface: effect 执行异常: ' + (err && err.message ? err.message : String(err)))
      }
    }
  }
  // 通知 Go 安排渲染 pass(渲染任务在 loop 队列上,不会与本次 flush 重入)
  if (!notifyHost || !stateDirty) return
  stateDirty = false
  __adatHost.notify()
}

function isRefLike(v) {
  return v !== null && typeof v === 'object' && v.__isRef === true
}

function unwrapRef(v) {
  return isRefLike(v) ? v.value : v
}

function unwrapDeep(v) {
  if (v === null || typeof v !== 'object') return v
  if (isRefLike(v)) return v.value
  if (v.__isReactive === true) return v.__raw
  return v
}

function track(target, key) {
  if (activeEffect === null) return
  let m = targetMap.get(target)
  if (m === undefined) targetMap.set(target, (m = new Map()))
  let s = m.get(key)
  if (s === undefined) m.set(key, (s = new Set()))
  s.add(activeEffect)
  if (activeEffect.deps !== undefined) activeEffect.deps.add(s) // 记录依赖集,支持 stop
}

function cleanupDeps(e) {
  for (const s of e.deps) s.delete(e)
  e.deps.clear()
}

function trigger(target, key) {
  // 即使没有 JS 侧依赖(渲染是 Go 驱动的全树重求值),也要调度 flush
  // → notify → 重渲染:否则异步续体等路径的状态变更不会触发渲染。
  const m = targetMap.get(target)
  if (m !== undefined) {
    const s = m.get(key)
    if (s !== undefined) {
      for (const e of s) dirtyQueue.add(e)
    }
  }
  if (renderDepth > 0) {
    if (syncingProps === 0) renderStateDirty = true
    return
  }
  stateDirty = true
  scheduleFlush()
}

// triggerAll:数组的批量变更(push/splice/索引赋值)粗粒度地触发
// 数组自身依赖 + 父对象上的键依赖(渲染读的是 state.items)。
function triggerAll(target) {
  const m = targetMap.get(target)
  if (m !== undefined) {
    for (const s of m.values()) {
      for (const e of s) dirtyQueue.add(e)
    }
  }
  const links = parentLinks.get(target)
  if (links !== undefined) {
    for (const l of links) trigger(l.target, l.key)
  }
  if (syncingProps > 0) return
  if (renderDepth > 0) {
    renderStateDirty = true
    return
  }
  stateDirty = true
  scheduleFlush()
}

const ARRAY_METHODS = new Set(['push', 'pop', 'shift', 'unshift', 'splice', 'sort', 'reverse'])

// parentLinks:数组 → [父对象与键];数组的批量变更需要同时触发父键
// (渲染依赖读的是 state.items,依赖记在 state 上,不在数组上)。
const parentLinks = new WeakMap()

function linkParent(rawArray, parentTarget, parentKey) {
  let links = parentLinks.get(rawArray)
  if (links === undefined) parentLinks.set(rawArray, (links = []))
  for (const l of links) {
    if (l.target === parentTarget && l.key === parentKey) return
  }
  links.push({ target: parentTarget, key: parentKey })
}

function reactive(obj) {
  if (obj === null || typeof obj !== 'object' || obj.__isReactive === true) return obj
  return new Proxy(obj, {
    get(t, k, r) {
      if (k === '__isReactive') return true
      if (k === '__raw') return t
      // 数组变更方法:调用后整体触发
      if (Array.isArray(t) && ARRAY_METHODS.has(k)) {
        return function (...args) {
          const res = t[k](...args)
          triggerAll(t)
          return res
        }
      }
      track(t, k)
      const v = Reflect.get(t, k, r)
      if (v === null || typeof v !== 'object') return v
      if (Array.isArray(v)) linkParent(v, t, k)
      return reactive(v)
    },
    set(t, k, nv, r) {
      nv = unwrapDeep(nv)
      const old = Reflect.get(t, k, r)
      if (Object.is(old, nv)) return true
      const ok = Reflect.set(t, k, nv, r)
      if (Array.isArray(t)) triggerAll(t)
      else trigger(t, k)
      return ok
    },
    deleteProperty(t, k) {
      const ok = delete t[k]
      if (Array.isArray(t)) triggerAll(t)
      else trigger(t, k)
      return ok
    },
    has(t, k) {
      track(t, k)
      return Reflect.has(t, k)
    },
  })
}

function ref(v) {
  const deps = new Set()
  const r = {
    __isRef: true,
    get value() {
      if (activeEffect !== null) deps.add(activeEffect)
      return v
    },
    set value(nv) {
      nv = unwrapDeep(nv)
      if (Object.is(v, nv)) return
      v = nv
      for (const e of deps) dirtyQueue.add(e)
      stateDirty = true
      scheduleFlush()
    },
  }
  return r
}

function computed(getter) {
  let val
  let dirty = true
  const deps = new Set() // 依赖本 computed 的下游 effect(watch 等)
  const self = {
    run() {
      dirty = true
      for (const e of deps) dirtyQueue.add(e)
    },
  }
  const r = {
    __isRef: true, // computed 按 ref 语义自动解包
    get value() {
      if (activeEffect !== null && activeEffect !== self) deps.add(activeEffect)
      if (dirty) {
        const prev = activeEffect
        activeEffect = self
        try {
          val = unwrapRef(getter())
        } finally {
          activeEffect = prev
        }
        dirty = false
      }
      return val
    },
  }
  return r
}

function watch(source, cb, opts) {
  const getter =
    typeof source === 'function'
      ? source
      : isRefLike(source)
        ? () => source.value
        : () => source
  let last
  const e = {
    deps: new Set(), // track 记录的依赖集合(stop 时逐一移除)
    active: true,
    run() {
      if (!this.active) return
      cleanupDeps(this)
      const prev = activeEffect
      activeEffect = this
      let nv
      try {
        nv = getter()
      } finally {
        activeEffect = prev
      }
      if (!Object.is(nv, last)) {
        const old = last
        last = nv
        cb(nv, old)
      }
    },
  }
  // 初始化 last 并建立依赖,但不触发回调
  {
    const prev = activeEffect
    activeEffect = e
    try {
      last = getter()
    } finally {
      activeEffect = prev
    }
  }
  if (opts && opts.immediate) cb(last, undefined)
  const stop = function () {
    if (!e.active) return
    e.active = false
    cleanupDeps(e)
    dirtyQueue.delete(e)
  }
  // setup 里创建的 watch 随实例卸载/HMR 重新 setup 自动停止。
  if (mountingInstance !== null) mountingInstance.cleanups.push(stop)
  return stop
}

function effect(fn) {
  const e = {
    deps: new Set(), // track 记录的依赖集合(stop 时逐一移除)
    active: true,
    run() {
      if (!this.active) return
      cleanupDeps(this)
      const prev = activeEffect
      activeEffect = this
      try {
        fn()
      } finally {
        activeEffect = prev
      }
    },
  }
  e.run()
  const stop = function () {
    if (!e.active) return
    e.active = false
    cleanupDeps(e)
    dirtyQueue.delete(e)
  }
  if (mountingInstance !== null) mountingInstance.cleanups.push(stop)
  return stop
}

// ============================================================
// 二、组件注册表与实例
// ============================================================

const components = new Map() // name → default export(setup 等)
const plans = new Map()      // name → plan 对象
const styles = new Map()     // name → class → tokens
const i18n = new Map()       // name → locale → key → 文案('__app' 是应用级)
const instances = new Map()  // instId → instance
const handlers = new Map()   // hid → { inst, action, frames, gen }
const windows = new Map()    // winId → instId
const services = new Map()   // name → { method → 宿主原生函数 } (ctx.api.call)
const eventHandlers = new Map() // event → Set<{inst, fn}>(Go → 组件事件)
const componentPlugins = new Map() // 组件名 → 插件 id(宿主组件为空)
const pluginScopes = new Map()      // 插件 id → Set<能力名>(默认拒绝)
let themeTokens = {}                // 宿主主题令牌(ctx.theme / 模板里 theme.* 用)
let seq = 0
let renderGen = 0 // 渲染代数:每轮渲染递增;旧代 handler 渲染后被清扫
let renderCount = 0 // 渲染 pass 计数(stats 暴露,回归「一次变更一次渲染」)
let mountingInstance = null
let currentLocale = 'zh-CN'

function makeInstance(name, propsObj) {
  const def = components.get(name)
  if (def === undefined) throw new Error('surface: 组件未注册: ' + name)
  const inst = {
    id: 'i' + ++seq,
    name,
    def,
    props: propsObj,
    scope: {},
    winId: null,        // 所属窗口(mountWindow 设定;子实例继承)
    mounts: [],
    unmounts: [],
    cleanups: [],
    emitMap: {},        // event → { hid, parentId }
    error: undefined,   // setup 失败的错误信息(错误边界)
    eventRegs: [],      // ctx.on 的订阅记录(卸载时退订)
    children: new Map(), // cacheKey → instId(本实例的子组件)
    child: null,        // 渲染期临时:当前 comp 节点的父登记
    plugin: componentPlugins.get(name) || '', // 所属插件 id(宿主组件为空)
  }
  instances.set(inst.id, inst)
  return inst
}

// requireCapability:插件只能调用清单里声明的服务方法(默认拒绝)。
// 宿主组件不受限(plugin 为空)。
function requireCapability(inst, service, method) {
  const pid = inst.plugin
  if (!pid) return
  const key = String(service) + '.' + String(method)
  const scope = pluginScopes.get(pid)
  if (scope === undefined) {
    throw new Error('surface: 插件 ' + pid + ' 未登记能力白名单')
  }
  if (scope.has(key) || scope.has('*')) return
  for (const c of scope) {
    if (c.endsWith('.*') && key.startsWith(c.slice(0, -1))) return
  }
  throw new Error('surface: 插件 ' + pid + ' 无能力调用 ' + key + '(需在 plugin.json 的 capabilities 声明)')
}

function makeCtx(inst) {
  return {
    // 插件 id(宿主组件为空串)
    pluginId: inst.plugin || '',
    // theme(key?):宿主主题令牌;不传 key 返回整个对象,缺失 key 返回 ''
    theme(key) {
      // 返回副本:插件不能改写宿主的主题对象
      if (key === undefined) return Object.assign({}, themeTokens)
      const v = themeTokens[String(key)]
      return v === undefined ? '' : v
    },
    // emitHost(event, payload):插件 → 宿主,事件名自动加 plugin.<id>. 前缀
    emitHost(event, payload) {
      if (!inst.plugin) {
        __adatHost.log('surface: emitHost 只能在插件组件里调用(' + inst.name + ')')
        return
      }
      __adatHost.emit('plugin.' + inst.plugin + '.' + String(event),
        payload === undefined ? '' : JSON.stringify(payload))
    },
    emit(event, payload) {
      const reg = inst.emitMap[event]
      if (reg === undefined) {
        __adatHost.log('surface: emit ' + event + ' 没有父监听(组件 ' + inst.name + ')')
        return
      }
      runHandler(reg.hid, payload === undefined ? null : payload)
    },
    t(key, ...args) {
      return tLookup(inst.id, String(key), args)
    },
    log(...args) {
      __adatHost.log.apply(null, ['[' + inst.name + ']'].concat(args.map(String)))
    },
    onCleanup(fn) {
      inst.cleanups.push(fn)
    },
    // api.call(service, method, ...args):调用 Go 侧 Expose 的服务方法。
    // 同步原生调用(在 loop goroutine 上执行);参数与返回值走 JSON;
    // Go 返回 error 时这里抛出(JS 侧 try/catch)。
    // 慢任务请用 callAsync:Go 侧跑在独立 goroutine,返回 Promise,不阻塞渲染。
    api: {
      call(service, method, ...args) {
        requireCapability(inst, service, method)
        const svc = services.get(String(service))
        if (svc === undefined) {
          throw new Error('surface: 服务未注册: ' + service)
        }
        const fn = svc[String(method)]
        if (typeof fn !== 'function') {
          throw new Error('surface: 服务 ' + service + ' 没有方法 ' + method)
        }
        return fn(JSON.stringify(args))
      },
      callAsync(service, method, ...args) {
        requireCapability(inst, service, method)
        const svc = services.get(String(service))
        if (svc === undefined) {
          throw new Error('surface: 服务未注册: ' + service)
        }
        const fn = svc[String(method) + '!async']
        if (typeof fn !== 'function') {
          throw new Error('surface: 服务 ' + service + ' 没有异步方法 ' + method + '(Expose 时会自动提供)')
        }
        return fn(JSON.stringify(args))
      },
    },
    // on(event, fn):订阅 Go 侧 Notify 的广播事件;实例卸载时自动退订。
    // 插件组件只订阅自己命名空间的事件(plugin.<id>.):宿主 EmitTo 才可达,
    // 避免多插件之间事件串流。
    on(event, fn) {
      if (typeof fn !== 'function') {
        throw new Error('surface: ctx.on 需要函数')
      }
      const key = inst.plugin ? 'plugin.' + inst.plugin + '.' + String(event) : String(event)
      let hs = eventHandlers.get(key)
      if (hs === undefined) eventHandlers.set(key, (hs = new Set()))
      const entry = { inst, fn }
      hs.add(entry)
      inst.eventRegs.push({ key, entry })
    },
    // toast(message):在所属窗口底部弹一条几秒自动消失的提示。
    toast(message) {
      __adatHost.toast(inst.winId || '', String(message))
    },
  }
}

// onMount / onUnmount:只在 setup 里调用(挂在当前正在装配的实例上)。
function onMount(fn) {
  if (mountingInstance === null) {
    throw new Error('surface: onMount 只能在 setup 里调用')
  }
  mountingInstance.mounts.push(fn)
}

function onUnmount(fn) {
  if (mountingInstance === null) {
    throw new Error('surface: onUnmount 只能在 setup 里调用')
  }
  mountingInstance.unmounts.push(fn)
}

function runSetup(inst) {
  const def = inst.def
  mountingInstance = inst
  let scope = {}
  try {
    if (typeof def.setup === 'function') {
      scope = def.setup(inst.props, makeCtx(inst)) || {}
    }
  } catch (err) {
    // 错误边界:setup 失败不炸窗口,实例标记错误,渲染层出错误卡片
    inst.error = err && err.message ? err.message : String(err)
    __adatHost.log('surface: setup 失败(' + inst.name + '): ' + inst.error)
  } finally {
    mountingInstance = null
  }
  inst.scope = scope
  // onMount:setup 结束后立即执行(MVP;无首帧前/后区分)
  for (const fn of inst.mounts) {
    try {
      fn()
    } catch (err) {
      __adatHost.log('surface: onMount 异常: ' + (err && err.message ? err.message : String(err)))
    }
  }
  inst.mounts.length = 0
}

function disposeInstance(inst) {
  for (const [, childId] of inst.children) {
    const c = instances.get(childId)
    if (c !== undefined) disposeInstance(c)
  }
  inst.children.clear()
  for (const fn of inst.unmounts) {
    try {
      fn()
    } catch (err) {
      __adatHost.log('surface: onUnmount 异常: ' + String(err))
    }
  }
  for (const fn of inst.cleanups) {
    try {
      fn()
    } catch (err) {
      __adatHost.log('surface: cleanup 异常: ' + String(err))
    }
  }
  inst.unmounts.length = 0
  inst.cleanups.length = 0
  // 退订 Go 事件(ctx.on)
  for (const reg of inst.eventRegs) {
    const hs = eventHandlers.get(reg.key)
    if (hs !== undefined) {
      hs.delete(reg.entry)
      if (hs.size === 0) eventHandlers.delete(reg.key)
    }
  }
  inst.eventRegs.length = 0
  // 清理该实例产生的 handler(按 inst 匹配)
  for (const [hid, h] of Array.from(handlers.entries())) {
    if (h.inst === inst) handlers.delete(hid)
  }
  instances.delete(inst.id)
}

// ============================================================
// 三、表达式求值器(AST 解释,不 eval)
// ============================================================

function resolveBase(s, key) {
  for (let i = s.frames.length - 1; i >= 0; i--) {
    const f = s.frames[i]
    if (f !== null && typeof f === 'object' && Object.prototype.hasOwnProperty.call(f, key)) {
      return f[key]
    }
  }
  return undefined
}

function pathString(ast) {
  if (ast.t !== 'path') return '(表达式)'
  let out = ''
  for (const seg of ast.p) {
    if (seg.key !== undefined) out += (out === '' ? '' : '.') + seg.key
    else out += '[…]'
  }
  return out
}

// tLookup:$t 查表;缺失 key 返回 key 本身并上报 warning(§4.4)。
function tLookup(instId, key, args) {
  let text
  const inst = instances.get(instId)
  if (inst !== undefined) {
    const m = i18n.get(inst.name)
    if (m !== undefined) {
      const loc = m[currentLocale]
      if (loc !== undefined && loc[key] !== undefined) text = loc[key]
    }
  }
  if (text === undefined) {
    const app = i18n.get('__app')
    if (app !== undefined) {
      const loc = app[currentLocale]
      if (loc !== undefined && loc[key] !== undefined) text = loc[key]
    }
  }
  if (text === undefined) {
    __adatHost.log('surface: i18n 缺失 key: ' + key)
    text = key
  }
  return text.replace(/\{(\d+)\}/g, (whole, n) => {
    const v = args[Number(n)]
    return v === undefined ? whole : String(v)
  })
}

function evalExpr(ast, s) {
  switch (ast.t) {
    case 'lit':
      return ast.v === undefined ? null : ast.v
    case 'arr':
      return (ast.args || []).map((a) => evalExpr(a, s))
    case 'path': {
      let v = resolveBase(s, ast.p[0].key)
      if (v === undefined) {
        // 未定义的根:视作 undefined(渲染端转空串;调试用日志)
        return undefined
      }
      for (let i = 1; i < ast.p.length; i++) {
        const seg = ast.p[i]
        if (v === null || v === undefined) return undefined
        if (seg.key !== undefined) {
          v = v[seg.key]
        } else {
          v = v[evalExpr(seg.idx, s)]
        }
      }
      return unwrapRef(v)
    }
    case 'un': {
      const v = evalExpr(ast.e, s)
      return ast.op === '!' ? !v : -v
    }
    case 'tern':
      return evalExpr(ast.e, s) ? evalExpr(ast.a, s) : evalExpr(ast.b, s)
    case 'bin': {
      const op = ast.op
      if (op === '&&') {
        const l = evalExpr(ast.l, s)
        return l ? evalExpr(ast.r, s) : l
      }
      if (op === '||') {
        const l = evalExpr(ast.l, s)
        return l ? l : evalExpr(ast.r, s)
      }
      if (op === '??') {
        const l = evalExpr(ast.l, s)
        return l === null || l === undefined ? evalExpr(ast.r, s) : l
      }
      const l = evalExpr(ast.l, s)
      const r = evalExpr(ast.r, s)
      switch (op) {
        case '+': return l + r
        case '-': return l - r
        case '*': return l * r
        case '/': return l / r
        case '%': return l % r
        case '==': return l === r
        case '!=': return l !== r
        case '<': return l < r
        case '>': return l > r
        case '<=': return l <= r
        case '>=': return l >= r
      }
      throw new Error('surface: 未知运算符 ' + op)
    }
    case 'call': {
      // $t 特例:i18n 在 JS 内查表(组件命名空间 → 应用级 → key 本身)
      if (ast.fn.t === 'path' && ast.fn.p.length === 1 && ast.fn.p[0].key === '$t') {
        const targs = ast.args || []
        const key = String(evalExpr(targs[0], s))
        const args = targs.slice(1).map((a) => evalExpr(a, s))
        return tLookup(s.inst ? s.inst.id : '', key, args)
      }
      const fn = evalExpr(ast.fn, s)
      if (typeof fn !== 'function') {
        throw new Error('surface: 调用目标不是函数: ' + pathString(ast.fn))
      }
      // 零参调用在 Go 侧被 omitempty 省略(ast.args 缺失),按空参数处理。
      return fn.apply(null, (ast.args || []).map((a) => evalExpr(a, s)))
    }
  }
  throw new Error('surface: 未知表达式类型 ' + ast.t)
}

// —— 动作求值(a-on / a-model 编译产物) ——

function evalAction(action, s) {
  switch (action.t) {
    case 'path': {
      const fn = evalExpr(action, s)
      if (typeof fn !== 'function') {
        throw new Error('surface: 动作不是函数: ' + pathString(action))
      }
      // 裸名动作:事件载荷作为第一个参数(等价 handler($event))
      fn(s.event)
      return
    }
    case 'call': {
      const fn = evalExpr(action.fn, s)
      if (typeof fn !== 'function') {
        throw new Error('surface: 动作不是函数: ' + pathString(action.fn))
      }
      fn.apply(null, (action.args || []).map((a) => evalExpr(a, s)))
      return
    }
    case 'assign': {
      assignPath(action.l, evalExpr(action.r, s), s)
      return
    }
  }
  throw new Error('surface: 未知动作类型 ' + action.t)
}

// assignPath 把值写入目标路径(写穿透 reactive/ref,触发依赖)。
function assignPath(target, value, s) {
  const rootKey = target.p[0].key
  let holder = null
  let baseVal = undefined
  for (let i = s.frames.length - 1; i >= 0; i--) {
    const f = s.frames[i]
    if (f !== null && typeof f === 'object' && Object.prototype.hasOwnProperty.call(f, rootKey)) {
      holder = f
      baseVal = f[rootKey]
      break
    }
  }
  if (holder === null) {
    throw new Error('surface: 赋值目标不在作用域内: ' + pathString(target))
  }
  // 根是 ref:query = v → query.value = v
  if (isRefLike(baseVal)) {
    if (target.p.length === 1) {
      baseVal.value = value
      return
    }
    // ref.value.xxx = v:穿过 value
    return assignInto(baseVal.value, target.p.slice(1), value)
  }
  assignInto(holder[rootKey], target.p.slice(1), value, holder, rootKey)
}

function assignInto(obj, segs, value, holder, rootKey) {
  if (segs.length === 0) {
    // 直接写 holder[key](rootKey 无 ref 包装时)
    if (holder !== null) {
      holder[rootKey] = value
    }
    return
  }
  let cur = obj
  for (let i = 0; i < segs.length - 1; i++) {
    const seg = segs[i]
    if (cur === null || cur === undefined) {
      throw new Error('surface: 赋值路径中途为空: ' + JSON.stringify(segs))
    }
    cur = cur[seg.key]
  }
  const last = segs[segs.length - 1]
  if (cur === null || cur === undefined) {
    throw new Error('surface: 赋值路径中途为空: ' + JSON.stringify(segs))
  }
  cur[last.key] = value
}

// ============================================================
// 四、渲染:Plan → widget 树(JSON)
// ============================================================

const containerTags = new Set(['Column', 'Row', 'Scroll', 'Split'])

function truthy(v) {
  return v !== undefined && v !== null && v !== false && v !== 0 && v !== ''
}

// modelKey 生成 a-model 的稳定绑定键(跨帧)。
function modelKey(target, s) {
  let out = pathString(target)
  if (s.keyChain !== undefined && s.keyChain.length > 0) {
    out += s.keyChain.map((k) => '@' + String(k)).join('')
  }
  return out
}

function textOf(parts, s) {
  let out = ''
  for (const p of parts) {
    if (p.v !== undefined) {
      out += p.v
    } else {
      const v = evalExpr(p.e, s)
      out += v === undefined || v === null ? '' : String(v)
    }
  }
  return out
}

function renderNodes(list, s) {
  const out = []
  if (list === undefined) return out
  for (const node of list) {
    switch (node.k) {
      case 'el':
        out.push(renderEl(node, s))
        break
      case 'if': {
        for (const br of node.branches) {
          if (br.cond === undefined || truthy(evalExpr(br.cond, s))) {
            const kids = renderNodes(br.children, s)
            for (const k of kids) out.push(k)
            break
          }
        }
        break
      }
      case 'for': {
        // 全量重求值。基准实测(见 README §性能):在常见条目规模下,
        // 全量求值快于 keyed 记忆化的簿记开销,故统一走本路径。
        const src = evalExpr(node.src, s)
        const arr = src === null || src === undefined ? [] : Array.isArray(src) ? src : Object.keys(src)
        for (let idx = 0; idx < arr.length; idx++) {
          const frame = {}
          frame[node.item] = arr[idx]
          if (node.index !== undefined && node.index !== '') frame[node.index] = idx
          const s2 = {
            inst: s.inst,
            frames: s.frames.concat([frame]),
            path: s.path + '/' + node.id + '[' + idx + ']',
            keyChain: (s.keyChain || []).concat([idx]),
          }
          const kids = renderNodes(node.children, s2)
          for (const k of kids) out.push(k)
        }
        break
      }
      case 'comp':
        out.push(renderComp(node, s))
        break
    }
  }
  return out
}

// applyStyle:把 class 令牌合入 props(静态属性已写入的优先)。
function applyStyle(props, classes, inst) {
  const tokens = styles.get(inst.name)
  if (tokens === undefined) return
  for (const cls of classes) {
    const rule = tokens[cls]
    if (rule === undefined) continue
    for (const k in rule) {
      if (props[k] === undefined) props[k] = rule[k]
    }
  }
}

// handlerID 是跨帧稳定的 handler 键(窗口 + 渲染路径 + 节点 + 事件):
// 树 JSON 稳定后 Go 侧的 no-op 去重才生效,多窗口之间也不会串键。
function handlerID(s, nodeID, ev) {
  return 'h:' + (s.inst.winId || '') + ':' + s.path + '#' + nodeID + ':' + ev
}

function renderEl(node, s) {
  const props = {}
  const dynClasses = []
  let events
  // 静态属性
  if (node.attrs !== undefined) {
    for (const k in node.attrs) props[k] = node.attrs[k]
  }
  // 动态绑定
  if (node.binds !== undefined) {
    for (const k in node.binds) {
      if (k.startsWith('class:')) {
        if (truthy(evalExpr(node.binds[k], s))) dynClasses.push(k.slice(6))
        continue
      }
      const v = evalExpr(node.binds[k], s)
      if (v !== undefined) props[k] = v
    }
  }
  const classes = (node.class || []).concat(dynClasses)
  applyStyle(props, classes, s.inst)
  // 事件(a-on)
  if (node.on !== undefined && node.on.length > 0) {
    events = {}
    for (const ev of node.on) {
      const hid = handlerID(s, node.id, ev.ev)
      handlers.set(hid, { inst: s.inst, action: ev.action, frames: s.frames, gen: renderGen })
      events[ev.ev] = hid
    }
  }
  // a-model:当前值 + 稳定绑定键 + 内部 input/change 动作(写回状态)
  if (node.model !== undefined) {
    const mv = evalExpr(node.model.target, s)
    if (node.model.kind === 'open') props.open = truthy(mv)
    else if (node.model.kind === 'bool') props.checked = truthy(mv)
    else if (node.model.kind === 'index') props.selected = mv === undefined || mv === null ? -1 : mv
    else props.value = mv === undefined || mv === null ? '' : mv
    const evName = node.model.kind === 'choice' || node.model.kind === 'bool' || node.model.kind === 'open' || node.model.kind === 'index' ? 'change' : 'input'
    const hid = handlerID(s, node.id, evName)
    handlers.set(hid, {
      inst: s.inst,
      frames: s.frames,
      gen: renderGen,
      action: {
        t: 'assign',
        l: node.model.target,
        r: { t: 'path', p: [{ key: '$event' }, { key: 'value' }] },
      },
    })
    if (events === undefined) events = {}
    events[evName] = hid
  }
  // 文本内容(Text/标签文本)
  if (node.parts !== undefined && node.parts.length > 0) {
    props.text = textOf(node.parts, s)
  }
  // 子节点
  let children
  if (node.children !== undefined && node.children.length > 0) {
    if (containerTags.has(node.tag)) {
      children = renderNodes(node.children, s)
    } else {
      // 标签文本(Text 的内容 / Button 等的 label):取 Text 子节点的 parts
      let label = ''
      for (const ch of node.children) {
        if (ch.parts !== undefined) label += textOf(ch.parts, s)
      }
      if (label !== '') {
        if (node.tag === 'Text') props.text = label
        else props.label = label
      }
    }
  }
  // model 绑定键
  let model
  if (node.model !== undefined) {
    model = modelKey(node.model.target, s)
  }
  return { id: s.path + '#' + node.id, tag: node.tag, props, events, model, children }
}

// renderComp:解析组件标签,挂载或复用子实例,产出子组件树。
function renderComp(node, s) {
  const compMap = s.inst.def.components || {}
  const def = compMap[node.name]
  if (def === undefined) {
    throw new Error('surface: 组件 ' + s.inst.name + ' 没有注册子组件 ' + node.name + '(script components 缺失)')
  }
  // 求值 props
  const propsObj = {}
  if (node.attrs !== undefined) {
    for (const k in node.attrs) propsObj[k] = node.attrs[k]
  }
  if (node.props !== undefined) {
    for (const k in node.props) {
      // undefined 也要写入:否则 prop 变成 undefined 时子实例会保留旧值
      // (例如 a-prop:selected="state.sel" 中 sel 被清空)。
      propsObj[k] = evalExpr(node.props[k], s)
    }
  }
  // 事件:登记到 handlers,父子 emit 关联
  const emitRegs = {}
  if (node.on !== undefined && node.on.length > 0) {
    for (const ev of node.on) {
      const hid = handlerID(s, node.id, ev.ev)
      handlers.set(hid, { inst: s.inst, action: ev.action, frames: s.frames, gen: renderGen })
      emitRegs[ev.ev] = hid
    }
  }
  // 实例复用:cacheKey = 父实例 + 渲染路径
  const cacheKey = s.path + '#' + node.id
  let childId = s.inst.children.get(cacheKey)
  let inst
  if (childId !== undefined) {
    inst = instances.get(childId)
    if (inst === undefined) {
      s.inst.children.delete(cacheKey)
      childId = undefined
    }
  }
  if (childId === undefined) {
    inst = makeInstance(node.name, reactive(propsObj))
    inst.winId = s.inst.winId // 子实例继承所属窗口(ctx.toast 等)
    s.inst.children.set(cacheKey, inst.id)
    runSetup(inst)
  } else {
    // props 更新:逐项写入响应式 props(比较前解包 raw,避免 Proxy 身份误判);
    // syncingProps 期间的写入不触发新渲染,见 trigger 的判断。
    syncingProps++
    try {
      for (const k in propsObj) {
        const nv = unwrapDeep(propsObj[k])
        if (!Object.is(unwrapDeep(inst.props[k]), nv)) inst.props[k] = nv
      }
      for (const k in inst.props) {
        if (!(k in propsObj)) delete inst.props[k]
      }
    } finally {
      syncingProps--
    }
  }
  inst.emitMap = {}
  for (const ev in emitRegs) inst.emitMap[ev] = { hid: emitRegs[ev] }
  // 渲染子树(子实例自己的 plan)
  const plan = plans.get(node.name)
  if (plan === undefined) {
    throw new Error('surface: 组件 ' + node.name + ' 没有 Plan(未编译注册)')
  }
  const visited = currentVisited
  visited.add(inst.id)
  // 错误边界:子组件 setup 失败 → 自己的错误卡片(与渲染失败区分)。
  if (inst.error !== undefined) {
    return {
      id: s.path + '#' + node.id + '-err',
      tag: 'Column',
      props: { gap: 4, padding: 8, background: '#2b1d22' },
      children: [
        { id: s.path + '#' + node.id + '-err-title', tag: 'Text', props: { text: '⚠ ' + node.name + ' 装配出错', bold: true, color: '#ff8a80' } },
        { id: s.path + '#' + node.id + '-err-msg', tag: 'Text', props: { text: String(inst.error), color: '#ffb4a9' } },
      ],
    }
  }
  try {
    const tree = renderPlanTree(plan.root, { inst, frames: [inst.scope, { props: inst.props }, inst.props], path: 'c/' + inst.id, keyChain: [] })
    if (childId === undefined) tree.siv = true // 新挂载的实例:让后端滚入视野
    return tree
  } catch (err) {
    // 错误边界:子组件渲染失败只降级为自己的错误占位,不拖垮整个窗口
    const msg = err && err.message ? err.message : String(err)
    __adatHost.log('surface: 子组件渲染失败(' + node.name + '): ' + msg)
    return {
      id: s.path + '#' + node.id + '-err',
      tag: 'Text',
      props: { text: '⚠ ' + node.name + ' 渲染出错: ' + msg, color: '#ff8a80' },
    }
  }
}

let currentVisited = null

function renderPlanTree(root, s) {
  const kids = renderNodes([root], s)
  if (kids.length !== 1) {
    throw new Error('surface: 组件 ' + s.inst.name + ' 根节点必须是单个元素')
  }
  return kids[0]
}

// errorTree:窗口级错误卡片(setup 失败 / 渲染抛错的降级呈现)。
function errorTree(name, message, phase) {
  return {
    id: 'err-' + name,
    tag: 'Column',
    props: { fill: true, center: true, gap: 8, padding: 16, background: '#2b1d22' },
    children: [
      { id: 'err-title', tag: 'Text', props: { text: '⚠ ' + phase + '出错', bold: true, 'font-size': 18, color: '#ff8a80' } },
      { id: 'err-comp', tag: 'Text', props: { text: '组件: ' + name, color: '#b9c6d8' } },
      { id: 'err-msg', tag: 'Text', props: { text: String(message), color: '#ffb4a9' } },
    ],
  }
}

// renderWindow:渲染一个窗口的完整树;返回 JSON 字符串。
// 顺带清扫本窗口不再被渲染的子实例与旧代 handler(if/for 消失的节点)。
// 错误边界:装配失败或渲染抛错都降级为错误卡片,窗口保持可用。
function renderWindow(winId) {
  const instId = windows.get(winId)
  if (instId === undefined) throw new Error('surface: 窗口未挂载: ' + winId)
  const inst = instances.get(instId)
  if (inst === undefined) throw new Error('surface: 窗口实例丢失: ' + winId)
  const plan = plans.get(inst.name)
  if (plan === undefined) throw new Error('surface: 组件 ' + inst.name + ' 没有 Plan')
  renderGen++
  renderCount++
  renderDepth++
  const visited = new Set([inst.id])
  currentVisited = visited
  let tree
  let rendered = false
  try {
    if (inst.error !== undefined) {
      tree = errorTree(inst.name, inst.error, '装配')
    } else {
      try {
        tree = renderPlanTree(plan.root, { inst, frames: [inst.scope, { props: inst.props }, inst.props], path: 'r', keyChain: [] })
        rendered = true
      } catch (err) {
        const msg = err && err.message ? err.message : String(err)
        __adatHost.log('surface: 渲染失败(' + inst.name + '): ' + msg)
        tree = errorTree(inst.name, msg, '渲染')
      } finally {
        currentVisited = null
      }
    }
    // 渲染成功才清扫未访问的子实例。渲染抛错时保留上一帧的子实例与内部状态,
    // 否则一次瞬时数据错误会把整窗子组件卸载、恢复后状态全丢。
    if (rendered) sweepChildren(inst, visited)
    // 清扫:只回收「本窗口的旧代 handler」——多窗口同轮渲染时,
    // 后渲染的窗口不能删掉先渲染窗口的 handler。
    for (const [hid, h] of Array.from(handlers.entries())) {
      if (h.gen < renderGen && h.inst !== undefined && h.inst.winId === winId) handlers.delete(hid)
    }
  } finally {
    renderDepth--
    if (renderDepth === 0) {
      // 渲染期收集到的依赖(props 同步):执行但不通知宿主,当前树已含新值。
      if (dirtyQueue.size > 0) flushEffects(false)
      // 渲染期发生的「非 props」状态写入(如子组件 onMount 改父状态):
      // 当前树可能已经过期,补一轮渲染。
      if (renderStateDirty) {
        renderStateDirty = false
        stateDirty = true
        scheduleFlush()
      }
    }
  }
  return JSON.stringify(tree)
}

// sweepChildren:递归卸载不在 visited 里的子实例。
function sweepChildren(inst, visited) {
  for (const [key, childId] of Array.from(inst.children.entries())) {
    if (!visited.has(childId)) {
      const c = instances.get(childId)
      inst.children.delete(key)
      if (c !== undefined) disposeInstance(c)
    } else {
      const c = instances.get(childId)
      if (c !== undefined) sweepChildren(c, visited)
    }
  }
}

// ============================================================
// 五、事件
// ============================================================

function runHandler(hid, payload) {
  const h = handlers.get(hid)
  if (h === undefined) {
    __adatHost.log('surface: handler 不存在: ' + hid)
    return false
  }
  const s = { inst: h.inst, frames: h.frames, keyChain: [], event: payload }
  if (payload !== null && payload !== undefined) {
    s.frames = h.frames.concat([{ $event: payload }])
  }
  evalAction(h.action, s)
  return true
}

function fireHandler(hid, payloadJSON) {
  let payload = null
  if (payloadJSON !== null && payloadJSON !== undefined && payloadJSON !== '') {
    payload = JSON.parse(payloadJSON)
  }
  try {
    return runHandler(hid, payload)
  } catch (err) {
    __adatHost.log('surface: 事件处理异常: ' + (err && err.message ? err.message : String(err)))
    return false
  }
}

// ============================================================
// 六、宿主入口(Go 调用)
// ============================================================

const PLAN_VERSION = 1

// registerComponent 注册一个组件:Plan JSON、样式令牌、i18n、脚本 default export。
// pluginID 可选:插件包的组件带上归属(能力校验与卸载按它定位)。
export function registerComponent(name, planJSON, styleJSON, i18nJSON, def, pluginID) {
  const plan = JSON.parse(planJSON)
  if (plan.planVersion !== PLAN_VERSION) {
    throw new Error('surface: 组件 ' + name + ' 的 Plan 版本 ' + plan.planVersion + ' 不支持(需要 ' + PLAN_VERSION + ')')
  }
  plans.set(name, plan)
  if (styleJSON !== null && styleJSON !== undefined && styleJSON !== '') {
    styles.set(name, JSON.parse(styleJSON))
  }
  if (i18nJSON !== null && i18nJSON !== undefined && i18nJSON !== '') {
    i18n.set(name, JSON.parse(i18nJSON))
  }
  components.set(name, def || {})
  if (pluginID !== undefined && pluginID !== null && pluginID !== '') {
    componentPlugins.set(String(name), String(pluginID))
  }
}

// bindPlugin 登记插件的能力白名单与组件归属(HMR 装载路径用:组件先
// 经 __adatHMRAdopt 注册,再一次性绑定插件身份)。
export function bindPlugin(pluginID, capsJSON, compsJSON) {
  const caps = JSON.parse(capsJSON || '[]')
  pluginScopes.set(String(pluginID), new Set(caps))
  const comps = JSON.parse(compsJSON || '[]')
  for (const name of comps) componentPlugins.set(String(name), String(pluginID))
}

// unbindPlugin 移除插件登记(组件本身由 unregisterComponents 注销)。
export function unbindPlugin(pluginID) {
  pluginScopes.delete(String(pluginID))
  for (const [name, pid] of Array.from(componentPlugins)) {
    if (pid === String(pluginID)) componentPlugins.delete(name)
  }
}

// unregisterComponents 注销一组组件:先 dispose 它们的全部实例(连带子
// 实例/订阅/handler),再删注册表。插件卸载时调用。
export function unregisterComponents(namesJSON) {
  const names = JSON.parse(namesJSON || '[]')
  for (const name of names) {
    for (const [, inst] of Array.from(instances)) {
      if (inst.name === name) disposeInstance(inst)
    }
    components.delete(name)
    plans.delete(name)
    styles.delete(name)
    i18n.delete(name)
    componentPlugins.delete(name)
  }
}

// forgetModules 删除 CJS 注册表里某前缀的模块(插件卸载后不再可 require)。
export function forgetModules(prefix) {
  const p = String(prefix)
  for (const key of Array.from(cjsCache.keys())) {
    if (key.startsWith(p)) cjsCache.delete(key)
  }
  for (const key of Array.from(requireMaps.keys())) {
    if (key.startsWith(p)) requireMaps.delete(key)
  }
}

// setTheme 设置宿主主题令牌(插件 ctx.theme() 与模板中的 theme.* 读取)。
export function setTheme(json) {
  themeTokens = json === null || json === undefined || json === ''
    ? {}
    : JSON.parse(json)
}

// serviceCapabilities 返回已暴露的服务方法(service.method),供宿主
// 生成插件能力白名单文档 / 校验插件清单。
export function serviceCapabilities() {
  const names = []
  for (const [svc, methods] of services) {
    for (const m of Object.keys(methods)) {
      if (m.endsWith('!async')) continue
      names.push(svc + '.' + m)
    }
  }
  return JSON.stringify(names)
}

// setLocale 设置当前语言(zh-CN 等)。
export function setLocale(locale) {
  currentLocale = String(locale || 'zh-CN')
}

// setAppI18n 注册应用级文案('__app' 命名空间,组件查表 miss 后回退)。
export function setAppI18n(json) {
  if (json !== null && json !== undefined && json !== '') {
    i18n.set('__app', JSON.parse(json))
  }
}

// registerService 注册一个 Go 服务方法(Go 经 rt.Expose 注入原生函数):
// syncFn 为同步调用(api.call),asyncFn 返回 Promise(api.callAsync)。
export function registerService(name, method, syncFn, asyncFn) {
  let svc = services.get(String(name))
  if (svc === undefined) services.set(String(name), (svc = {}))
  svc[String(method)] = syncFn
  if (typeof asyncFn === 'function') {
    svc[String(method) + '!async'] = asyncFn
  }
}

// fireHostEvent 把 Go 侧 Notify 的广播派发给订阅的组件(ctx.on)。
export function fireHostEvent(event, payloadJSON) {
  const payload = payloadJSON !== '' && payloadJSON !== undefined && payloadJSON !== null
    ? JSON.parse(payloadJSON)
    : null
  const hs = eventHandlers.get(String(event))
  if (hs === undefined) return
  for (const h of Array.from(hs)) {
    try {
      h.fn(payload)
    } catch (err) {
      __adatHost.log('surface: 事件处理异常: ' + (err && err.message ? err.message : String(err)))
    }
  }
}

// mountWindow 挂载窗口根组件;返回实例 id。
export function mountWindow(winId, name, propsJSON) {
  let props = {}
  if (propsJSON !== null && propsJSON !== undefined && propsJSON !== '') {
    props = JSON.parse(propsJSON)
  }
  const inst = makeInstance(name, reactive(props))
  inst.winId = String(winId)
  windows.set(winId, inst.id)
  runSetup(inst)
  return inst.id
}

// unmountWindow 卸载窗口:移除注册并 dispose 根实例(连带子实例、
// 事件订阅与该实例产生的 handler),保证挂载/卸载循环不泄漏。
export function unmountWindow(winId) {
  const instId = windows.get(winId)
  if (instId === undefined) return
  windows.delete(winId)
  const inst = instances.get(instId)
  if (inst !== undefined) disposeInstance(inst)
}

// stats:注册表计数(泄漏测试与 Inspector 用)。
export function stats() {
  let subs = 0
  for (const hs of eventHandlers.values()) subs += hs.size
  return JSON.stringify({
    instances: instances.size,
    handlers: handlers.size,
    eventSubs: subs,
    windows: windows.size,
    plugins: pluginScopes.size,
    seq,
    renders: renderCount,
  })
}

// 组件脚本从这里 import(本模块即 ada:surface)。
export { reactive, ref, computed, watch, effect, onMount, onUnmount }

// Go 侧经 globalThis 调用(合并模块图里 Hook 解析不可靠,global 最稳)。
globalThis.__adatSurface = {
  registerComponent,
  mountWindow,
  unmountWindow,
  renderWindow,
  fireHandler,
  setLocale,
  setAppI18n,
  registerService,
  fireHostEvent,
  stats,
  bindPlugin,
  unbindPlugin,
  unregisterComponents,
  forgetModules,
  setTheme,
  serviceCapabilities,
}

// ============================================================
// 七、HMR(热重载):CJS 注册表 + require 垫片
// ============================================================
//
// 初始装配走 ESM 模块图;热重载的增量代码经 RunScript 以经典脚本进入:
// Go 把变更模块降级为 CJS(imports 变 require),配合这里的垫片装载。
//
// 语义(对齐 Vue HMR 的常用子集):
//   - 模板/样式/i18n 变更 → __adatHMRData 只换数据,实例与状态完整保留
//   - 脚本变更 → __adatHMRAdopt 换 def 并重跑 setup(props 保留,组件内部状态重置)

const cjsCache = new Map()     // 模块名 → { exports }
const requireMaps = new Map()  // 模块名 → { spec → 规范名 }(Go 预解析的相对导入)

// __adatRequireMap 登记一个模块的相对导入映射(Go 侧已按词表解析)。
globalThis.__adatRequireMap = function (from, mapJSON) {
  requireMaps.set(from, JSON.parse(mapJSON))
}

function makeRequire(from) {
  const specMap = requireMaps.get(from)
  return function (spec) {
    if (spec === 'ada:surface') return globalThis.__adatSurfaceAPI
    const target = specMap && specMap[spec] ? specMap[spec] : spec
    const mod = cjsCache.get(target)
    if (mod === undefined) {
      throw new Error('surface: HMR require 未装载: ' + spec + '(来自 ' + from + ')')
    }
    return getCJS(target).exports
  }
}

// __adatDefine 登记 CJS 工厂(惰性执行:首次 require/取导出才运行,
// 支持任意定义顺序与循环引用)。
globalThis.__adatDefine = function (name, factory) {
  cjsCache.set(name, { factory: factory, exports: undefined })
}

function getCJS(name) {
  const mod = cjsCache.get(name)
  if (mod === undefined) {
    throw new Error('surface: CJS 模块未装载: ' + name)
  }
  if (mod.exports === undefined) {
    mod.exports = {}
    mod.factory(mod, mod.exports, makeRequire(name))
  }
  return mod
}

globalThis.__adatGetExports = function (name) {
  return getCJS(name).exports
}

// __adatHMRAdopt:脚本变更的组件——换 def 并对存活实例重跑 setup
// (props 保留;组件内部状态重置;旧清理函数/事件订阅先撤销)。
globalThis.__adatHMRAdopt = function (name, planJSON, styleJSON, i18nJSON, def) {
  registerComponent(name, planJSON, styleJSON, i18nJSON, def)
  let dbgCount = 0
  for (const [, inst] of Array.from(instances)) {
    if (inst.name !== name) continue
    dbgCount++
    inst.def = def // 替换实例的 def(registerComponent 只更新注册表)
    for (const fn of inst.cleanups) {
      try { fn() } catch (err) { __adatHost.log('surface: HMR cleanup 异常: ' + String(err)) }
    }
    for (const fn of inst.unmounts) {
      try { fn() } catch (err) { __adatHost.log('surface: HMR unmount 异常: ' + String(err)) }
    }
    for (const reg of inst.eventRegs) {
      const hs = eventHandlers.get(reg.key)
      if (hs !== undefined) {
        hs.delete(reg.entry)
        if (hs.size === 0) eventHandlers.delete(reg.key)
      }
    }
    inst.cleanups.length = 0
    inst.unmounts.length = 0
    inst.eventRegs.length = 0
    inst.mounts.length = 0
    inst.emitMap = {}
    inst.error = undefined
    runSetup(inst)
  }
}

// __adatHMRData:仅模板/样式/i18n 变更——只换数据,实例与状态原封不动。
globalThis.__adatHMRData = function (name, planJSON, styleJSON, i18nJSON) {
  const old = components.get(String(name))
  if (old === undefined) {
    throw new Error('surface: HMR 目标组件未注册: ' + name)
  }
  registerComponent(name, planJSON, styleJSON, i18nJSON, old)
}

// __adatSurfaceAPI:require('ada:surface') 返回的组件 API 面。
globalThis.__adatSurfaceAPI = {
  reactive,
  ref,
  computed,
  watch,
  effect,
  onMount,
  onUnmount,
}
