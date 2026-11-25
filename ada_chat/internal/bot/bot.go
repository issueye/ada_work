// Package bot owns the host side of the bot process lifecycle: a workspace
// process per working directory, one RPC peer for every bot that shares it.
// The agent runtime itself is an external executable supplied by the host.
package bot

// Env names shared with the runtime process. The token never appears on the
// command line; the host passes it through the environment.
const (
	EnvAddr        = "ADA_BOT_ADDR"
	EnvToken       = "ADA_BOT_TOKEN"
	EnvName        = "ADA_BOT_NAME"
	EnvModel       = "ADA_BOT_MODEL"
	EnvInstruction = "ADA_BOT_INSTRUCTION"
	EnvWorkspace   = "ADA_BOT_WORKSPACE"
)

// ClientName is the only peer the workspace process accepts.
const ClientName = "ada-chat"
