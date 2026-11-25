package ui

import "testing"
import "ada_chat/internal/app"

// TestRemoveThinkingTag 守住标签移除:从集合里删掉,若正是当前选中则一起清空。
func TestRemoveThinkingTag(t *testing.T) {
	u := &UI{providerTags: []string{"low", "medium", "high"}, modelThinking: "medium"}
	u.removeThinkingTag("low")
	if len(u.providerTags) != 2 || u.providerTags[0] != "medium" || u.providerTags[1] != "high" {
		t.Fatalf("移除后应剩 medium/high: %v", u.providerTags)
	}
	if u.modelThinking != "medium" {
		t.Fatalf("移除未选中的标签不应改当前选择: %q", u.modelThinking)
	}

	u.removeThinkingTag("medium")
	if u.modelThinking != "" {
		t.Fatalf("移除当前选中标签应清空选择: %q", u.modelThinking)
	}
	if len(u.providerTags) != 1 || u.providerTags[0] != "high" {
		t.Fatalf("应只剩 high: %v", u.providerTags)
	}
}

// TestAddThinkingTag 守住新增:去空白、去重,成功后清空输入。
func TestAddThinkingTag(t *testing.T) {
	u := &UI{providerTags: []string{"low"}, newTag: "  minimal  "}
	u.addThinkingTag()
	if len(u.providerTags) != 2 || u.providerTags[1] != "minimal" {
		t.Fatalf("新增标签失败: %v", u.providerTags)
	}
	if u.newTag != "" {
		t.Fatalf("新增后输入框应清空")
	}

	u.newTag = "low"
	u.addThinkingTag()
	if len(u.providerTags) != 2 {
		t.Fatalf("重复标签不应新增: %v", u.providerTags)
	}
}

// TestCommitModelFields 守住模型参数写入:数字字段解析进 ModelSpec,编辑器关闭。
func TestCommitModelFields(t *testing.T) {
	u := &UI{
		providerList:  []app.Provider{{ID: "p", Name: "P", Models: []app.ModelSpec{{ID: "m1"}}}},
		providerIdx:   0,
		modelEditor:   true,
		modelEdit:     -1,
		modelID:       "m2",
		modelCtx:      "131072",
		modelMax:      "8192",
		modelSummary:  "80",
		modelThinking: "high",
	}
	u.commitModel(&u.providerList[0])
	if u.modelEditor {
		t.Fatalf("保存成功后编辑器应关闭: %q", u.modelErr)
	}
	p := &u.providerList[0]
	if len(p.Models) != 2 || p.Models[1].ID != "m2" {
		t.Fatalf("模型没有加入列表: %+v", p.Models)
	}
	m := p.Models[1]
	if m.ContextWindow != 131072 || m.MaxOutput != 8192 || m.SummaryThreshold != 80 || m.Thinking != "high" {
		t.Fatalf("模型参数没有写入: %+v", m)
	}
}
