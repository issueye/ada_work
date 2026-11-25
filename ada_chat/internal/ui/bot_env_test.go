package ui_test

import (
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/ui"
)

// TestBotDialogTabs 守住「内容按标签页归类」:基本/提示词/环境变量三页各自独立,
// 每页只出现自己的字段。
func TestBotDialogTabs(t *testing.T) {
	a := baseApp(t)
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 1000, 820)
	tt.Frame()
	if err := tt.Click("添加机器人"); err != nil {
		t.Fatalf("打开对话框失败: %v", err)
	}
	tt.Advance(250 * time.Millisecond)
	tt.Frame()

	for _, tab := range []string{"基本", "提示词", "环境变量"} {
		if _, ok := tt.Find(tab); !ok {
			t.Fatalf("缺少页签 %q", tab)
		}
	}
	// 默认在「基本」页:名称与工作目录可见,提示词/环境变量不在这一页。
	if _, ok := tt.Find("名称"); !ok {
		t.Fatalf("基本页应显示名称")
	}
	if _, ok := tt.Find("工作目录"); !ok {
		t.Fatalf("基本页应显示工作目录")
	}
	if _, ok := tt.Find("系统提示词"); ok {
		t.Fatalf("基本页不应显示提示词")
	}

	// 切到「提示词」:只显示提示词。
	if err := tt.Click("提示词"); err != nil {
		t.Fatalf("切到提示词页失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("系统提示词"); !ok {
		t.Fatalf("提示词页应显示系统提示词")
	}
	if _, ok := tt.Find("工作目录"); ok {
		t.Fatalf("提示词页不应显示工作目录")
	}
	shot(t, tt, "bot-dialog-tab-prompt.png")

	// 切到「环境变量」:添加按钮可用,点一下出现一行输入。
	if err := tt.Click("环境变量"); err != nil {
		t.Fatalf("切到环境变量页失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("添加变量"); !ok {
		t.Fatalf("环境变量页应有添加变量按钮")
	}
	if err := tt.Click("添加变量"); err != nil {
		t.Fatalf("点添加变量失败: %v", err)
	}
	tt.Frame()
	if _, ok := tt.Find("变量名"); !ok {
		t.Fatalf("添加后应出现变量名输入")
	}
	if _, ok := tt.Find("变量值"); !ok {
		t.Fatalf("添加后应出现变量值输入")
	}
	shot(t, tt, "bot-dialog-tab-env.png")
}
