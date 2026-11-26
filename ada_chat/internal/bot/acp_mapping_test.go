package bot

import (
	"encoding/json"
	"testing"

	"ada_chat/internal/app"
)

func TestACPArgs(t *testing.T) {
	s := &Supervisor{app: &app.App{Prefs: app.Prefs{
		AgentKind: app.AgentACP, AgentPreset: `C:\preset.json`,
	}}}
	got := s.acpArgs()
	if len(got) != 3 || got[0] != "acp" || got[1] != "--preset" || got[2] != `C:\preset.json` {
		t.Fatalf("默认 ACP 参数不符: %+v", got)
	}

	s.args = []string{"acp", "--preset", "custom.json"}
	got = s.acpArgs()
	if len(got) != 3 || got[2] != "custom.json" {
		t.Fatalf("ADA_BOT_RUNTIME_ARGS 应覆盖默认参数: %+v", got)
	}

	s.args = nil
	s.app.Prefs.AgentPreset = ""
	if got := s.acpArgs(); got != nil {
		t.Fatalf("没有 preset/args 时应返回 nil: %+v", got)
	}
}

func TestACPContentText(t *testing.T) {
	cases := map[string]string{
		`{"type":"text","text":"hi"}`:                                  "hi",
		`[{"type":"text","text":"你"},{"type":"text","text":"好"}]`:      "你好",
		`[{"type":"content","content":{"type":"text","text":"工具结果"}}]`: "工具结果",
		`"纯字符串"`: "纯字符串",
		`null`:   "",
	}
	for raw, want := range cases {
		if got := acpContentText(json.RawMessage(raw)); got != want {
			t.Fatalf("acpContentText(%s) = %q,期望 %q", raw, got, want)
		}
	}
}

func TestACPOptionAndStatus(t *testing.T) {
	options := []acpOptionInfo{
		{OptionID: "allow_once", Kind: "allow_once"},
		{OptionID: "reject_once", Kind: "reject_once"},
	}
	if got := acpOption(options, true); got != "allow_once" {
		t.Fatalf("放行选项不符: %q", got)
	}
	if got := acpOption(options, false); got != "reject_once" {
		t.Fatalf("拒绝选项不符: %q", got)
	}
	if got := acpOption([]acpOptionInfo{{OptionID: "allow_once", Kind: "allow"}}, false); got != "" {
		t.Fatalf("没有拒绝项时应返回空: %q", got)
	}
	for in, want := range map[string]string{
		"completed": "ok", "failed": "error", "in_progress": "running", "": "running",
	} {
		if got := acpToolStatus(in); got != want {
			t.Fatalf("acpToolStatus(%q) = %q,期望 %q", in, got, want)
		}
	}
}

func TestACPRawString(t *testing.T) {
	if got := acpRawString(json.RawMessage(`{"path":"a.txt"}`)); got != `{"path":"a.txt"}` {
		t.Fatalf("对象应原样显示: %q", got)
	}
	if got := acpRawString(json.RawMessage(`"文本"`)); got != "文本" {
		t.Fatalf("字符串应去引号: %q", got)
	}
	if got := acpRawString(nil); got != "" {
		t.Fatalf("nil 应为空: %q", got)
	}
}
