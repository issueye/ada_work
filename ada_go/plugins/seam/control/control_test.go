package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type fakeInvoker struct {
	calls []string
}

func (f *fakeInvoker) Invoke(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	f.calls = append(f.calls, method)
	switch method {
	case MethodSessionCreate:
		var req CreateRequest
		_ = json.Unmarshal(params, &req)
		return json.Marshal(SessionInfo{ID: "s1", Title: req.Title})
	case MethodSessionPrompt:
		var req PromptRequest
		_ = json.Unmarshal(params, &req)
		return json.Marshal(TurnResult{ID: "t1", Text: "echo:" + req.Text})
	case MethodSessionList:
		return json.RawMessage(`[{"id":"s1"}]`), nil
	case MethodSessionPermission:
		return json.RawMessage(`{"optionId":"allow"}`), nil
	default:
		return nil, errors.New("unknown")
	}
}

func TestClientRoutesInvoker(t *testing.T) {
	inv := &fakeInvoker{}
	c := New(inv)
	info, err := c.Create(context.Background(), CreateRequest{Title: "demo"})
	if err != nil || info.ID != "s1" || info.Title != "demo" {
		t.Fatalf("create 结果不符: %+v err=%v", info, err)
	}
	turn, err := c.Prompt(context.Background(), PromptRequest{SessionID: "s1", Text: "hi"})
	if err != nil || turn.Text != "echo:hi" {
		t.Fatalf("prompt 结果不符: %+v err=%v", turn, err)
	}
	list, err := c.List(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != "s1" {
		t.Fatalf("list 结果不符: %+v err=%v", list, err)
	}
	if _, err := c.RequestPermission(context.Background(), PermissionRequest{SessionID: "s1"}); err != nil {
		t.Fatalf("permission 失败: %v", err)
	}
}

func TestNilClientIsUnimplemented(t *testing.T) {
	if _, err := New(nil).Create(context.Background(), CreateRequest{}); err == nil {
		t.Fatal("没有实现时应返回错误")
	}
}
