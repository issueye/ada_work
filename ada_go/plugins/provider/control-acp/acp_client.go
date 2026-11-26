package controlacp

import (
	"context"

	acpseam "github.com/issueye/ada_go_v2/plugins/seam/acp"
)

// Capabilities 返回 client 在 initialize 里声明的反向能力。
//
// 连接建立前(或 client 没声明)全部为 false;调用方据此决定是否使用
// 编辑器文件/终端,而不是假设它们存在。
func (e *Endpoint) Capabilities() acpseam.Capabilities {
	if e == nil {
		return acpseam.Capabilities{}
	}
	e.capMu.RLock()
	defer e.capMu.RUnlock()
	return e.caps
}

func (e *Endpoint) setCapabilities(c *clientCapabilities) {
	var caps acpseam.Capabilities
	if c != nil {
		caps.ReadTextFile = c.FS.ReadTextFile
		caps.WriteTextFile = c.FS.WriteTextFile
		caps.Terminal = c.Terminal
	}
	e.capMu.Lock()
	e.caps = caps
	e.capMu.Unlock()
}

func (e *Endpoint) ensureCapability(ok bool, name string) error {
	if e == nil || e.conn == nil {
		return acpseam.Unavailable("ACP 连接不可用")
	}
	if !ok {
		return acpseam.Unimplemented("ACP client 未声明 " + name)
	}
	return nil
}

// ReadTextFile 调用 client 的 fs/read_text_file;line/limit <= 0 表示不传。
func (e *Endpoint) ReadTextFile(ctx context.Context, sessionID, path string, line, limit int) (string, error) {
	if err := e.ensureCapability(e.Capabilities().ReadTextFile, "fs.readTextFile"); err != nil {
		return "", err
	}
	if sessionID == "" || path == "" {
		return "", acpseam.Invalid("fs/read_text_file 需要 sessionId 与 path")
	}
	params := map[string]any{"sessionId": sessionID, "path": path}
	if line > 0 {
		params["line"] = line
	}
	if limit > 0 {
		params["limit"] = limit
	}
	var res struct {
		Content string `json:"content"`
	}
	if err := e.conn.Call(ctx, "fs/read_text_file", params, &res); err != nil {
		return "", acpseam.Unavailable("fs/read_text_file 失败: " + err.Error())
	}
	return res.Content, nil
}

// WriteTextFile 调用 client 的 fs/write_text_file。
func (e *Endpoint) WriteTextFile(ctx context.Context, sessionID, path, content string) error {
	if err := e.ensureCapability(e.Capabilities().WriteTextFile, "fs.writeTextFile"); err != nil {
		return err
	}
	if sessionID == "" || path == "" {
		return acpseam.Invalid("fs/write_text_file 需要 sessionId 与 path")
	}
	params := map[string]any{"sessionId": sessionID, "path": path, "content": content}
	if err := e.conn.Call(ctx, "fs/write_text_file", params, nil); err != nil {
		return acpseam.Unavailable("fs/write_text_file 失败: " + err.Error())
	}
	return nil
}

// CreateTerminal 调用 client 的 terminal/create,返回 terminalId。
func (e *Endpoint) CreateTerminal(ctx context.Context, sessionID string, req acpseam.TerminalRequest) (string, error) {
	if err := e.ensureCapability(e.Capabilities().Terminal, "terminal"); err != nil {
		return "", err
	}
	if sessionID == "" || req.Command == "" {
		return "", acpseam.Invalid("terminal/create 需要 sessionId 与 command")
	}
	params := map[string]any{"sessionId": sessionID, "command": req.Command}
	if len(req.Args) > 0 {
		params["args"] = req.Args
	}
	if len(req.Env) > 0 {
		params["env"] = req.Env
	}
	if req.Cwd != "" {
		params["cwd"] = req.Cwd
	}
	if req.OutputByteLimit > 0 {
		params["outputByteLimit"] = req.OutputByteLimit
	}
	var res struct {
		TerminalID string `json:"terminalId"`
	}
	if err := e.conn.Call(ctx, "terminal/create", params, &res); err != nil {
		return "", acpseam.Unavailable("terminal/create 失败: " + err.Error())
	}
	if res.TerminalID == "" {
		return "", acpseam.Unavailable("terminal/create 没有返回 terminalId")
	}
	return res.TerminalID, nil
}

// terminalQuery 校验能力与 id 后发一次 terminal 请求并解码结果。
func (e *Endpoint) terminalQuery(ctx context.Context, sessionID, terminalID, method string, out any) error {
	if err := e.ensureCapability(e.Capabilities().Terminal, "terminal"); err != nil {
		return err
	}
	if sessionID == "" || terminalID == "" {
		return acpseam.Invalid(method + " 需要 sessionId 与 terminalId")
	}
	if err := e.conn.Call(ctx, method, map[string]any{"sessionId": sessionID, "terminalId": terminalID}, out); err != nil {
		return acpseam.Unavailable(method + " 失败: " + err.Error())
	}
	return nil
}

// TerminalOutput 调用 client 的 terminal/output。
func (e *Endpoint) TerminalOutput(ctx context.Context, sessionID, terminalID string) (acpseam.TerminalOutput, error) {
	var out acpseam.TerminalOutput
	err := e.terminalQuery(ctx, sessionID, terminalID, "terminal/output", &out)
	return out, err
}

// WaitForExit 调用 client 的 terminal/wait_for_exit。
func (e *Endpoint) WaitForExit(ctx context.Context, sessionID, terminalID string) (acpseam.ExitStatus, error) {
	var out acpseam.ExitStatus
	err := e.terminalQuery(ctx, sessionID, terminalID, "terminal/wait_for_exit", &out)
	return out, err
}

// KillTerminal 调用 client 的 terminal/kill(不释放终端)。
func (e *Endpoint) KillTerminal(ctx context.Context, sessionID, terminalID string) error {
	return e.terminalQuery(ctx, sessionID, terminalID, "terminal/kill", nil)
}

// ReleaseTerminal 调用 client 的 terminal/release。
func (e *Endpoint) ReleaseTerminal(ctx context.Context, sessionID, terminalID string) error {
	return e.terminalQuery(ctx, sessionID, terminalID, "terminal/release", nil)
}

// 编译期确认 Endpoint 实现 seam/acp.Client。
var _ acpseam.Client = (*Endpoint)(nil)
