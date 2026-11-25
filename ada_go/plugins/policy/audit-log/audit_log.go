// Package auditlog 是审计落盘策略:把观察/审批事件追加为 JSONL。
//
// 只记录事件 payload(观察事件本身已约定不含 body/secret);不做脱敏/上报,按大小轮转。
package auditlog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

// EventApprovalDecided 是 fs/shell 策略发出的审批结果事件(seam 暂未导出常量)。
const EventApprovalDecided = "approval/decided"

// PolicyName 是插件 id。
const PolicyName = "audit-log"

// record 是 JSONL 的一行。
type record struct {
	TS      string `json:"ts"`
	Event   string `json:"event"`
	Payload any    `json:"payload,omitempty"`
}

type config struct {
	Path string `json:"path"`
	// MaxBytes > 0 时按大小轮转;Keep 是保留的轮转文件数(默认 2)。
	MaxBytes int64 `json:"maxBytes,omitempty"`
	Keep     int   `json:"keep,omitempty"`
}

// Plugin 返回 audit-log 插件:按配置路径追加 JSONL。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "audit-log: 解析配置失败: %v", err)
		}
		if conf.Path == "" {
			return nil, kernel.Fail(kernel.CodeInvalid, "audit-log: 必须配置 path")
		}
		if conf.MaxBytes < 0 || conf.Keep < 0 {
			return nil, kernel.Fail(kernel.CodeInvalid, "audit-log: maxBytes/keep 不能为负")
		}
		keep := conf.Keep
		if keep == 0 {
			keep = 2
		}
		f, err := os.OpenFile(conf.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("audit-log: 打开审计文件失败: %w", err)
		}

		var mu sync.Mutex
		rotate := func() {
			_ = f.Close()
			for i := keep; i >= 1; i-- {
				from := conf.Path
				if i > 1 {
					from = fmt.Sprintf("%s.%d", conf.Path, i-1)
				}
				to := fmt.Sprintf("%s.%d", conf.Path, i)
				_ = os.Remove(to)
				_ = os.Rename(from, to)
			}
			f, _ = os.OpenFile(conf.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		}
		write := func(event string, payload any) {
			mu.Lock()
			defer mu.Unlock()
			data, err := json.Marshal(record{
				TS: time.Now().UTC().Format(time.RFC3339Nano), Event: event, Payload: redactPayload(payload),
			})
			if err != nil {
				return
			}
			if conf.MaxBytes > 0 {
				if info, statErr := f.Stat(); statErr == nil && info.Size()+int64(len(data))+1 > conf.MaxBytes {
					rotate()
				}
			}
			_, _ = f.Write(append(data, '\n'))
		}

		for _, name := range []string{
			seamfs.EventObserve, seamshell.EventObserve,
			seamapproval.EventRequested, EventApprovalDecided,
			caps.EventDeny,
		} {
			eventName := name
			ctx.On(eventName, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
				var payload any
				if len(args) > 0 {
					payload = args[0]
				}
				write(eventName, payload)
				return nil, nil
			})
		}
		return func(context.Context) error { return f.Close() }, nil
	})
}
