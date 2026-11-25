# ada_work

四个独立 Go 模块的单体工作区：同一个仓库，各自保留 `go.mod`。

| 目录 | 模块 | 说明 |
| --- | --- | --- |
| [ada_go](ada_go) | `github.com/issueye/ada_go_v2` | 插件内核与 agent 平台 |
| [ada_jsrt](ada_jsrt) | `github.com/issueye/ada_jsrt` | 执行层：esbuild 把 TS 降级为 JS，moejs 在进程内运行；被 `ada_go` 以 `replace ../ada_jsrt` 引用 |
| [ada_t](ada_t) | `github.com/issueye/ada_t` | 运行时模板/脚本编译与原生控件 UI |
| [ada_chat](ada_chat) | `ada_chat` | 桌面聊天客户端，依赖 `ada_t` |
| `dist/` | — | 发布打包产物，不入库 |

## 验证

```powershell
cd ada_jsrt; go test ./...
cd ../ada_go; go test ./...
```

`ada_go` 依赖同级的 `ada_jsrt`：检出时必须两个目录一起存在，否则 `go build` 会因 `replace ../ada_jsrt` 找不到模块而失败。
