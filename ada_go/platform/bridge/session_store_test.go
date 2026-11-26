package bridge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// ada:session 提供会话身份与插件自有事件;ada:store / ctx.store 共享同一个插件私有 KV。
func TestJSSessionAndStore(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import { current, append } from "ada:session"
import { get, set, list } from "ada:store"
type Ctx = {
  tool: (spec: any, handler: (args: any) => any) => void
  store: { set: (k: string, v: unknown) => void; get: (k: string) => unknown }
}
export function apply(ctx: Ctx) {
  ctx.store.set("boot", 1)
  ctx.tool(
    { name: "session_store", summary: "会话与 store", readOnly: false, input: { type: "object", properties: {} } },
    () => {
      const cur = current()
      const n = Number(get("counter") || 0) + 1
      set("counter", n)
      if (cur) append("note", { sessionId: cur.id, n })
      return JSON.stringify({ cur, n, boot: get("boot"), keys: list("") })
    },
  )
}`,
	})
	p, err := bridge.New(bridge.Options{
		Name: "js-session", Artifact: art,
		Inject: kernel.Inject{Names: []string{"tools"}},
	})
	if err != nil {
		t.Fatalf("构造 bridge 失败: %v", err)
	}

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(plugin kernel.Plugin) {
		t.Helper()
		if _, err := app.Plugin(plugin, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", plugin.Name(), err)
		}
	}
	mount(session.Plugin())
	mount(memory.Plugin())
	mount(seamtools.Plugin())
	bf, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 bridge 失败: %v", err)
	}
	if err := bf.Ready(context.Background()); err != nil {
		t.Fatalf("bridge 未就绪: %v", err)
	}

	reg, err := kernel.Service[*seamtools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.Open(context.Background(), "", session.Meta{Title: "store"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := session.WithSession(context.Background(), sess)

	call := seamtools.CallInfo{Name: "session_store", SessionID: sess.ID(), Args: json.RawMessage(`{}`)}
	first := invokeJS(t, reg, ctx, call)
	if first.cur["id"] != sess.ID() || first.n != 1 || first.boot != float64(1) {
		t.Fatalf("第一次调用不符: %+v", first)
	}
	if !contains(first.keys, "boot") || !contains(first.keys, "counter") {
		t.Fatalf("store keys 不符: %+v", first.keys)
	}

	second := invokeJS(t, reg, ctx, call)
	if second.n != 2 {
		t.Fatalf("store 应跨调用保留计数,实际 %d", second.n)
	}

	events, err := sess.Events(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var notes int
	for _, ev := range events {
		if ev.Type == session.PluginEvent("js-session", "note") {
			notes++
			if !ev.Ignorable {
				t.Fatalf("插件事件必须 Ignorable: %+v", ev)
			}
			if !strings.Contains(string(ev.Data), sess.ID()) {
				t.Fatalf("事件载荷不符: %s", ev.Data)
			}
		}
	}
	if notes != 2 {
		t.Fatalf("两次调用应写两条插件事件,实际 %d", notes)
	}

	// 没有会话上下文:current() 返回 null,store 仍可用。
	bg := invokeJS(t, reg, context.Background(), seamtools.CallInfo{
		Name: "session_store", Args: json.RawMessage(`{}`),
	})
	if bg.cur != nil || bg.n != 3 {
		t.Fatalf("无会话调用不符: %+v", bg)
	}
}

type jsResult struct {
	cur  map[string]any
	n    int
	boot float64
	keys []string
}

func invokeJS(t *testing.T, reg *seamtools.Registry, ctx context.Context, call seamtools.CallInfo) jsResult {
	t.Helper()
	res, err := reg.Invoke(ctx, call)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res.Status != seamtools.StatusOK || len(res.Content) == 0 {
		t.Fatalf("工具结果不符: %+v", res)
	}
	var raw struct {
		Cur  map[string]any `json:"cur"`
		N    int            `json:"n"`
		Boot float64        `json:"boot"`
		Keys []string       `json:"keys"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &raw); err != nil {
		t.Fatalf("结果不是 JSON: %q err=%v", res.Content[0].Text, err)
	}
	return jsResult{cur: raw.Cur, n: raw.N, boot: raw.Boot, keys: raw.Keys}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ada:session 的只读事件接口:history 分页与 recent 取尾部。
func TestJSSessionHistory(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import { current, history, recent } from "ada:session"
type Ctx = { tool: (spec: any, handler: (args: any) => any) => void }
export function apply(ctx: Ctx) {
  ctx.tool(
    { name: "read_history", summary: "读取会话事件", readOnly: true, input: { type: "object", properties: {} } },
    () => {
      return JSON.stringify({ cur: current(), all: history(0, 10), last: recent(2) })
    },
  )
  ctx.tool(
    { name: "read_many", summary: "超过 limit 上限", readOnly: true, input: { type: "object", properties: {} } },
    () => JSON.stringify(history(0, 999)),
  )
}`,
	})
	p, err := bridge.New(bridge.Options{
		Name: "js-history", Artifact: art,
		Inject: kernel.Inject{Names: []string{"tools"}},
	})
	if err != nil {
		t.Fatalf("构造 bridge 失败: %v", err)
	}
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(plugin kernel.Plugin) {
		t.Helper()
		if _, err := app.Plugin(plugin, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", plugin.Name(), err)
		}
	}
	mount(session.Plugin())
	mount(memory.Plugin())
	mount(seamtools.Plugin())
	bf, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 bridge 失败: %v", err)
	}
	if err := bf.Ready(context.Background()); err != nil {
		t.Fatalf("bridge 未就绪: %v", err)
	}
	reg, err := kernel.Service[*seamtools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.Open(context.Background(), "", session.Meta{Title: "history"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(
		session.Event{Type: session.TypeUserMessage, Source: "test", Data: json.RawMessage(`{"role":"user","content":[{"kind":"text","text":"hi"}]}`)},
		session.Event{Type: "plugin:test/note", Source: "test", Ignorable: true, Data: json.RawMessage(`{"n":1}`)},
	); err != nil {
		t.Fatal(err)
	}

	ctx := session.WithSession(context.Background(), sess)
	res, err := reg.Invoke(ctx, seamtools.CallInfo{Name: "read_history", SessionID: sess.ID(), Args: json.RawMessage(`{}`)})
	if err != nil || res.Status != seamtools.StatusOK || len(res.Content) == 0 {
		t.Fatalf("调用失败: %+v err=%v", res, err)
	}
	var got struct {
		Cur  map[string]any   `json:"cur"`
		All  []map[string]any `json:"all"`
		Last []map[string]any `json:"last"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &got); err != nil {
		t.Fatalf("结果不是 JSON: %v", err)
	}
	if got.Cur["id"] != sess.ID() {
		t.Fatalf("current 不符: %+v", got.Cur)
	}
	if len(got.All) != 2 || got.All[0]["type"] != session.TypeUserMessage || got.All[1]["type"] != "plugin:test/note" {
		t.Fatalf("history 不符: %+v", got.All)
	}
	data, _ := got.All[0]["data"].(map[string]any)
	if data["role"] != "user" {
		t.Fatalf("事件 data 未保留 JSON: %+v", got.All[0])
	}
	if len(got.Last) != 2 || got.Last[1]["type"] != "plugin:test/note" {
		t.Fatalf("recent 不符: %+v", got.Last)
	}

	// 超过 limit 上限应失败,而不是悄悄截断。
	res, err = reg.Invoke(ctx, seamtools.CallInfo{Name: "read_many", SessionID: sess.ID(), Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != seamtools.StatusError || !strings.Contains(res.Content[0].Text, "limit") {
		t.Fatalf("超限读取应失败: %+v", res)
	}

	// 无会话上下文:读取应失败。
	res, err = reg.Invoke(context.Background(), seamtools.CallInfo{Name: "read_history", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != seamtools.StatusError {
		t.Fatalf("无会话读取应失败: %+v", res)
	}
}
