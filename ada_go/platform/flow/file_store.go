// FileStore 是 flow 检查点的文件实现:每个 runID 一个 JSON 文件,原子写。
package flow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// runIDRe 限制检查点文件名:只允许字母/数字/下划线/短横线。
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// FileStore 把检查点存放在目录下(<dir>/<runID>.json)。
type FileStore struct {
	dir string

	mu sync.Mutex
}

// NewFileStore 创建文件存储;目录不存在时创建。
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("flow: FileStore 目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("flow: 创建检查点目录失败: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

func (s *FileStore) path(runID string) (string, error) {
	if !runIDRe.MatchString(runID) {
		return "", fmt.Errorf("flow: runID 非法(只允许字母/数字/下划线/短横线): %q", runID)
	}
	return filepath.Join(s.dir, runID+".json"), nil
}

// Save 原子写检查点(临时文件 + rename)。
func (s *FileStore) Save(_ context.Context, runID string, data []byte) error {
	path, err := s.path(runID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp(s.dir, ".checkpoint-*")
	if err != nil {
		return fmt.Errorf("flow: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("flow: 写检查点失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flow: 关闭检查点临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("flow: 原子写检查点失败: %w", err)
	}
	return nil
}

// Load 读取检查点;不存在时返回错误。
func (s *FileStore) Load(_ context.Context, runID string) ([]byte, error) {
	path, err := s.path(runID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("flow: 读取检查点失败: %w", err)
	}
	return data, nil
}

// Delete 删除检查点;不存在视为成功(幂等)。
func (s *FileStore) Delete(_ context.Context, runID string) error {
	path, err := s.path(runID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("flow: 删除检查点失败: %w", err)
	}
	return nil
}
