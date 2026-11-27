# 环境变量文件(.env)

> 一句话:把 API key、endpoint 这类**本机配置**放进一个不进版本库的文件,
> 启动时补进进程环境;真实环境变量永远优先。实现:`platform/denv`。

## 1. 为什么需要

现状是 `secrets-env` 读 `ADA_SECRET_<name>`、`llm-openai` 读它的 `apiKeyEnv`
(默认 `ADA_LLM_API_KEY`)。它们都只看**进程环境**,于是每次试用都要先设一串环境变量,
或者把 key 写进 preset/config —— 后者违背 AGENTS 红线 13(secret 不进 config/日志)。

`.env` 提供一个本机文件入口:**文件放值,环境里只用名字**,preset/config 保持干净。

## 2. 加载位置与优先级

| 程序 | 读取的文件 | 说明 |
| --- | --- | --- |
| `ada`(CLI,含 `ada acp`) | `./.env` | 相对当前工作目录;在发布包根运行即为包根文件 |
| `ADA_ENV_FILE` | 指向的文件 | 设置后**只**读它(CI/测试用临时环境) |

优先级(高 → 低):

1. 真实环境变量(命令行、CI、宿主传入)——`.env` 不会覆盖已存在的键;
2. 后加载的文件覆盖先加载的文件(`denv.Apply` 支持多文件叠加);
3. 文件里的值。

CLI 只读 cwd 的 `.env`:发布包里请从包根启动 `ada acp`;桌面壳(ada_chat)启动 agent 时
把工作目录设成用户项目,工作区是被 agent 读写的不可信输入,不能让仓库里的 `.env` 改写
模型端点或密钥 —— 桌面场景应当用 `seam/secrets` 或机器人环境变量提供密钥。

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
- 桌面壳:ada_chat 启动的 `ada acp` 用**机器人环境变量**或 `seam/secrets` 拿 key,
  不读工作区里的 `.env`(工作区是 agent 读写范围,属不可信输入);
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
- **CLI 只读自己的 cwd**:见 §2——发布包里从包根启动;桌面壳走机器人环境变量,
  不让用户工作区里的文件改写模型端点。

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
