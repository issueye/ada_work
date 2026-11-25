package app

import (
	"path/filepath"
	"testing"
)

// TestInstanceKeyIncludesEnv 守住共享进程的判据:同目录同环境才共享,
// 环境不同(每个机器人自己的 key)必须各起一个进程。
func TestInstanceKeyIncludesEnv(t *testing.T) {
	dir := t.TempDir()
	a := &Bot{Workspace: dir, Env: map[string]string{"ADA_LLM_MODEL": "m1"}}
	b := &Bot{Workspace: dir, Env: map[string]string{"ADA_LLM_MODEL": "m1"}}
	c := &Bot{Workspace: dir, Env: map[string]string{"ADA_LLM_MODEL": "m2"}}
	empty := &Bot{Workspace: dir}

	if InstanceKey(a) != InstanceKey(b) {
		t.Fatalf("同目录同环境应得到同一个实例键")
	}
	if InstanceKey(a) == InstanceKey(c) {
		t.Fatalf("同名变量不同值必须分实例")
	}
	if InstanceKey(a) == InstanceKey(empty) {
		t.Fatalf("有环境变量与没有环境变量必须分实例")
	}
	other := &Bot{Workspace: t.TempDir(), Env: a.Env}
	if InstanceKey(other) == InstanceKey(a) {
		t.Fatalf("不同目录必须分实例")
	}
}

// TestNormalizeBotEnv 清洗非法名与空值,避免把奇怪的东西塞进子进程。
func TestNormalizeBotEnv(t *testing.T) {
	in := map[string]string{
		"  ADA_LLM_API_KEY ": " sk-1 ",
		"bad-name":           "x",
		"1BAD":               "x",
		"EMPTY":              "   ",
		"GOOD":               "v",
	}
	out := normalizeBotEnv(in)
	if len(out) != 2 {
		t.Fatalf("应只保留 2 个合法且非空的变量,实际 %v", out)
	}
	if out["ADA_LLM_API_KEY"] != "sk-1" {
		t.Fatalf("值应去掉首尾空白: %q", out["ADA_LLM_API_KEY"])
	}
	if out["GOOD"] != "v" {
		t.Fatalf("GOOD 丢失: %v", out)
	}
	if normalizeBotEnv(nil) != nil || normalizeBotEnv(map[string]string{"EMPTY": ""}) != nil {
		t.Fatalf("没有有效变量时应返回 nil")
	}
}

// TestBotEnvPersists 环境变量进 state.json,重启后随机器人一起恢复。
func TestBotEnvPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	withStatePath(t, path)

	app := New()
	env := map[string]string{"ADA_LLM_MODEL": "bot-model", "ADA_SECRET_DEEPSEEK_API_KEY": "sk-x"}
	bot, err := app.AddBot(Bot{Name: "带环境变量的机器人", Workspace: dir, Env: env})
	if err != nil {
		t.Fatalf("添加机器人失败: %v", err)
	}
	if len(bot.Env) != 2 {
		t.Fatalf("环境变量没有存到机器人: %v", bot.Env)
	}

	restored := New()
	if len(restored.Bots) != 1 {
		t.Fatalf("应恢复 1 个机器人,实际 %d", len(restored.Bots))
	}
	if got := restored.Bots[0].Env; got["ADA_LLM_MODEL"] != "bot-model" || got["ADA_SECRET_DEEPSEEK_API_KEY"] != "sk-x" {
		t.Fatalf("重启后环境变量丢失: %v", got)
	}
}
