package bot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"ada_chat/internal/rpc"
)

// endpoint is where a workspace process listens and the token both sides use.
type endpoint struct {
	id    int
	addr  rpc.Address
	token []byte
	dir   string
}

func newEndpoint(instanceID int) (endpoint, error) {
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return endpoint{}, fmt.Errorf("生成工作区 token 失败: %w", err)
	}
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		return endpoint{}, fmt.Errorf("生成工作区地址失败: %w", err)
	}
	suffix := hex.EncodeToString(nonce)
	ep := endpoint{id: instanceID, token: []byte(hex.EncodeToString(tok))}
	switch runtime.GOOS {
	case "windows":
		ep.addr = rpc.Address{Network: rpc.Pipe, Name: fmt.Sprintf("ada-ws-%d-%s", instanceID, suffix)}
	default:
		dir, err := os.MkdirTemp("", "ada-ws-*")
		if err != nil {
			return endpoint{}, fmt.Errorf("创建工作区 socket 目录失败: %w", err)
		}
		ep.dir = dir
		ep.addr = rpc.Address{Network: rpc.Unix, Name: filepath.Join(dir, "rpc.sock")}
	}
	return ep, nil
}

func (e endpoint) cleanup() {
	if e.dir != "" {
		_ = os.RemoveAll(e.dir)
	}
}

func (e endpoint) env() []string {
	return []string{
		EnvAddr + "=" + e.addr.String(),
		EnvToken + "=" + string(e.token),
	}
}
