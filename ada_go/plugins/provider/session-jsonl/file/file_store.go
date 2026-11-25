// Package file 是 session-jsonl 的 JSONL 文件后端:一个会话一个文件,
// 只负责文件读写;并发由上层 Session 的 actor 保证。
package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/plugins/provider/session-jsonl/journal"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

const (
	ProviderName = journal.ProviderName

	logSuffix  = ".jsonl"
	metaSuffix = ".meta.json"
)

// Journal 是 journal.Journal 的本地别名,避免文件后端重复写接口类型。
type Journal = journal.Journal

// Store 是「一个会话一个 JSONL 文件」的后端。
//
// 它只负责文件读写,不持有会话的 actor;同一个 Journal 只由一个 actor 写,
// 不同 Journal 之间没有共享锁,因此多个会话可以并行落盘。
type Store struct {
	dir  string
	sync bool
}

// NewStore 构造文件后端;sync 表示每次 Append 是否 fsync。
func NewStore(dir string, sync bool) *Store {
	return &Store{dir: dir, sync: sync}
}

// Open 加载已有会话;文件不存在时创建。
func (s *Store) Open(id string, meta session.Meta) (Journal, session.Meta, error) {
	if err := validateID(id); err != nil {
		return nil, session.Meta{}, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, session.Meta{}, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "创建会话目录失败", err)
	}
	path := s.logPath(id)
	if _, err := os.Stat(path); err == nil {
		j, err := openFileJournal(s.dir, id, meta, s.sync)
		if err != nil {
			return nil, session.Meta{}, err
		}
		return j, j.Meta(), nil
	} else if !os.IsNotExist(err) {
		return nil, session.Meta{}, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "检查会话文件失败", err)
	}
	j, err := s.create(id, meta)
	if err != nil {
		return nil, session.Meta{}, err
	}
	return j, meta, nil
}

// Create 创建一个全新会话;已存在时返回 EINVALID。
func (s *Store) Create(id string, meta session.Meta) (Journal, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "创建会话目录失败", err)
	}
	return s.create(id, meta)
}

// Fork 用事件前缀创建子会话,保留 seq;事件 SessionID 改为子会话 id。
func (s *Store) Fork(events []session.Event, id string, meta session.Meta) (Journal, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	j, err := s.Create(id, meta)
	if err != nil {
		return nil, err
	}
	raw := make([]session.Event, len(events))
	for i, ev := range events {
		cp := ev
		if cp.Seq == 0 {
			cp.Seq = uint64(i + 1)
		}
		cp.SessionID = id
		if cp.Source == "" {
			cp.Source = "core"
		}
		raw[i] = cp
	}
	if err := j.(*fileJournal).writeRecords(raw); err != nil {
		_ = j.Close()
		_ = os.Remove(s.logPath(id))
		_ = os.Remove(s.metaPath(id))
		return nil, err
	}
	j.(*fileJournal).nextSeq = uint64(len(raw)) + 1
	if len(raw) > 0 {
		j.(*fileJournal).nextSeq = raw[len(raw)-1].Seq + 1
	}
	return j, nil
}

func (s *Store) create(id string, meta session.Meta) (Journal, error) {
	path := s.logPath(id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, seamerr.New(seamerr.ErrInvalid, ProviderName, "会话已存在: "+id, false)
		}
		return nil, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "创建会话文件失败", err)
	}
	if err := writeMeta(s.metaPath(id), meta); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &fileJournal{
		id:       id,
		path:     path,
		metaPath: s.metaPath(id),
		meta:     meta,
		sync:     s.sync,
		f:        f,
		nextSeq:  1,
	}, nil
}

func (s *Store) logPath(id string) string  { return filepath.Join(s.dir, id+logSuffix) }
func (s *Store) metaPath(id string) string { return filepath.Join(s.dir, id+metaSuffix) }

// fileJournal 是单个会话文件。
type fileJournal struct {
	id       string
	path     string
	metaPath string
	meta     session.Meta
	sync     bool

	f       *os.File
	nextSeq uint64

	mu     sync.Mutex
	closed bool
}

func openFileJournal(dir, id string, meta session.Meta, syncWrites bool) (*fileJournal, error) {
	path := filepath.Join(dir, id+logSuffix)
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "打开会话文件失败", err)
	}
	metaPath := filepath.Join(dir, id+metaSuffix)
	stored, ok, err := readMeta(metaPath)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !ok {
		stored = meta
		if err := writeMeta(metaPath, stored); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		_ = f.Close()
		return nil, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "读取会话文件失败", err)
	}
	events, good, err := parseEvents(data)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if good < int64(len(data)) {
		// 崩溃留下的半行:截断到最后一条完整事件之后。
		if err := f.Truncate(good); err != nil {
			_ = f.Close()
			return nil, seamerr.New(seamerr.ErrUnavailable, ProviderName,
				fmt.Sprintf("截断半行失败: %v", err), false)
		}
	}
	if good == int64(len(data)) && len(data) > 0 && data[len(data)-1] != '\n' {
		// 完整但缺少换行:补一个,避免下一次 Append 粘行。
		if _, err := f.Seek(int64(len(data)), io.SeekStart); err != nil {
			_ = f.Close()
			return nil, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "定位会话文件末尾失败", err)
		}
		if _, err := f.Write([]byte("\n")); err != nil {
			_ = f.Close()
			return nil, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "补写换行失败", err)
		}
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			_ = f.Close()
			return nil, seamerr.New(seamerr.ErrInvalid, ProviderName,
				fmt.Sprintf("会话 %s 的 seq 不连续: 第 %d 条为 %d", id, i+1, ev.Seq), false)
		}
	}
	next := uint64(len(events)) + 1
	if len(events) > 0 {
		next = events[len(events)-1].Seq + 1
	}
	return &fileJournal{
		id:       id,
		path:     path,
		metaPath: metaPath,
		meta:     stored,
		sync:     syncWrites,
		f:        f,
		nextSeq:  next,
	}, nil
}

// ID 返回会话 id。
func (j *fileJournal) ID() string { return j.id }

// Meta 返回会话元信息。
func (j *fileJournal) Meta() session.Meta { return j.meta }

// Append 追加一批事件;seq 由后端分配,并补全 TS/SessionID/Source。
func (j *fileJournal) Append(events []session.Event) ([]session.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, errClosed()
	}
	out, err := j.normalize(events, true)
	if err != nil {
		return nil, err
	}
	if err := j.writeRecords(out); err != nil {
		return nil, err
	}
	j.nextSeq += uint64(len(out))
	return out, nil
}

// ReadAll 返回已持久化事件;损坏且无法恢复时返回 EINVALID。
func (j *fileJournal) ReadAll() ([]session.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, errClosed()
	}
	data, err := os.ReadFile(j.path)
	if err != nil {
		return nil, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "读取会话文件失败", err)
	}
	events, _, err := parseEvents(data)
	return events, err
}

// Compact 追加一条摘要事件;upTo 仅记录在摘要载荷里。
func (j *fileJournal) Compact(ctx context.Context, upTo uint64, summary session.Event) (session.Event, error) {
	if err := ctx.Err(); err != nil {
		return session.Event{}, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "压缩前已取消", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return session.Event{}, errClosed()
	}
	if summary.Type == "" {
		summary.Type = session.TypeCompactSummary
	}
	if len(summary.Data) == 0 {
		summary.Data = json.RawMessage(fmt.Sprintf(`{"upTo":%d}`, upTo))
	}
	out, err := j.normalize([]session.Event{summary}, true)
	if err != nil {
		return session.Event{}, err
	}
	if err := j.writeRecords(out); err != nil {
		return session.Event{}, err
	}
	j.nextSeq++
	return out[0], nil
}

// Close 关闭文件句柄;之后 Append/ReadAll 返回 ECANCELED。
func (j *fileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return j.f.Close()
}

func (j *fileJournal) normalize(events []session.Event, assign bool) ([]session.Event, error) {
	out := make([]session.Event, len(events))
	for i, ev := range events {
		if ev.Type == "" {
			return nil, seamerr.New(seamerr.ErrInvalid, ProviderName, "事件 type 不能为空", false)
		}
		cp := ev
		if assign {
			cp.Seq = j.nextSeq + uint64(i)
		} else if cp.Seq == 0 {
			cp.Seq = j.nextSeq + uint64(i)
		}
		if cp.TS.IsZero() {
			cp.TS = time.Now().UTC()
		}
		cp.SessionID = j.id
		if cp.Source == "" {
			cp.Source = "core"
		}
		out[i] = cp
	}
	return out, nil
}

func (j *fileJournal) writeRecords(events []session.Event) error {
	if len(events) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			return seamerr.Wrap(seamerr.ErrInvalid, ProviderName, "序列化事件失败", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	info, err := j.f.Stat()
	if err != nil {
		return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "读取文件大小失败", err)
	}
	before := info.Size()
	if _, err := j.f.Seek(before, io.SeekStart); err != nil {
		return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "定位会话文件末尾失败", err)
	}
	if _, err := j.f.Write(buf.Bytes()); err != nil {
		_ = j.f.Truncate(before)
		return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "写入会话文件失败", err)
	}
	if j.sync {
		if err := j.f.Sync(); err != nil {
			_ = j.f.Truncate(before)
			return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "同步会话文件失败", err)
		}
	}
	return nil
}

// parseEvents 解析 JSONL;返回完整事件与「可保留的字节数」。
// 最后一行不完整时只返回它之前的完整前缀,交给调用方截断。
func parseEvents(data []byte) ([]session.Event, int64, error) {
	var out []session.Event
	var good int64
	for pos := 0; pos < len(data); {
		nl := bytes.IndexByte(data[pos:], '\n')
		end := len(data)
		if nl >= 0 {
			end = pos + nl
		}
		line := data[pos:end]
		if len(line) > 0 {
			var ev session.Event
			if err := json.Unmarshal(line, &ev); err != nil {
				if nl < 0 {
					// 最后一行的半截 JSON:丢弃并截断。
					return out, good, nil
				}
				return nil, good, seamerr.New(seamerr.ErrInvalid, ProviderName,
					fmt.Sprintf("会话文件第 %d 字节处不是合法事件: %v", pos, err), false)
			}
			out = append(out, ev)
		}
		if nl < 0 {
			good = int64(len(data))
			break
		}
		pos = end + 1
		good = int64(pos)
	}
	return out, good, nil
}

func writeMeta(path string, meta session.Meta) error {
	if _, err := os.Stat(path); err == nil {
		return seamerr.New(seamerr.ErrInvalid, ProviderName, "会话元数据已存在", false)
	} else if !os.IsNotExist(err) {
		return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "检查会话元数据失败", err)
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return seamerr.Wrap(seamerr.ErrInvalid, ProviderName, "序列化会话元数据失败", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".meta-*")
	if err != nil {
		return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "创建会话元数据临时文件失败", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "写入会话元数据失败", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "关闭会话元数据失败", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "落盘会话元数据失败", err)
	}
	return nil
}

func readMeta(path string) (session.Meta, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return session.Meta{}, false, nil
		}
		return session.Meta{}, false, seamerr.Wrap(seamerr.ErrUnavailable, ProviderName, "读取会话元数据失败", err)
	}
	var meta session.Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		return session.Meta{}, false, seamerr.Wrap(seamerr.ErrInvalid, ProviderName, "解析会话元数据失败", err)
	}
	return meta, true, nil
}

func validateID(id string) error {
	if id == "" || len(id) > 128 {
		return seamerr.New(seamerr.ErrInvalid, ProviderName, "会话 id 为空或过长", false)
	}
	if strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
		return seamerr.New(seamerr.ErrInvalid, ProviderName, "会话 id 含路径分隔符", false)
	}
	for i, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return seamerr.New(seamerr.ErrInvalid, ProviderName, fmt.Sprintf("会话 id 含非法字符 %q", r), false)
		}
		if i == 0 && (r == '.' || r == '-') {
			return seamerr.New(seamerr.ErrInvalid, ProviderName, "会话 id 不能以 . 或 - 开头", false)
		}
	}
	return nil
}

func errClosed() error {
	return seamerr.New(seamerr.ErrCanceled, ProviderName, "会话已关闭", false)
}
