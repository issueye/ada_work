package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/catalog"
	seamllm "github.com/issueye/ada_go_v2/plugins/seam/llm"
)

func TestIdentityForTrimsInstruction(t *testing.T) {
	got := identityFor("代码助手", "  先读文件  ")
	want := "你是 代码助手，一个在本地工作目录里运行的 agent。\n先读文件"
	if got != want {
		t.Errorf("identity=%q, want %q", got, want)
	}
	if got := identityFor("代码助手", "  "); strings.HasSuffix(got, "\n") {
		t.Errorf("空指令不应留下尾部换行: %q", got)
	}
}

func TestApplyPersonaReplacesDefaultIdentity(t *testing.T) {
	base := identityFor("机器人", "")
	system := "## Identity\n" + base + "\n\n## Rules\n- 不要执行危险命令"
	req := &seamllm.Request{
		Messages: []seamllm.Message{{
			Role:    seamllm.RoleSystem,
			Content: []seamllm.Block{{Kind: seamllm.BlockText, Text: system}},
		}},
	}
	persona := identityFor("第二机器人", "只读 a.txt")
	applyPersona(req, base, persona)
	text := req.Messages[0].Content[0].Text
	if !strings.Contains(text, "第二机器人") || !strings.Contains(text, "只读 a.txt") {
		t.Errorf("身份没有被替换: %q", text)
	}
	if strings.Contains(text, base) {
		t.Errorf("默认身份仍然存在: %q", text)
	}
	if !strings.Contains(text, "## Rules") {
		t.Errorf("非身份段被破坏: %q", text)
	}

	// 身份与默认值相同(第一个机器人)时不应改写提示。
	same := &seamllm.Request{
		Messages: []seamllm.Message{{
			Role:    seamllm.RoleSystem,
			Content: []seamllm.Block{{Kind: seamllm.BlockText, Text: system}},
		}},
	}
	applyPersona(same, base, base)
	if same.Messages[0].Content[0].Text != system {
		t.Errorf("相同身份不应改写提示: %q", same.Messages[0].Content[0].Text)
	}
}

func TestLoadManifestsPatchesAgentForTheWorkingDirectory(t *testing.T) {
	root := catalog.FindBase(".")
	if root == "" {
		t.Fatal("module root not found")
	}
	preset := filepath.Join(root, "examples", "agent", "agent.preset.json")
	workspace := filepath.Join(root, "examples", "agent", "workspace")
	ms, err := loadManifests(preset, workspace, "代码助手", "先读文件再回答", llmOverride{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var fs, prompt bool
	for _, m := range ms {
		switch m.ID {
		case "fs-local":
			fs = m.Config["workspace"] == workspace
		case "system-prompt":
			identity, _ := m.Config["identity"].(string)
			prompt = strings.Contains(identity, "代码助手") && strings.Contains(identity, "先读文件再回答")
		}
	}
	if !fs || !prompt {
		t.Errorf("fs patched=%v prompt patched=%v", fs, prompt)
	}
	if got := presetModel(ms); got != "replay" {
		t.Errorf("preset model %q, want replay", got)
	}
}

func TestSessionKey(t *testing.T) {
	if got := sessionKey(3, "42"); got != "session:42" {
		t.Errorf("sessionKey(3,42) = %q", got)
	}
	if got := sessionKey(3, " "); got != "bot:3" {
		t.Errorf("空 sessionId 应退回 bot 键, got %q", got)
	}
	if got := sessionKey(0, ""); got != "default" {
		t.Errorf("无 bot 无 session 应落到默认会话, got %q", got)
	}
}

func TestProviderOverrideSwitchesPreset(t *testing.T) {
	root := catalog.FindBase(".")
	if root == "" {
		t.Fatal("module root not found")
	}
	preset := filepath.Join(root, "examples", "agent", "agent.preset.json")
	workspace := filepath.Join(root, "examples", "agent", "workspace")

	openaiMS, err := loadManifests(preset, workspace, "代码助手", "", llmOverride{
		Provider: "openai", Endpoint: "https://example.com/v1", Model: "mock-model",
	})
	if err != nil {
		t.Fatalf("openai override: %v", err)
	}
	if findManifest(openaiMS, "llm-openai").Disabled {
		t.Error("llm-openai should be enabled")
	}
	if !findManifest(openaiMS, "llm-replay").Disabled {
		t.Error("llm-replay should be disabled")
	}
	if got := presetModel(openaiMS); got != "mock-model" {
		t.Errorf("agent-loop model %q, want mock-model", got)
	}
	if got := findManifest(openaiMS, "llm-openai").Config["endpoint"]; got != "https://example.com/v1" {
		t.Errorf("endpoint %v", got)
	}
	if got := findManifest(openaiMS, "llm-openai").Config["apiKeyEnv"]; got != envLLMAPIKey {
		t.Errorf("apiKeyEnv %v", got)
	}

	replayMS, err := loadManifests(preset, workspace, "代码助手", "", llmOverride{Provider: "replay"})
	if err != nil {
		t.Fatalf("replay override: %v", err)
	}
	if findManifest(replayMS, "llm-replay").Disabled {
		t.Error("llm-replay should be enabled")
	}
	if !findManifest(replayMS, "llm-openai").Disabled {
		t.Error("llm-openai should be disabled")
	}
	if got := presetModel(replayMS); got != "replay" {
		t.Errorf("agent-loop model %q, want replay", got)
	}
}
