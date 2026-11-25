package app

import (
	"path/filepath"
	"testing"
)

// TestThemePersists 守住明暗偏好的落盘与恢复。
func TestThemePersists(t *testing.T) {
	dir := t.TempDir()
	restore := SetConfigPath(filepath.Join(dir, "config.json"))
	t.Cleanup(restore)

	a := New()
	a.Prefs.Theme = "dark"
	if err := a.SavePrefs(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	b := New()
	if b.Prefs.Theme != "dark" {
		t.Fatalf("主题没有落盘: %q", b.Prefs.Theme)
	}
}
