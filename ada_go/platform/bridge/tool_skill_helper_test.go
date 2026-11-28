package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// mountSkillHelper 挂载 seam-tools + fs-local(临时工作区)+ skill-helper(TS)。
// cfg 可选:不给时用清单默认配置(不注入技能清单)。
func mountSkillHelper(t *testing.T, workspace string, cfg ...map[string]any) (*kernel.App, *tools.Registry) {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)
	mountBuiltin(t, app, "seam-fs", nil)
	mountBuiltin(t, app, "fs-local", map[string]any{"workspace": workspace})

	manifest, err := plugin.Load("../../plugins/tool/skill-helper")
	if err != nil {
		t.Fatalf("读取 skill-helper 清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造 skill-helper 失败: %v", err)
	}
	var conf map[string]any
	if len(cfg) > 0 {
		conf = cfg[0]
	}
	f, err := app.Plugin(p, kernel.NewConfig(conf))
	if err != nil {
		t.Fatalf("挂载 skill-helper 失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("skill-helper 未就绪: %v", err)
	}
	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	return app, raw.(*tools.Registry)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillWorkspace 造两个技能:一个完整(带 references/),一个缺 SKILL.md。
func skillWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "skills", "demo", "SKILL.md"), `---
name: 演示技能
description: 演示如何列出与读取技能
---
# 演示技能

步骤一:先看 references/note.md。
`)
	writeFile(t, filepath.Join(ws, "skills", "demo", "references", "note.md"), "参考内容:先读这里。\n")
	writeFile(t, filepath.Join(ws, "skills", "broken", "readme.txt"), "没有 SKILL.md\n")
	return ws
}

// TestSkillHelperListAndShow 守住 list/show 的闭环:清单、过滤、正文、附带文件。
func TestSkillHelperListAndShow(t *testing.T) {
	ws := skillWorkspace(t)
	_, reg := mountSkillHelper(t, ws)

	res := pluginCall(t, reg, "skill_list", map[string]any{})
	text := resultText(res)
	if !strings.Contains(text, "演示技能 (demo)") || !strings.Contains(text, "演示如何列出与读取技能") {
		t.Fatalf("skill_list 缺少技能条目: %q", text)
	}
	if !strings.Contains(text, "broken") {
		t.Fatalf("缺 SKILL.md 的目录应出现在跳过说明里: %q", text)
	}

	res = pluginCall(t, reg, "skill_list", map[string]any{"query": "demo"})
	if got := resultText(res); !strings.Contains(got, "演示技能") {
		t.Fatalf("query 过滤失败: %q", got)
	}
	res = pluginCall(t, reg, "skill_list", map[string]any{"query": "不存在"})
	if got := resultText(res); !strings.Contains(got, "没有技能") {
		t.Fatalf("过滤为空时应给出提示: %q", got)
	}

	res = pluginCall(t, reg, "skill_show", map[string]any{"name": "demo"})
	text = resultText(res)
	if !strings.Contains(text, "技能: 演示技能 (demo)") || !strings.Contains(text, "描述: 演示如何列出与读取技能") {
		t.Fatalf("skill_show 头部不对: %q", text)
	}
	if !strings.Contains(text, "步骤一:先看 references/note.md") {
		t.Fatalf("skill_show 缺少正文: %q", text)
	}
	if !strings.Contains(text, "附带文件: references/note.md") {
		t.Fatalf("skill_show 缺少附带文件列表: %q", text)
	}
	if strings.Contains(text, "name: 演示技能") {
		t.Fatalf("skill_show 不应重复输出 frontmatter: %q", text)
	}

	// 读附带文件,支持二级目录。
	res = pluginCall(t, reg, "skill_show", map[string]any{"name": "演示技能", "file": "references/note.md"})
	if got := resultText(res); !strings.Contains(got, "参考内容") {
		t.Fatalf("读取附带文件失败: %q", got)
	}
}

// TestSkillHelperErrors 守住错误路径:未知技能、越界文件、空工作区。
func TestSkillHelperErrors(t *testing.T) {
	ws := skillWorkspace(t)
	_, reg := mountSkillHelper(t, ws)

	callSkillExpectError(t, reg, "skill_show", map[string]any{"name": "不存在"}, "可用技能")

	callSkillExpectError(t, reg, "skill_show", map[string]any{"name": "demo", "file": "../secret.txt"}, "相对路径")

	_, empty := mountSkillHelper(t, t.TempDir())
	res := pluginCall(t, empty, "skill_list", map[string]any{})
	if got := resultText(res); !strings.Contains(got, "没有技能") {
		t.Fatalf("空工作区应提示没有技能: %q", got)
	}
}

// TestSkillHelperScaffold 守住生成:创建目录与 SKILL.md、不覆盖、生成后可被 list 看到。
func TestSkillHelperScaffold(t *testing.T) {
	ws := t.TempDir()
	_, reg := mountSkillHelper(t, ws)

	res := pluginCall(t, reg, "skill_scaffold", map[string]any{
		"name": "PDF tools", "description": "处理 PDF 时使用",
	})
	text := resultText(res)
	if !strings.Contains(text, "已创建技能") {
		t.Fatalf("生成结果不对: %q", text)
	}
	path := filepath.Join(ws, "skills", "pdf-tools", "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("SKILL.md 没有落盘: %v", err)
	}
	if !strings.Contains(string(raw), "name: PDF tools") || !strings.Contains(string(raw), "description: 处理 PDF 时使用") {
		t.Fatalf("SKILL.md 头部不对: %s", raw)
	}

	res = pluginCall(t, reg, "skill_list", map[string]any{})
	if got := resultText(res); !strings.Contains(got, "PDF tools (pdf-tools)") {
		t.Fatalf("生成后 list 应能看到: %q", got)
	}

	res = pluginCall(t, reg, "skill_show", map[string]any{"name": "pdf-tools"})
	if got := resultText(res); !strings.Contains(got, "描述: 处理 PDF 时使用") {
		t.Fatalf("生成后 show 读不到描述: %q", got)
	}

	callSkillExpectError(t, reg, "skill_scaffold", map[string]any{"name": "PDF tools"}, "已存在")
}

// TestSkillHelperAutoInject 守住 autoInject:打开后每轮请求都带技能清单,
// 关闭时保持原样;清单里已有标记时不重复注入。
func TestSkillHelperAutoInject(t *testing.T) {
	ws := skillWorkspace(t)
	app, _ := mountSkillHelper(t, ws, map[string]any{"autoInject": true, "maxDescRune": 60})

	request := func(extra ...llm.Message) *llm.Request {
		msgs := []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.Block{{Kind: llm.BlockText, Text: "你是助手"}}},
			{Role: llm.RoleUser, Content: []llm.Block{{Kind: llm.BlockText, Text: "看看 github 榜单"}}},
		}
		msgs = append(msgs, extra...)
		return &llm.Request{Model: "replay", Messages: msgs}
	}

	got, err := app.Root().Waterfall(context.Background(), llm.EventRequest, request())
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	req, ok := got.(*llm.Request)
	if !ok {
		t.Fatalf("返回值应还原成 *llm.Request,实际 %T", got)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("应注入 1 条技能清单,实际 %d 条: %+v", len(req.Messages), req.Messages)
	}
	injected := req.Messages[1]
	if injected.Role != llm.RoleSystem {
		t.Fatalf("注入消息应是 system: %+v", injected)
	}
	text := injected.Text()
	if !strings.Contains(text, "【可用技能】") || !strings.Contains(text, "演示技能 (demo)") {
		t.Fatalf("清单缺少技能条目: %q", text)
	}
	if !strings.Contains(text, "演示如何列出与读取技能") {
		t.Fatalf("清单应带一行描述: %q", text)
	}

	// 已有标记:同一请求被重复派发时不重复插入。
	already := request(llm.Message{
		Role:    llm.RoleSystem,
		Content: []llm.Block{{Kind: llm.BlockText, Text: "【可用技能】\n(旧清单)"}},
	})
	got, err = app.Root().Waterfall(context.Background(), llm.EventRequest, already)
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	if n := len(got.(*llm.Request).Messages); n != 3 {
		t.Fatalf("已带清单时不应重复注入,实际 %d 条", n)
	}

	// 关闭 autoInject:不加消息。
	appOff, _ := mountSkillHelper(t, ws)
	got, err = appOff.Root().Waterfall(context.Background(), llm.EventRequest, request())
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	if n := len(got.(*llm.Request).Messages); n != 2 {
		t.Fatalf("未开 autoInject 时不应注入,实际 %d 条", n)
	}
}

func callSkillExpectError(t *testing.T, reg *tools.Registry, name string, args map[string]any, want string) {
	t.Helper()
	res := pluginCall(t, reg, name, args)
	if res.Status != tools.StatusError {
		t.Fatalf("%s 应返回 error 状态,实际 %q: %q", name, res.Status, resultText(res))
	}
	if !strings.Contains(resultText(res), want) {
		t.Fatalf("%s 错误信息缺少 %q: %q", name, want, resultText(res))
	}
}
