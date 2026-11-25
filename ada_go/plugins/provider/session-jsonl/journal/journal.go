// Package journal 定义 session-jsonl 的持久化抽象。
//
// 把 Store/Journal 单独成包,是为了让文件后端(file)与 provider/actor 解耦:
// 以后换 SQLite、bbolt 或内存实现时,只需要替换 file 包,Provider 与 Session 不变。
package journal

import (
	"context"

	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// ProviderName 同时是 provider 名与插件 id。
const ProviderName = "session-jsonl"

// Journal 是「单个会话日志」的持久化抽象。
//
// 实现必须保证:
//   - Append 分配连续 seq,并补全 TS/SessionID/Source;
//   - 一次 Append 要么全部可见,要么全部不可见(至少不能返回一半 seq);
//   - ReadAll 返回已持久化的事件副本;
//   - Compact 只追加摘要,不重写历史。
//
// 同一 Journal 只应由一个 actor 写;读可以由多个 goroutine 并发。
type Journal interface {
	ID() string
	Meta() session.Meta
	Append(events []session.Event) ([]session.Event, error)
	ReadAll() ([]session.Event, error)
	Compact(ctx context.Context, upTo uint64, summary session.Event) (session.Event, error)
	Close() error
}

// Store 是会话日志后端的工厂抽象。
//
// Open 在会话不存在时创建,存在时加载;Create 永远创建新会话;
// Fork 用给定事件前缀创建子会话。ID 必须由调用方保证合法。
type Store interface {
	Open(id string, meta session.Meta) (Journal, session.Meta, error)
	Create(id string, meta session.Meta) (Journal, error)
	Fork(events []session.Event, id string, meta session.Meta) (Journal, error)
}
