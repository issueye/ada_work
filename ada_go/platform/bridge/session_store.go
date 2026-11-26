package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// 会话追加与 store 的边界。超出时明确报错,不静默截断。
const (
	maxSessionKindBytes = 64
	maxSessionDataBytes = 64 << 10
	maxStoreKeyBytes    = 128
	maxStoreValueBytes  = 64 << 10
	maxStoreEntries     = 1000
)

// sessionCurrent 实现 ada:session.current():返回会话 JSON;没有会话时返回 "null"。
func sessionCurrent(ctx context.Context) (string, error) {
	sess, ok := session.FromContext(ctx)
	if !ok || sess == nil {
		return "null", nil
	}
	meta := sess.Meta()
	b, err := json.Marshal(map[string]string{
		"id": sess.ID(), "title": meta.Title, "workspace": meta.Workspace,
	})
	if err != nil {
		return "", fmt.Errorf("ada:session.current 编码失败: %w", err)
	}
	return string(b), nil
}

// sessionAppend 实现 ada:session.append():只允许写插件自有事件。
//
// 事件类型固定为 plugin:<插件 id>/<kind>,并强制 Ignorable=true:
// TS 插件不能伪造 user/assistant/tool 这些模型可见事件。
func sessionAppend(plugin string, ctx context.Context, kind, dataJSON string) (uint64, error) {
	sess, ok := session.FromContext(ctx)
	if !ok || sess == nil {
		return 0, fmt.Errorf("ada:session.append 只能在有会话上下文的工具调用里使用")
	}
	if kind == "" || len(kind) > maxSessionKindBytes {
		return 0, fmt.Errorf("ada:session.append 的 kind 必须非空且不超过 %d 字节", maxSessionKindBytes)
	}
	for _, r := range kind {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == '/' {
			continue
		}
		return 0, fmt.Errorf("ada:session.append 的 kind 含非法字符: %q", kind)
	}
	if len(dataJSON) == 0 {
		dataJSON = "null"
	}
	if len(dataJSON) > maxSessionDataBytes {
		return 0, fmt.Errorf("ada:session.append 的 data 超过 %d 字节", maxSessionDataBytes)
	}
	if !json.Valid([]byte(dataJSON)) {
		return 0, fmt.Errorf("ada:session.append 的 data 不是合法 JSON")
	}
	seqs, err := sess.Append(session.Event{
		Type: session.PluginEvent(plugin, kind), Data: json.RawMessage(dataJSON),
		Source: plugin, Ignorable: true,
	})
	if err != nil {
		return 0, fmt.Errorf("ada:session.append 写入失败: %w", err)
	}
	if len(seqs) == 0 {
		return 0, fmt.Errorf("ada:session.append 没有分配到 seq")
	}
	return seqs[0], nil
}

// pluginStore 是插件私有 KV:进程内、随插件卸载丢弃。
//
// 它不是持久存储:持久化需要 storage seam,当前未实现。
type pluginStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newPluginStore() *pluginStore {
	return &pluginStore{data: make(map[string]string)}
}

func (s *pluginStore) get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return "null", nil
	}
	return v, nil
}

func (s *pluginStore) set(_ context.Context, key, valueJSON string) error {
	if key == "" || len(key) > maxStoreKeyBytes {
		return fmt.Errorf("ctx.store.set 的 key 必须非空且不超过 %d 字节", maxStoreKeyBytes)
	}
	if len(valueJSON) > maxStoreValueBytes {
		return fmt.Errorf("ctx.store.set 的 value 超过 %d 字节", maxStoreValueBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[key]; !exists && len(s.data) >= maxStoreEntries {
		return fmt.Errorf("ctx.store 已满(%d 个键)", maxStoreEntries)
	}
	s.data[key] = valueJSON
	return nil
}

func (s *pluginStore) del(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *pluginStore) list(_ context.Context, prefix string) (string, error) {
	s.mu.Lock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		if prefix == "" || len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			keys = append(keys, k)
		}
	}
	s.mu.Unlock()
	sort.Strings(keys)
	b, err := json.Marshal(keys)
	if err != nil {
		return "", fmt.Errorf("ctx.store.list 编码失败: %w", err)
	}
	return string(b), nil
}

// 事件读取的边界:默认值与上限。超出上限时报错,让脚本用 from 分页。
const (
	defaultHistoryLimit = 50
	maxHistoryLimit     = 200
	defaultRecentLimit  = 20
	maxHistoryBytes     = 256 << 10
)

// sessionEventDTO 是暴露给脚本的事件形状;Data 保持原始 JSON。
type sessionEventDTO struct {
	Seq       uint64          `json:"seq"`
	Type      string          `json:"type"`
	TS        time.Time       `json:"ts"`
	TurnID    string          `json:"turnId,omitempty"`
	StepID    string          `json:"stepId,omitempty"`
	Source    string          `json:"source"`
	Ignorable bool            `json:"ignorable,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// sessionHistory 实现 ada:session.history(from, limit):seq >= from 的事件。
func sessionHistory(ctx context.Context, from uint64, limit int) (string, error) {
	sess, err := historySession(ctx)
	if err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		return "", fmt.Errorf("ada:session.history 的 limit 不能超过 %d", maxHistoryLimit)
	}
	events, err := sess.Events(from, limit)
	if err != nil {
		return "", fmt.Errorf("ada:session.history 读取失败: %w", err)
	}
	return encodeSessionEvents(events)
}

// sessionRecent 实现 ada:session.recent(limit):最近 N 条事件。
func sessionRecent(ctx context.Context, limit int) (string, error) {
	sess, err := historySession(ctx)
	if err != nil {
		return "", err
	}
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	if limit > maxHistoryLimit {
		return "", fmt.Errorf("ada:session.recent 的 limit 不能超过 %d", maxHistoryLimit)
	}
	events, err := sess.Events(0, 0)
	if err != nil {
		return "", fmt.Errorf("ada:session.recent 读取失败: %w", err)
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	return encodeSessionEvents(events)
}

func historySession(ctx context.Context) (session.Session, error) {
	sess, ok := session.FromContext(ctx)
	if !ok || sess == nil {
		return nil, fmt.Errorf("ada:session 的事件读取只能在有会话上下文的工具调用里使用")
	}
	return sess, nil
}

func encodeSessionEvents(events []session.Event) (string, error) {
	out := make([]sessionEventDTO, 0, len(events))
	for _, ev := range events {
		out = append(out, sessionEventDTO{
			Seq: ev.Seq, Type: ev.Type, TS: ev.TS,
			TurnID: ev.TurnID, StepID: ev.StepID, Source: ev.Source,
			Ignorable: ev.Ignorable, Data: ev.Data,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("ada:session 事件编码失败: %w", err)
	}
	if len(b) > maxHistoryBytes {
		return "", fmt.Errorf("ada:session 事件结果超过 %d 字节,请减小 limit 或使用 from 分页", maxHistoryBytes)
	}
	return string(b), nil
}
