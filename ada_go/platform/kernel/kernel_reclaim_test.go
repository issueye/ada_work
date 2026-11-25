package kernel_test

import (
	"context"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// 1. ttl 必须为正。
func TestReclaimRejectsBadTTL(t *testing.T) {
	app := newApp(t)
	if _, err := app.ReclaimIdle(context.Background(), 0, time.Now()); err == nil {
		t.Fatalf("ttl=0 应报错")
	}
}

// 2. 有 dependents 的提供者本轮不回收;依赖者先回收,下一轮再回收提供者。
func TestReclaimSkipsProvidersWithDependents(t *testing.T) {
	app := newApp(t)
	provider, err := app.Plugin(kernel.Func("provider", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("svc", 1)
		return nil, err
	}).WithProvide("svc"), kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载提供者失败: %v", err)
	}
	consumer, err := app.Plugin(kernel.Func("consumer", func(*kernel.Context, kernel.Config) (kernel.Dispose, error) {
		return nil, nil
	}).WithInject("svc"), kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	if consumer.State() != kernel.Active {
		t.Fatalf("消费者应 active,实际 %s", consumer.State())
	}

	now := time.Now().Add(time.Hour)
	ids, err := app.ReclaimIdle(context.Background(), 30*time.Minute, now)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if got := app.Inspect(); len(got.Fibers) != 1 {
		t.Fatalf("第一轮应只回收依赖者,剩余 fiber %d", len(got.Fibers))
	}
	if !containsID(ids, consumer.ID()) || containsID(ids, provider.ID()) {
		t.Fatalf("第一轮回收结果不正确: %v", ids)
	}

	ids, err = app.ReclaimIdle(context.Background(), 30*time.Minute, now)
	if err != nil {
		t.Fatalf("第二轮回收失败: %v", err)
	}
	if !containsID(ids, provider.ID()) {
		t.Fatalf("第二轮应回收提供者: %v", ids)
	}
	if got := app.Inspect(); len(got.Fibers) != 0 {
		t.Fatalf("回收后不应残留 fiber: %+v", got.Fibers)
	}
}

// 3. 未超过 ttl 的 fiber 不回收;超过后回收。
func TestReclaimRespectsTTL(t *testing.T) {
	app := newApp(t)
	f, err := app.Plugin(kernel.Func("idle", func(*kernel.Context, kernel.Config) (kernel.Dispose, error) {
		return nil, nil
	}), kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if f.LastActive().IsZero() {
		t.Fatalf("挂载后 LastActive 应有值")
	}

	if ids, err := app.ReclaimIdle(context.Background(), time.Hour, f.LastActive().Add(30*time.Minute)); err != nil || len(ids) != 0 {
		t.Fatalf("未超过 ttl 不应回收: ids=%v err=%v", ids, err)
	}
	ids, err := app.ReclaimIdle(context.Background(), time.Hour, f.LastActive().Add(2*time.Hour))
	if err != nil || !containsID(ids, f.ID()) {
		t.Fatalf("超过 ttl 应回收: ids=%v err=%v", ids, err)
	}
}

// 4. 服务解析会 touch 调用方,影响回收判定。
func TestServiceAccessTouchesFiber(t *testing.T) {
	app := newApp(t)
	if _, err := app.Plugin(kernel.Func("provider", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("svc", 1)
		return nil, err
	}).WithProvide("svc"), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载提供者失败: %v", err)
	}
	consumer, err := app.Plugin(kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		if _, err := ctx.Service("svc"); err != nil {
			return nil, err
		}
		return nil, nil
	}).WithInject("svc"), kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	last := consumer.LastActive()
	if last.IsZero() {
		t.Fatalf("服务解析后应有活动时间")
	}
	consumer.Touch()
	if consumer.LastActive().Before(last) {
		t.Fatalf("Touch 后 LastActive 不应倒退")
	}
	idleSince := consumer.LastActive()

	// 消费者在 apply 里取过服务 → 最近活动时间被更新;此时只过了 30 分钟,不应回收。
	if ids, err := app.ReclaimIdle(context.Background(), time.Hour, idleSince.Add(30*time.Minute)); err != nil || len(ids) != 0 {
		t.Fatalf("活动后 30 分钟内不应回收: ids=%v err=%v", ids, err)
	}
	// 2 小时后:消费者先回收,提供者第二轮再回收。
	ids, err := app.ReclaimIdle(context.Background(), time.Hour, idleSince.Add(2*time.Hour))
	if err != nil || !containsID(ids, consumer.ID()) {
		t.Fatalf("应回收消费者: ids=%v err=%v", ids, err)
	}
}

// 5. Pending/Failed 状态不参与回收(它们没有「活动」可言,由依赖门控管理)。
func TestPendingNotReclaimed(t *testing.T) {
	app := newApp(t)
	pending, err := app.Plugin(kernel.Func("waits", func(*kernel.Context, kernel.Config) (kernel.Dispose, error) {
		return nil, nil
	}).WithInject("svc"), kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if pending.State() != kernel.Pending {
		t.Fatalf("应 pending,实际 %s", pending.State())
	}
	ids, err := app.ReclaimIdle(context.Background(), time.Minute, time.Now().Add(24*time.Hour))
	if err != nil || len(ids) != 0 {
		t.Fatalf("pending fiber 不应被回收: ids=%v err=%v", ids, err)
	}
}

func containsID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}
