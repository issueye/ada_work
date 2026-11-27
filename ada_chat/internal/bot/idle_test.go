package bot

// 本文件守住「一轮不要被总时长误杀」:只有连续 idle 没有新进展才中止,
// 有事件时一直续命;总上限由 maxCtx 兜底(另测)。

import (
	"context"
	"testing"
	"time"
)

// TestWatchIdleCancelsAfterSilence 沉默超过 idle 就取消,并标记 idleHit。
func TestWatchIdleCancelsAfterSilence(t *testing.T) {
	s := &Supervisor{}
	r := &instance{}
	r.touch()
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	go s.watchIdle(r, 1, 40*time.Millisecond, ctx, cancel, stop)

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("沉默超过 idle 后应取消这一轮")
	case <-stop:
		t.Fatalf("未关闭 stop 时看门狗不应自行退出")
	}
	if !r.idleHit.Load() {
		t.Fatalf("取消原因应标记为 idle")
	}
}

// TestWatchIdleKeepsProgress 持续有进展时看门狗不得取消。
func TestWatchIdleKeepsProgress(t *testing.T) {
	s := &Supervisor{}
	r := &instance{}
	r.touch()
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	go s.watchIdle(r, 1, 80*time.Millisecond, ctx, cancel, stop)

	// 每 20ms 打一次点,持续 400ms:远超 idle,但不该被取消。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		r.touch()
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("持续有进展时不应取消这一轮")
	default:
	}
	close(stop)
	cancel()
}
