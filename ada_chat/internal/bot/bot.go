// Package bot 拥有机器人进程生命周期的宿主侧:每个工作目录一个 ACP agent 进程,
// 该目录下的所有机器人共用它。agent 运行时是宿主提供的外部可执行文件。
package bot

// Env names shared with the runtime process. 它是宿主告诉 agent「你是谁、在哪个
// 工作目录」的身份契约;供应商与 ACP 参数另由设置写入。
const (
	EnvName        = "ADA_BOT_NAME"
	EnvModel       = "ADA_BOT_MODEL"
	EnvInstruction = "ADA_BOT_INSTRUCTION"
	EnvWorkspace   = "ADA_BOT_WORKSPACE"
)
