package bridge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// TestJSToolMemory 用真实 fs-local 跑 memory-helper 的完整回路:
// 写入 → 合并 → 中文检索 → 读取 → 更新 → 忘记 → 统计。
func TestJSToolMemory(t *testing.T) {
	dir := t.TempDir()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mountBuiltin(t, app, "seam-tools", nil)
	mountBuiltin(t, app, "seam-fs", nil)
	mountBuiltin(t, app, "fs-local", map[string]any{"workspace": dir})

	manifest, err := plugin.Load("../../plugins/tool/memory-helper")
	if err != nil {
		t.Fatalf("读取 memory-helper 清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造 memory-helper 失败: %v", err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	reg := raw.(*tools.Registry)

	call := func(name string, args map[string]any) tools.Result {
		t.Helper()
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: name, Args: data})
		if err != nil {
			t.Fatalf("%s 调用失败: %v", name, err)
		}
		return res
	}
	text := func(res tools.Result) string {
		t.Helper()
		if len(res.Content) == 0 {
			return ""
		}
		return res.Content[0].Text
	}

	// 1. 写入:记录模型、标签与重要度。
	written := call("memory_write", map[string]any{
		"text":       "用户偏好中文回答,代码注释也使用中文。",
		"tags":       []string{"preference", "language"},
		"importance": 4,
	})
	if written.Status != tools.StatusOK {
		t.Fatalf("memory_write 失败: %+v", written)
	}
	out := text(written)
	if !strings.Contains(out, "已记住") {
		t.Fatalf("写入结果不正确: %s", out)
	}
	idRe := regexp.MustCompile(`#([0-9a-f]{8})`)
	m := idRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("结果里没有短 id: %s", out)
	}
	shortID := m[1]

	// 2. 重复写入应合并而不是新增。
	again := call("memory_write", map[string]any{
		"text":       "用户偏好中文回答,代码注释也使用中文。",
		"tags":       []string{"style"},
		"importance": 5,
	})
	if again.Status != tools.StatusOK || !strings.Contains(text(again), "合并") {
		t.Fatalf("重复写入应合并: %+v", again)
	}

	// 3. 中文检索:bigram 命中,结果带短 id。
	search := call("memory_search", map[string]any{"query": "中文回答", "limit": 5})
	if search.Status != tools.StatusOK || !strings.Contains(text(search), "#"+shortID) {
		t.Fatalf("memory_search 未命中: %+v", search)
	}

	// 4. 按前缀读取完整内容。
	read := call("memory_read", map[string]any{"id": shortID})
	if read.Status != tools.StatusOK || !strings.Contains(text(read), "用户偏好中文回答") {
		t.Fatalf("memory_read 失败: %+v", read)
	}

	// 5. 更新重要度与标签。
	update := call("memory_update", map[string]any{"id": shortID, "importance": 5, "tags": []string{"preference"}})
	if update.Status != tools.StatusOK || !strings.Contains(text(update), "已更新") {
		t.Fatalf("memory_update 失败: %+v", update)
	}

	// 6. 列表与统计都应当看到这一条。
	list := call("memory_list", map[string]any{"limit": 10})
	if list.Status != tools.StatusOK || !strings.Contains(text(list), "#"+shortID) {
		t.Fatalf("memory_list 失败: %+v", list)
	}
	stats := call("memory_stats", map[string]any{})
	if stats.Status != tools.StatusOK || !strings.Contains(text(stats), "总数: 1") {
		t.Fatalf("memory_stats 失败: %+v", stats)
	}

	// 7. 忘记后检索不到,文件里也没有正文了。
	forget := call("memory_forget", map[string]any{"id": shortID})
	if forget.Status != tools.StatusOK || !strings.Contains(text(forget), "已忘记") {
		t.Fatalf("memory_forget 失败: %+v", forget)
	}
	search = call("memory_search", map[string]any{"query": "中文回答"})
	if search.Status != tools.StatusOK || !strings.Contains(text(search), "没有匹配") {
		t.Fatalf("忘记后仍能检索到: %+v", search)
	}

	file := filepath.Join(dir, ".ada", "memory.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("记忆文件不存在: %v", err)
	}
	if strings.Contains(string(data), "用户偏好中文回答") {
		t.Fatalf("忘记后文件仍保留正文: %s", data)
	}
	var store struct {
		Version int `json:"version"`
		Records []struct {
			ID string `json:"id"`
		} `json:"records"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatalf("记忆文件不是合法 JSON: %v", err)
	}
	if store.Version != 1 || len(store.Records) != 0 {
		t.Fatalf("记忆文件结构不正确: %+v", store)
	}
}

// TestJSToolMemoryPin 验证 pinned 记录不被容量淘汰,全部 pinned 时写入会明确报错。
func TestJSToolMemoryPin(t *testing.T) {
	dir := t.TempDir()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mountBuiltin(t, app, "seam-tools", nil)
	mountBuiltin(t, app, "seam-fs", nil)
	mountBuiltin(t, app, "fs-local", map[string]any{"workspace": dir})

	manifest, err := plugin.Load("../../plugins/tool/memory-helper")
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造插件失败: %v", err)
	}
	// 关闭近似去重:本用例只测淘汰与 pinned,不能让「临时记忆 N」被合并成一条。
	f, err := app.Plugin(p, kernel.NewConfig(map[string]any{"maxRecords": 50, "dedupeThreshold": 0}))
	if err != nil {
		t.Fatalf("挂载插件失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("插件未就绪: %v", err)
	}
	raw, _ := app.Root().Service("tools")
	reg := raw.(*tools.Registry)

	call := func(args map[string]any) string {
		t.Helper()
		data, _ := json.Marshal(args)
		res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: "memory_write", Args: data})
		if err != nil || res.Status != tools.StatusOK || len(res.Content) == 0 {
			t.Fatalf("memory_write 失败: %v %+v", err, res)
		}
		return res.Content[0].Text
	}

	// 重要度最低但 pinned:它必须在 50 条上限下仍被保留。
	call(map[string]any{"text": "固定记忆:不要淘汰这一条", "importance": 1, "pinned": true})
	for i := 0; i < 50; i++ {
		call(map[string]any{"text": "临时记忆 " + fmt.Sprint(i), "importance": 2})
	}

	data, err := os.ReadFile(filepath.Join(dir, ".ada", "memory.json"))
	if err != nil {
		t.Fatalf("记忆文件不存在: %v", err)
	}
	if !strings.Contains(string(data), "固定记忆") {
		t.Fatalf("pinned 记录被淘汰了: %s", data)
	}
	var store struct {
		Records []struct {
			Text   string `json:"text"`
			Pinned bool   `json:"pinned"`
		} `json:"records"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatalf("记忆文件不是合法 JSON: %v", err)
	}
	if len(store.Records) != 50 {
		t.Fatalf("记录数应为 50,实际 %d", len(store.Records))
	}
	// 把剩余 48 条 unpinned 逐条挤出去(每次写入淘汰一条),然后容量全部是 pinned。
	call(map[string]any{"text": "第二条固定记忆", "importance": 1, "pinned": true})
	for i := 0; i < 48; i++ {
		call(map[string]any{"text": "又一条 pinned " + fmt.Sprint(i), "importance": 1, "pinned": true})
	}
	// 再写一条 pinned 时已无可淘汰项;必须报错,不能静默丢数据。
	data, _ = json.Marshal(map[string]any{"text": "溢出 pinned", "pinned": true})
	res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: "memory_write", Args: data})
	if err != nil || res.Status != tools.StatusError {
		t.Fatalf("全部 pinned 时应报错,实际 %v %+v", err, res)
	}
}
