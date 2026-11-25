package expr

import (
	"encoding/json"
	"testing"
)

func TestParseOK(t *testing.T) {
	cases := []string{
		"1", "1.5", "'hi'", "\"hi\"", "true", "false", "null",
		"state.count", "state.user.name", "items[0]", "list[i + 1]",
		"!ok", "-x", "a + b * c", "(a + b) / 2",
		"a === b", "a !== b", "a > 1 && b < 2 || c",
		"a ?? b", "cond ? x : y", "cond ? (a ? b : c) : d",
		"[]", "[1, 'two', ok]", "[1, [2, 3]]",
		"inc", "inc()", "inc(1, 2)", "select(item)", "$t('key', n)",
		"state.items[i].name",
	}
	for _, src := range cases {
		if _, err := Parse(src); err != nil {
			t.Errorf("Parse(%q) 应成功,得到 %v", src, err)
		}
	}
}

func TestParseRejected(t *testing.T) {
	cases := []string{
		"a = 1",          // 赋值
		"new Foo()",      // new
		"this.x",         // this
		"await x",        // await
		"typeof x",       // 运算符不在词表
		"foo()()",        // 链式调用
		"a++",            // 后缀不在词表
		"",               // 空
		"a +",            // 悬垂运算符
		"(a",             // 未闭合
		"'unterminated",  // 字符串未闭合
		"window.alert()", // 全局路径(求值期判定,但这里能解析——见下)
	}
	// window.alert() 语法上合法(作用域外名字求值期拒绝),从拒绝清单里排除
	for _, src := range cases {
		if src == "window.alert()" {
			continue
		}
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q) 应拒绝", src)
		}
	}
}

func TestParseAction(t *testing.T) {
	for _, src := range []string{"inc", "inc()", "select(item, 2)", "state.fn", "obj.handler"} {
		if _, err := ParseAction(src); err != nil {
			t.Errorf("ParseAction(%q) 应成功: %v", src, err)
		}
	}
	for _, src := range []string{"1 + 2", "'x' ? a : b"} {
		if _, err := ParseAction(src); err == nil {
			t.Errorf("ParseAction(%q) 应拒绝(动作只能是函数/调用)", src)
		}
	}
}

func TestNumericLiteralOperators(t *testing.T) {
	// 数字字面量后紧跟 +/- 时不能被吞进字面量(1+2 必须是二元加法)。
	cases := []struct {
		src string
		op  string
		l   float64
		r   float64
	}{
		{"1+2", "+", 1, 2},
		{"1-2", "-", 1, 2},
		{"2*3", "*", 2, 3},
		{"1e3+2", "+", 1000, 2},
		{"1.5e-3+1", "+", 0.0015, 1},
	}
	for _, c := range cases {
		n, err := Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.src, err)
		}
		if n.T != "bin" || n.Op != c.op {
			t.Fatalf("Parse(%q) = %s/%s, want bin/%s", c.src, n.T, n.Op, c.op)
		}
		lv, _ := n.L.V.(float64)
		rv, _ := n.R.V.(float64)
		if lv != c.l || rv != c.r {
			t.Fatalf("Parse(%q) 操作数 = %v/%v, want %v/%v", c.src, lv, rv, c.l, c.r)
		}
	}
}

func TestASTStableJSON(t *testing.T) {
	// 同输入同字节(golden 前提)
	n1, err := Parse("a.b[0] + 1")
	if err != nil {
		t.Fatal(err)
	}
	n2, _ := Parse("a.b[0] + 1")
	b1, _ := json.Marshal(n1)
	b2, _ := json.Marshal(n2)
	if string(b1) != string(b2) {
		t.Fatalf("AST JSON 不稳定:\n%s\n%s", b1, b2)
	}
	// 关键结构:路径段、二元、字面量
	want := `{"t":"bin","op":"+","l":{"t":"path","p":[{"key":"a"},{"key":"b"},{"idx":{"t":"lit","v":0}}]},"r":{"t":"lit","v":1}}`
	if string(b1) != want {
		t.Fatalf("AST JSON 与期望不符:\n got %s\nwant %s", b1, want)
	}
}

func TestNullLiteralJSON(t *testing.T) {
	n, err := Parse("null")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(n)
	// null 字面量 V 为 nil → JSON 里 v 缺省;JS 端按 undefined→null 处理
	if string(b) != `{"t":"lit"}` {
		t.Fatalf("null 字面量 JSON: %s", b)
	}
}
