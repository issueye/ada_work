// FileJobStore 是 JobStore 的 JSON 文件实现:原子写 + 进程内互斥。
//
// 它不是分布式锁:多进程同时写同一文件仍可能互相覆盖(后写者赢)。
// 生产环境的多窗口防重要么共用一个进程,要么换真正的分布式存储。
package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fileJobState struct {
	LastRun    time.Time `json:"lastRun,omitempty"`
	LeaseUntil time.Time `json:"leaseUntil,omitempty"`
}

type fileJobDoc struct {
	Jobs map[string]fileJobState `json:"jobs"`
}

// FileJobStore 把任务状态存放在一个 JSON 文件里。
//
// 跨进程安全:用 <path>.lock 的 O_CREATE|O_EXCL 锁文件包住「读-改-写」,
// 过期锁(超过 JobLease 的 mtime)会被回收。它不是完美的分布式锁,但能挡住
// 最常见的并发抢租约竞态。
type FileJobStore struct {
	path string

	mu sync.Mutex
}

// lockPath 返回锁文件路径。
func (s *FileJobStore) lockPath() string { return s.path + ".lock" }

// tryLock 尝试获取跨进程锁;成功后返回 release。
func (s *FileJobStore) tryLock() (func() error, bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid=%d time=%d", os.Getpid(), time.Now().UnixNano())
			_ = f.Close()
			return func() error { return os.Remove(s.lockPath()) }, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, fmt.Errorf("sched: 获取 job 锁失败: %w", err)
		}
		info, statErr := os.Stat(s.lockPath())
		if statErr == nil && time.Since(info.ModTime()) > JobLease {
			_ = os.Remove(s.lockPath()) // 过期锁
			continue
		}
		return nil, false, nil
	}
	return nil, false, nil
}

// NewFileJobStore 创建文件存储;父目录不存在时创建。
func NewFileJobStore(path string) (*FileJobStore, error) {
	if path == "" {
		return nil, fmt.Errorf("sched: FileJobStore 的路径不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("sched: 创建存储目录失败: %w", err)
	}
	return &FileJobStore{path: path}, nil
}

// LastRun 返回上次运行时间。
func (s *FileJobStore) LastRun(_ context.Context, id string) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return time.Time{}, false, err
	}
	st, ok := doc.Jobs[id]
	return st.LastRun, ok && !st.LastRun.IsZero(), nil
}

// SetLastRun 记录运行时间并释放租约(原子写)。
func (s *FileJobStore) SetLastRun(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, ok, err := s.tryLock()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sched: job 存储锁被其它进程持有,无法写入 lastRun")
	}
	defer release()
	doc, err := s.load()
	if err != nil {
		return err
	}
	doc.Jobs[id] = fileJobState{LastRun: at}
	return s.save(doc)
}

// AcquireLease 尝试获取租约(进程内互斥 + 跨进程锁文件)。
func (s *FileJobStore) AcquireLease(_ context.Context, id string, now time.Time, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, locked, err := s.tryLock()
	if err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	defer release()
	doc, err := s.load()
	if err != nil {
		return false, err
	}
	st := doc.Jobs[id]
	if st.LeaseUntil.After(now) {
		return false, nil
	}
	st.LeaseUntil = now.Add(ttl)
	doc.Jobs[id] = st
	if err := s.save(doc); err != nil {
		return false, err
	}
	return true, nil
}

// State 返回某任务的当前状态(诊断/测试用)。
func (s *FileJobStore) State(id string) (JobState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return JobState{}, false, err
	}
	st, ok := doc.Jobs[id]
	return JobState(st), ok, nil
}

func (s *FileJobStore) load() (fileJobDoc, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileJobDoc{Jobs: map[string]fileJobState{}}, nil
		}
		return fileJobDoc{}, fmt.Errorf("sched: 读取 job 存储失败: %w", err)
	}
	var doc fileJobDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return fileJobDoc{}, fmt.Errorf("sched: job 存储损坏: %w", err)
	}
	if doc.Jobs == nil {
		doc.Jobs = map[string]fileJobState{}
	}
	return doc, nil
}

func (s *FileJobStore) save(doc fileJobDoc) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("sched: 序列化 job 存储失败: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".jobs-*")
	if err != nil {
		return fmt.Errorf("sched: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("sched: 写临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sched: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("sched: 原子写 job 存储失败: %w", err)
	}
	return nil
}
