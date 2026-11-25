# M0 spike 报告

> 时间：2026-11-19。方式：临时 Go 模块（`$TEMP/adaspike`、`$TEMP/adaM0`），不写入本仓库；所有数字为本机实测（Go 1.27.1 / windows-amd64 / `CGO_ENABLED=0` / moejs `v0.1.0-alpha.11` / esbuild `v0.28.2`）。

## 结论

12 个 spike 中 5 个已通过，M0 gate 关闭：

| # | 验证 | 结果 | 数据 |
|---|---|---|---|
| S1 | esbuild 降级 + moejs 多模块 `Link` | ✅ | TS `enum` + 构造器参数属性 + 相对导入，调用返回 `{"value":42,"level":5,"name":"x"}` |
| S2 | 重载无泄漏 | ✅ | 1000 次 create+load+call+stop：goroutine 0 泄漏；堆增量 358 KB 且不随轮次增长（100 轮 344 KB）；单次平均 11.4 µs |
| S6 | 共享 `*Module` 池化并发 | ✅ | 12 个 Runtime 各 200 次调用，0 错误 |
| S8 | 增量编译 | ✅ | 20 文件全量 13.3 ms；改 1 文件（transform+compile+link+load+call）平均 0.66 ms |
| S9 | ESM 循环依赖 | ✅ | 双向 import，`Link` 与调用正常 |

## 尚未验证（不阻塞 M1，阻塞各自里程碑）

| # | 验证 | 阻塞 | 计划 |
|---|---|---|---|
| S3 | Go + JS 监听器混跑 waterfall（取消/超时） | M2（jsrt） | M2 开工前 |
| S4 | 服务 epoch 级联 | Go 侧已覆盖(M1 测试:pending→active、级联重载、失败复活);JS 侧待 M2 | M2 补 JS 场景 |
| S5 | 内存上限语义（常驻 vs 单次执行） | M2 | 与 S3 同批 |
| S7 | sourcemap 反查准确率 | M2 | 与 S3 同批 |
| S10 | 闲置回收后再激活 | M4 | M4 开工前 |
| S11 | 拒绝矩阵 + 审计 | M5 | M5 开工前 |
| S12 | Windows 路径沙箱 | M5 | M5 开工前 |
| S13 | UI 运行时 harness（moejs 直跑 + OpAdapter；不可行则 chromedp，需新依赖 ADR） | U0（[ui-framework.md](ui-framework.md) §14） | U0 第 1 周 |

## 对方案的修正

1. **S8 快得超出预期**（0.66 ms，目标 200 ms），因此 `ada dev` 不需要复杂的增量编译缓存，**每次全量重编译 + 重载**即可（20 文件全量也只要 13.3 ms）。Artifact 缓存仍然要做，但目的是跨进程冷启动，不是开发期。
2. **S2 的单次开销 11.4 µs** 说明「重载 = 新 Runtime」的代价完全可接受；无需保留旧 Runtime 做热切换。
3. **S2 的 358 KB 堆增量不随轮次增长**说明 moejs 的 Runtime/Loop 能被正常回收；内核不需要自建 Runtime 池，按需创建即可（池化仅用于 `pool > 1` 的高并发插件）。

## 复现方式

两个 spike 的源码都在临时目录，未提交。若需要长期回归，按 [seam-api.md 附录 C](seam-api.md) 的 C6 方式转成 `testdata/spikes/` 下的基准测试（M2 起）。

## 对 M0 的判定

M0 的交付物是「spike 报告 + 一页 API 摘要」：本文件 + [kernel-api.md](kernel-api.md)。gate 已关闭，进入 M1。
