// 聊天示例:演示「前端式开发 golang native GUI」的完整双端闭环。
//
// UI 全部在 .aui 组件里(Chat/Sidebar/Bubble);Go 只提供数据面:
//   - app.Expose:chat.contacts / chat.history / chat.send(组件里 ctx.api.call)
//   - app.Emit:机器人回声消息异步推送(组件里 ctx.on('message'))
//
// 运行:go run ./examples/chat
package main

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/issueye/ada_t"
	"github.com/issueye/ada_t/backend/mygoui"
	"github.com/issueye/ada_t/internal/compile"
)

// chatMsg 是跨边界的消息(字段名即 JSON 键,组件里 m.id / m.from / m.text)。
type chatMsg struct {
	ID      int    `json:"id"`
	Contact string `json:"contact"`
	From    string `json:"from"` // "me" 或联系人 id
	Text    string `json:"text"`
	Time    string `json:"time"`
}

// contact 是会话列表项。
type contact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type chatStore struct {
	mu       sync.Mutex
	contacts []contact
	history  map[string][]chatMsg
	nextID   int
	now      func() string
	// 回声话术:按联系人与轮次轮换
	replies map[string][]string
	rot     map[string]int
}

func newStore() *chatStore {
	s := &chatStore{
		contacts: []contact{
			{ID: "ada", Name: "Ada"},
			{ID: "bot", Name: "回声机器人"},
			{ID: "team", Name: "产品群"},
		},
		history: map[string][]chatMsg{},
		replies: map[string][]string{
			"ada":  {"收到,我现在就处理!", "好主意 👍", "稍等,我看一下。", "哈哈,真的假的?"},
			"bot":  {"你说:「%s」", "(回声)%s", "重复一遍:%s"},
			"team": {"张三: 同意", "李四: 排期看一下", "王五: 我没问题"},
		},
		rot: map[string]int{},
		now: func() string { return time.Now().Format("15:04") },
	}
	seed := map[string][][2]string{
		"ada":  {{"ada", "嗨!这个示例怎么样?"}, {"me", "我刚把界面写进 .aui 了"}},
		"bot":  {{"bot", "我是回声机器人,发消息试试。"}},
		"team": {{"team", "王五: 例会改到下午三点"}},
	}
	for id, msgs := range seed {
		for _, m := range msgs {
			s.append(chatMsg{Contact: id, From: m[0], Text: m[1], Time: "09:41"})
		}
	}
	return s
}

func (s *chatStore) append(m chatMsg) chatMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	m.ID = s.nextID
	m.Time = s.now()
	s.history[m.Contact] = append(s.history[m.Contact], m)
	return m
}

func (s *chatStore) reply(contactID, userText string) chatMsg {
	s.mu.Lock()
	pool := s.replies[contactID]
	i := s.rot[contactID]
	s.rot[contactID] = i + 1
	s.mu.Unlock()
	if len(pool) == 0 {
		pool = []string{"…"}
	}
	text := pool[i%len(pool)]
	if strings.Contains(text, "%s") {
		text = fmt.Sprintf(text, userText)
	}
	return s.append(chatMsg{Contact: contactID, From: contactID, Text: text})
}

// clear 清空一个会话的历史。
func (s *chatStore) clear(contactID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[contactID] = nil
}

func main() {
	pkg, err := compile.CompileDir("examples/chat", compile.Options{Entry: "Chat"})
	if err != nil {
		log.Fatalf("编译组件失败: %v", err)
	}
	for _, w := range pkg.Warnings {
		log.Printf("[warn] %s", w)
	}

	app, err := adat.NewApp(pkg, adat.AppOptions{
		Backend: mygoui.New(),
		Locale:  "zh-CN",
		Logf:    func(f string, a ...any) { log.Printf("[js] "+f, a...) },
	})
	if err != nil {
		log.Fatalf("创建应用失败: %v", err)
	}

	store := newStore()

	// —— 服务面:组件里 ctx.api.call('chat', ...) ——
	if err := app.Expose("chat", "contacts", func() []map[string]any {
		store.mu.Lock()
		defer store.mu.Unlock()
		out := make([]map[string]any, 0, len(store.contacts))
		for _, ct := range store.contacts {
			out = append(out, map[string]any{"id": ct.ID, "name": ct.Name})
		}
		return out
	}); err != nil {
		log.Fatalf("Expose contacts: %v", err)
	}
	if err := app.Expose("chat", "history", func(id string) []chatMsg {
		store.mu.Lock()
		defer store.mu.Unlock()
		h := store.history[id]
		out := make([]chatMsg, len(h))
		copy(out, h)
		return out
	}); err != nil {
		log.Fatalf("Expose history: %v", err)
	}
	if err := app.Expose("chat", "send", func(contactID, text string) (chatMsg, error) {
		text = strings.TrimSpace(text)
		if text == "" {
			return chatMsg{}, fmt.Errorf("消息不能为空")
		}
		valid := false
		for _, ct := range store.contacts {
			if ct.ID == contactID {
				valid = true
				break
			}
		}
		if !valid {
			return chatMsg{}, fmt.Errorf("未知会话 %q", contactID)
		}
		// 模拟网络往返:组件用 callAsync 调用,这段延迟不会阻塞界面
		time.Sleep(600 * time.Millisecond)
		mine := store.append(chatMsg{Contact: contactID, From: "me", Text: text})
		// 机器人回声:稍后异步推送(组件 ctx.on('message') 接收)
		delay := time.Duration(600+rand.Intn(900)) * time.Millisecond
		time.AfterFunc(delay, func() {
			reply := store.reply(contactID, text)
			app.Emit("message", reply)
		})
		return mine, nil
	}); err != nil {
		log.Fatalf("Expose send: %v", err)
	}
	if err := app.Expose("chat", "clear", func(contactID string) bool {
		store.clear(contactID)
		return true
	}); err != nil {
		log.Fatalf("Expose clear: %v", err)
	}

	if err := app.Window(adat.WindowOptions{
		Title:           "Ada Chat",
		Width:           760,
		Height:          540,
		BackgroundColor: "#10151d", // 与消息区同色,标题/输入区不再露白底
	}); err != nil {
		log.Fatalf("开窗失败: %v", err)
	}

	// 运行期检查器:ADAT_INSPECT=1 时每 2 秒向终端转储 stats 与 widget 树。
	if os.Getenv("ADAT_INSPECT") == "1" {
		go func() {
			for {
				time.Sleep(2 * time.Second)
				fmt.Println("==== ADAT INSPECT ====")
				if m, err := app.Stats(); err == nil {
					fmt.Printf("stats: %v\n", m)
				}
				for _, id := range app.WindowIDs() {
					fmt.Printf("-- 窗口 %s --\n%s", id, app.DumpWindow(id))
				}
			}
		}()
	}

	// 组件级热重载:ADAT_CHAT_WATCH=1 时监听组件目录,变更即热替换
	// (窗口不关、服务不断;模板变更状态保留,脚本变更重跑 setup)。
	if os.Getenv("ADAT_CHAT_WATCH") == "1" {
		defer adat.WatchDir("examples/chat", "Chat", 600*time.Millisecond, func(pkg *compile.Package) {
			if err := app.HotReload(pkg); err != nil {
				log.Printf("[watch] 热重载失败(旧世界继续服务): %v", err)
			}
		})()
		// 演示驱动:ADAT_CHAT_DEMO=1 时,启动后模拟联系人陆续发来消息——
	}
	// 展示 ctx.on('message') 的异步推送、实时气泡与未读徽标(截图/演示用)。
	// 随后打开确认对话框并弹一条 toast(展示 AlertDialog 与 ctx.toast)。
	if os.Getenv("ADAT_CHAT_DEMO") == "1" {
		go func() {
			seq := []chatMsg{
				{Contact: "ada", From: "ada", Text: "看,这条消息是 Go 直接推送过来的"},
				{Contact: "bot", From: "bot", Text: "(主动推送)我是回声机器人,发消息试试"},
				{Contact: "team", From: "team", Text: "王五: 新版本截图走起"},
			}
			for i, m := range seq {
				time.Sleep(time.Duration(1200+i*1400) * time.Millisecond)
				stored := store.append(m)
				app.Emit("message", stored)
			}
			time.Sleep(2 * time.Second)
			app.Emit("ask-clear", nil) // 打开清空确认对话框
			time.Sleep(1500 * time.Millisecond)
			app.Emit("toast-demo", nil) // 弹一条 toast
		}()
	}

	if err := app.Run(); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}
