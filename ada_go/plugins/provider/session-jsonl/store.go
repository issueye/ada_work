// Package jsonl 是 session seam 的文件型 provider。
//
// 存储模型:一个会话一个 JSONL 文件,每行一条事件;会话之间不共享写锁,
// 同一会话由 Session 的 actor 串行落盘(见 session.go)。
//
// 持久化接口在 journal 子包,文件实现放在 file 子包;以后换 SQLite、bbolt
// 或内存后端时,Provider 与 Session 的并发语义不变。
package jsonl

import (
	"github.com/issueye/ada_go_v2/plugins/provider/session-jsonl/file"
	"github.com/issueye/ada_go_v2/plugins/provider/session-jsonl/journal"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

const (
	// ProviderName 同时是 provider 名与插件 id。
	ProviderName = journal.ProviderName
)

// Journal 是单个会话日志的持久化抽象(见 journal 子包)。
type Journal = journal.Journal

// Store 是会话日志后端的抽象(见 journal 子包)。
type Store = journal.Store

// NewFileStore 构造「一个会话一个 JSONL 文件」的后端。
func NewFileStore(dir string, syncWrites bool) Store {
	return file.NewStore(dir, syncWrites)
}

// errClosed 是会话/日志关闭后的统一错误。
func errClosed() error {
	return seamerr.New(seamerr.ErrCanceled, ProviderName, "会话已关闭", false)
}
