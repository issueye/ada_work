package controlacp

import (
	"encoding/json"
	"testing"

	"github.com/issueye/ada_go_v2/plugins/seam/control"
)

func TestMessageText(t *testing.T) {
	cases := []struct {
		name  string
		data  string
		want  string
		known bool
	}{
		{
			name:  "agent-loop 嵌套载荷",
			data:  `{"message":{"role":"assistant","content":[{"kind":"text","text":"hi"}]}}`,
			want:  "hi",
			known: true,
		},
		{
			name:  "多段文本拼接",
			data:  `{"message":{"content":[{"kind":"text","text":"你"},{"kind":"text","text":"好"}]}}`,
			want:  "你好",
			known: true,
		},
		{
			name:  "简化 text 字段",
			data:  `{"text":"hi"}`,
			want:  "hi",
			known: true,
		},
		{
			name:  "直接 message 形状",
			data:  `{"role":"user","content":[{"kind":"text","text":"问题"}]}`,
			want:  "问题",
			known: true,
		},
		{
			name:  "纯工具调用的空消息",
			data:  `{"message":{"role":"assistant","content":[]}}`,
			want:  "",
			known: true,
		},
		{
			name:  "不认识的形状",
			data:  `{"foo":1}`,
			want:  "",
			known: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, known := messageText(json.RawMessage(c.data))
			if got != c.want || known != c.known {
				t.Fatalf("messageText = (%q,%v),期望 (%q,%v)", got, known, c.want, c.known)
			}
		})
	}
}

func TestSessionUpdateSkipsEmptyAssistant(t *testing.T) {
	params := sessionUpdateParams(control.Update{
		SessionID: "s1", Kind: "assistant/message",
		Data: json.RawMessage(`{"message":{"role":"assistant","content":[]}}`),
	})
	if params != nil {
		t.Fatalf("空 assistant 消息不应产生 session/update: %+v", params)
	}
}

func TestToolKind(t *testing.T) {
	cases := map[string]string{
		"read_file":    "read",
		"list_files":   "read",
		"tool_find":    "search",
		"grep":         "search",
		"delete_file":  "delete",
		"rename_file":  "move",
		"write_file":   "edit",
		"edit_file":    "edit",
		"shell_exec":   "execute",
		"http_request": "fetch",
		"mystery":      "other",
	}
	for name, want := range cases {
		if got := toolKind(name); got != want {
			t.Fatalf("toolKind(%q) = %q,期望 %q", name, got, want)
		}
	}
}

func TestToolStatus(t *testing.T) {
	cases := map[string]string{
		"":        "completed",
		"ok":      "completed",
		"denied":  "failed",
		"error":   "failed",
		"timeout": "failed",
		"weird":   "failed",
	}
	for in, want := range cases {
		if got := toolStatus(in); got != want {
			t.Fatalf("toolStatus(%q) = %q,期望 %q", in, got, want)
		}
	}
}

func TestReplayUpdateParams(t *testing.T) {
	user := replayUpdateParams(control.Update{
		SessionID: "s1", Kind: "user/message",
		Data: json.RawMessage(`{"role":"user","content":[{"kind":"text","text":"问题"}]}`),
	})
	if user == nil {
		t.Fatal("回放应把 user/message 映射成 user_message_chunk")
	}
	update := user["update"].(map[string]any)
	if update["sessionUpdate"] != "user_message_chunk" {
		t.Fatalf("user 更新类型不符: %+v", update)
	}
	// 实时转发不回显用户消息。
	if sessionUpdateParams(control.Update{SessionID: "s1", Kind: "user/message"}) != nil {
		t.Fatal("实时转发不应回显 user/message")
	}
	// 控制面事件没有 ACP 等价物,应跳过。
	if sessionUpdateParams(control.Update{SessionID: "s1", Kind: "turn/start"}) != nil {
		t.Fatal("turn/start 不应发给 ACP")
	}
}
