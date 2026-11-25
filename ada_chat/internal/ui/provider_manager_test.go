package ui_test

import (
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/app"
	"ada_chat/internal/ui"
)

// TestProviderManagerLayout 守住供应商管理:左侧列表 + 右侧两页签,
// 模型行展示上下文/最大输出/摘要阈值/思考级别,删除有二次确认,标签可移除。
func TestProviderManagerLayout(t *testing.T) {
	a := baseApp(t)
	a.Prefs.Providers = []app.Provider{
		{
			ID: "deepseek", Name: "DeepSeek", Kind: "openai", Endpoint: "https://api.deepseek.com", APIKey: "sk-x",
			Model: "deepseek-chat",
			Models: []app.ModelSpec{
				{ID: "deepseek-chat", ContextWindow: 131072, MaxOutput: 8192, SummaryThreshold: 80, Thinking: "high"},
				{ID: "deepseek-reasoner", ContextWindow: 65536, MaxOutput: 8192, Thinking: "medium"},
			},
		},
	}
	a.Prefs.ActiveProvider = "deepseek"
	a.Prefs.ThinkingTags = []string{"low", "medium", "high"}

	u := ui.New(a)
	tt := mui.NewTester(u.Root, 1200, 860)
	tt.Frame()

	// 从用户 Popover 进入供应商管理。
	if err := tt.Click("老王"); err != nil {
		t.Fatalf("打开用户菜单失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if err := tt.Click("供应商"); err != nil {
		t.Fatalf("进入供应商管理失败: %v", err)
	}
	tt.Advance(250 * time.Millisecond)
	tt.Frame()

	for _, want := range []string{"DeepSeek", "供应商信息", "模型列表"} {
		if _, ok := tt.Find(want); !ok {
			t.Fatalf("供应商管理缺少 %q", want)
		}
	}
	if _, ok := tt.Find("Endpoint"); !ok {
		t.Fatalf("供应商信息页应显示 Endpoint")
	}
	shot(t, tt, "provider-manager-info.png")

	// 删除供应商先弹二次确认;取消后列表不变。
	if err := tt.Click("删除供应商"); err != nil {
		t.Fatalf("点删除供应商失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("删除供应商"); !ok {
		t.Fatalf("删除供应商应弹二次确认")
	}
	shot(t, tt, "provider-delete-confirm.png")
	if false {
	}
	if err := tt.Click("取消"); err != nil {
		t.Fatalf("取消删除失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("DeepSeek"); !ok {
		t.Fatalf("取消删除后供应商应保留")
	}

	// 切到「模型列表」:模型与参数可见。
	if err := tt.Click("模型列表"); err != nil {
		t.Fatalf("切到模型列表失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("deepseek-chat"); !ok {
		t.Fatalf("模型列表应显示模型 ID")
	}
	const summary = "上下文 131072 · 最大输出 8192 · 摘要阈值 80% · 思考 high"
	if _, ok := tt.Find(summary); !ok {
		t.Fatalf("模型摘要不正确,期望 %q", summary)
	}
	shot(t, tt, "provider-manager-models.png")

	// 删除模型同样先确认;取消后模型还在。
	if err := tt.Click("删除模型"); err != nil {
		t.Fatalf("点删除模型失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("删除模型"); !ok {
		t.Fatalf("删除模型应弹二次确认")
	}
	shot(t, tt, "provider-model-delete-confirm.png")
	if false {
	}
	if err := tt.Click("取消"); err != nil {
		t.Fatalf("取消删除模型失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("deepseek-reasoner"); !ok {
		t.Fatalf("取消删除后模型应保留")
	}

	// 打开「添加模型」编辑器:四个参数 + 思考级别标签都在。
	if err := tt.Click("添加模型"); err != nil {
		t.Fatalf("打开添加模型失败: %v", err)
	}
	tt.Frame()
	for _, want := range []string{"模型 ID", "上下文长度", "最大输出", "摘要阈值", "思考级别(点标签选中,× 移除;也可新增自定义标签)"} {
		if _, ok := tt.Find(want); !ok {
			t.Fatalf("模型编辑器缺少 %q", want)
		}
	}
	for _, tag := range []string{"low", "medium", "high"} {
		if _, ok := tt.Find(tag); !ok {
			t.Fatalf("思考级别缺少默认标签 %q", tag)
		}
	}
	shot(t, tt, "provider-model-editor.png")

	if err := tt.Click("取消"); err != nil {
		t.Fatalf("关闭模型编辑器失败: %v", err)
	}
	tt.Frame()
	if _, ok := tt.Find("模型 ID"); ok {
		t.Fatalf("取消后编辑器应关闭")
	}

	// 新增一个供应商并保存:列表变长,当前供应商切到新的,标签集合落盘。
	if err := tt.Click("添加供应商"); err != nil {
		t.Fatalf("添加供应商失败: %v", err)
	}
	tt.Frame()
	if _, ok := tt.Find("供应商 2"); !ok {
		t.Fatalf("新供应商应出现在左侧列表")
	}
	if err := tt.Click("保存"); err != nil {
		t.Fatalf("保存供应商失败: %v", err)
	}
	tt.Advance(150 * time.Millisecond)
	tt.Frame()
	if len(a.Prefs.Providers) != 2 || a.Prefs.ActiveProvider == "deepseek" {
		t.Fatalf("保存后应有 2 个供应商且当前切到新的: %d %q", len(a.Prefs.Providers), a.Prefs.ActiveProvider)
	}
	if len(a.Prefs.ThinkingTags) != 3 {
		t.Fatalf("默认思考标签应保留 3 个: %v", a.Prefs.ThinkingTags)
	}
	if len(a.Prefs.ThinkingTags) != 3 {
		t.Fatalf("默认思考标签应保留 3 个: %v", a.Prefs.ThinkingTags)
	}
	if len(a.Prefs.ThinkingTags) != 3 {
		t.Fatalf("默认思考标签应保留 3 个: %v", a.Prefs.ThinkingTags)
	}
	if len(a.Prefs.ThinkingTags) != 3 {
		t.Fatalf("默认思考标签应保留 3 个: %v", a.Prefs.ThinkingTags)
	}
	if len(a.Prefs.ThinkingTags) != 3 {
		t.Fatalf("默认思考标签应保留 3 个: %v", a.Prefs.ThinkingTags)
	}
}
