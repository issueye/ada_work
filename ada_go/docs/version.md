# 版本与引擎约束(version)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §9.2(清单)、§12.4(bundle)、§17(里程碑)。

## 1. 范围

```
platform/version/   semver 解析、比较与约束(`>= > <= < = !=`)
platform/plugin/    清单新增 engines(只认 "ada")
platform/bundle/    bundle 新增 engine;ExpandWithOptions 传入宿主版本做校验
```

## 2. 语法

- 版本号:`2.0.0`、`v2.0.0`、`2.0.0-beta.1`(必须三段,预发布段按 semver 排序);
- 约束:`>=2.0.0`、`>2.0.0`、`<=2.0.0`、`<2.0.0`、`=2.0.0`、`==2.0.0`、`!=2.0.0`;
  裸版本等价于 `=`;
- **不支持** `^`/`~`、范围并集(`||`)、通配与多约束组合——出现即报错,不做「尽量理解」。

## 3. 使用

清单(`plugin.json`):

```json
{ "id": "p", "version": "0.1.0", "kind": "go", "engines": { "ada": ">=2.0.0" } }
```

bundle(`ada.bundle.json`):

```json
{ "name": "base", "engine": ">=2.0.0", "entries": [ ... ] }
```

展开时传入宿主版本:

```go
manifests, warnings, err := p.ExpandWithOptions(bundle.ExpandOptions{HostVersion: "2.1.0"})
```

- 清单 `engines` 与 bundle `engine` 都在展开期校验;
- `HostVersion` 为空时**跳过版本校验**(离线/测试场景),生产宿主应提供;
- 校验失败是可读错误(包含约束与宿主版本)。

## 4. 偏差与边界

1. **没有范围组合**:`^`/`~`/`||`/通配一律拒绝;需要时由调用方拆成多条约束分别判断。
2. **宿主版本由调用方提供**:没有从构建信息自动推导(`ada version` 命令未实现)。
3. **清单只认 `ada`**:其它 engine 名(如 `node`)会在校验期报错;跨生态约束尚未设计。
4. **预发布段比较是简化 semver**:数字段与字符串段按 semver 规则排序,但没有「预发布只匹配同段」的语义细节。
5. 安装(install)尚未校验 engines;只在组合展开期生效。

## 5. 测试矩阵

| 场景 | 用例 |
|---|---|
| 解析/比较(含预发布、v 前缀) | `version.TestParseAndCompare` |
| 约束运算符与非法形式 | `version.TestConstraints` |
| Satisfy 的可读错误 | `version.TestSatisfy` |
| 清单 engines 校验 | `plugin.TestEngines` |
| bundle engine + 插件 engines + HostVersion 为空 | `bundle.TestEngineChecks` |
| 非法 engine 解析期拒绝 | `bundle.TestEngineInvalid` |

## 6. 下一步

1. `ada version`/构建信息注入宿主版本。
2. install 阶段校验 engines(与 trust/签名一起)。
3. 约束组合(多约束 AND)与 `^`/`~` 的显式等价展开。
4. 清单里其它 engine(如 `moejs`)的约束命名空间。

## 7. 验证命令

```powershell
go test -race -count=1 ./version ./plugin ./bundle
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```