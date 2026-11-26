package bridge

import (
	"context"
	"encoding/json"
	"fmt"

	planseam "github.com/issueye/ada_go_v2/plugins/seam/plan"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// planGet 实现 ada:plan.get():返回计划 JSON;没有计划时返回 "null"。
func planGet(ctx context.Context) (string, error) {
	sess, err := planSession(ctx)
	if err != nil {
		return "", err
	}
	p, ok := planLatest(sess)
	if !ok {
		return "null", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("ada:plan.get 编码失败: %w", err)
	}
	return string(b), nil
}

// planSet 实现 ada:plan.set(plan):校验、补默认值并追加事件。
func planSet(ctx context.Context, body string) (string, error) {
	sess, err := planSession(ctx)
	if err != nil {
		return "", err
	}
	var p planseam.Plan
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return "", fmt.Errorf("ada:plan.set 参数非法: %w", err)
	}
	normalized, err := planseam.Normalize(p)
	if err != nil {
		return "", err
	}
	if err := planAppend(sess, normalized); err != nil {
		return "", fmt.Errorf("ada:plan.set 写入失败: %w", err)
	}
	b, _ := json.Marshal(normalized)
	return string(b), nil
}

// planUpdate 实现 ada:plan.update(index, status):index 从 1 开始。
func planUpdate(ctx context.Context, index int, status string) (string, error) {
	sess, err := planSession(ctx)
	if err != nil {
		return "", err
	}
	st := planseam.Status(status)
	if !st.Valid() {
		return "", fmt.Errorf("ada:plan.update 的 status 非法: %s", status)
	}
	p, ok := planLatest(sess)
	if !ok {
		return "", fmt.Errorf("还没有计划,先调用 ada:plan.set")
	}
	if index < 1 || index > len(p.Steps) {
		return "", fmt.Errorf("ada:plan.update 的 index 超出范围: %d(共 %d 步)", index, len(p.Steps))
	}
	p.Steps[index-1].Status = st
	if err := planAppend(sess, p); err != nil {
		return "", fmt.Errorf("ada:plan.update 写入失败: %w", err)
	}
	b, _ := json.Marshal(p)
	return string(b), nil
}

// planSession 取调用作用域的会话;计划只能写在会话里。
func planSession(ctx context.Context) (session.Session, error) {
	sess, ok := session.FromContext(ctx)
	if !ok || sess == nil {
		return nil, fmt.Errorf("ada:plan 只能在有会话上下文的工具调用里使用")
	}
	return sess, nil
}

// planLatest 从会话历史倒序取最后一条计划事件。
func planLatest(sess session.Session) (planseam.Plan, bool) {
	events, err := sess.Events(0, 0)
	if err != nil {
		return planseam.Plan{}, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != planseam.EventUpdated {
			continue
		}
		if p, derr := planseam.Decode(events[i].Data); derr == nil {
			return p, true
		}
	}
	return planseam.Plan{}, false
}

func planAppend(sess session.Session, p planseam.Plan) error {
	_, err := sess.Append(session.Event{
		Type: planseam.EventUpdated, Data: planseam.Encode(p),
		Source: "bridge/plan", Ignorable: true,
	})
	return err
}
