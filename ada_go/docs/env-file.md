# 环境变量文件(.env)

> 一句话:把 API key、endpoint 这类**本机配置**放进一个不进版本库的文件,
> 启动时补进进程环境;真实环境变量永远优先。实现:`platform/denv`。

## 1. 为什么需要

现状是 `secrets-env` 读 `ADA_SECRET_<name>`、`llm-openai` 读它的 `apiKeyEnv`
(默认 `ADA_LLM_API_KEY`)、`ada-rpc-agent` 读 `ADA_BOT_*` / `ADA_LLM_*`。
它们都只看**进程环境**,于是每次试用都要先设一串环境变量,或者把 key 写进
preset/config —— 后者违背 AGENTS 红线 13(secret 不进 config/日志)。

`.env` 提供一个本机文件入口:**文件放值,环境里只用名字**,preset/config 保持干净。

## 2. 加载位置与优先级

| 程序 | 读取的文件 | 说明 |
| --- | --- | --- |
| `ada`(CLI) | `./.env` | 相对当前工作目录;在发布包根运行即为包根文件 |
| `ada-rpc-agent` | 可执行文件旁 `.env`,再读**它的父目录** `.env` | 发布包布局 `<root>/bin/ada-rpc-agent.exe` ⇒ `bin/.env` 与 `root/.env`;**不读 cwd** |
| 两者 | `ADA_ENV_FILE` 指向的文件 | 设置后**只**读它(CI/测试用临时环境) |

优先级(高 → 低):

1. 真实环境变量(命令行、CI、宿主传入)——`.env` 不会覆盖已存在的键;
2. 后加载的文件覆盖先加载的文件(目前两个入口各自只读一个文件,`denv.Apply` 支持多文件叠加);
3. 文件里的值。

`ada-rpc-agent` 只读 exe 旁的 `.env` 是**有意为之**:它的 cwd 是用户选的机器人工作区,
工作区是被 agent 读写的不可信输入,不能让仓库里的 `.env` 改写模型端点或密钥。

## 3. 语法

dotenv 的常用子集(不是完整实现,不支持变量插值/多行值):

```dotenv
# 整行注释
ADA_LLM_API_KEY=sk-xxx
export ADA_LLM_MODEL=ada-1
ADA_LLM_ENDPOINT="https://api.example.com/v1"
QUOTED='原样保留 # 与空格'
ESCAPED="line1\nline2\t\"quoted\""
EMPTY=
INLINE=value # 行内注释
URL=https://example.com/a#frag   # 没有空白的 # 属于值
```

- 键名:`[A-Za-z_][A-Za-z0-9_]*`;
- 双引号支持 `\n` `\r` `\t` `\"` `\\`;单引号原样保留;
- 无引号值:` #` 起算行内注释;
- **坏行直接报错并给行号**(`ada: denv: .env: 第 2 行缺少 '='`),不静默跳过——
  拼错的键被忽略比启动失败更难排查。

## 4. 典型用法

`dist/ada_go/.env`:

```dotenv
# OpenAI 兼容端点(或留空走 preset 的 replay)
ADA_LLM_API_KEY=sk-...
ADA_LLM_ENDPOINT=https://api.deepseek.com
ADA_LLM_MODEL=deepseek-chat

# http_search 的 DeepSeek 网页搜索 key(还需在 preset 里启用 seam-secrets + secrets-env,
# 并把 DEEPSEEK_API_KEY 放进 secrets-env 的 allow 名单)
ADA_SECRET_DEEPSEEK_API_KEY=sk-...
```

- CLI 直接生效:`ada agent --preset presets/agent-release.preset.json -message "..."`;
- 桌面壳:它启动的 `ada-rpc-agent` 从自己的 exe 旁读同一份 `.env`,模型 key 自动可见;
- 想换一份环境:设 `ADA_ENV_FILE=/path/to/other.env`(只读它,不再读默认文件)。

## 5. 安全边界

- **不进版本库**:`.gitignore` 已忽略 `.env`(保留 `.env.example`);
  POSIX 上建议 `chmod 600`,Windows 上至少别放在共享目录。
- **不写日志**:启动只打印「已从环境变量文件加载 N 项」,**不打印键值**。
- **仍然是进程环境**:加载后变量对所有插件与子进程可见,`shell-helper` 起的命令会继承它们。
  这与「在 shell 里 export」是同一类暴露面;确实敏感的值应走 `seam/secrets` 的按名读取,
  而不是 `.env`。
- **授权没有被绕过**:`secrets-env` 的 `allow` 白名单与 `secrets-guard` 的
  `secrets.<name>` 授权仍照旧生效;`.env` 只是让 `ADA_SECRET_*` 有地方放。
- **不读工作区**:见 §2——只有 CLI 读 cwd,`ada-rpc-agent` 只读可执行文件旁与它的父目录(包根)。

## 6. 测试与验证

```powershell
go test ./platform/denv -v     # 语法面、错误面、真实环境优先、文件叠加
```

端到端(临时目录里放 `.env`):

```powershell
# 有效文件 → 启动提示「加载 N 项」,命令照常
# 坏文件   → 退出码 2,报「第 N 行 ...」
# 缺文件   → 不提示、不报错,行为与没有 .env 一致
```
