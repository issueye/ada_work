package stylec

import (
	"testing"
)

func TestCompileTokens(t *testing.T) {
	tokens, err := Compile(`.hero {
  font-size: 44;
  bold: true;
  color: #2f6fed;
}
.dim { color: gray; }`)
	if err != nil {
		t.Fatalf("Compile 失败: %v", err)
	}
	hero := tokens["hero"]
	if hero == nil {
		t.Fatal("缺 .hero")
	}
	if hero["font-size"] != 44.0 {
		t.Errorf("font-size = %v", hero["font-size"])
	}
	if hero["bold"] != true {
		t.Errorf("bold = %v", hero["bold"])
	}
	if hero["color"] != "#2f6fed" {
		t.Errorf("color = %v", hero["color"])
	}
	if tokens["dim"]["color"] != "gray" {
		t.Errorf("dim.color = %v", tokens["dim"]["color"])
	}
}

func TestRejectAtRule(t *testing.T) {
	if _, err := Compile("@import url(x.css);"); err == nil {
		t.Fatal("@import 应拒绝")
	}
}

func TestRejectGlobalSelector(t *testing.T) {
	for _, src := range []string{"body { color: red; }", ".a, .b { color: red; }", ".a > .b { color: red; }", "* { color: red; }"} {
		if _, err := Compile(src); err == nil {
			t.Errorf("%q 应拒绝(只允许单个 .class)", src)
		}
	}
}

func TestRejectURL(t *testing.T) {
	if _, err := Compile(".a { color: url(http://x); }"); err == nil {
		t.Fatal("url() 应拒绝")
	}
}

func TestRejectUnknownProp(t *testing.T) {
	if _, err := Compile(".a { transform: scale(2); }"); err == nil {
		t.Fatal("白名单外属性应拒绝")
	}
}

func TestRejectDuplicateRule(t *testing.T) {
	if _, err := Compile(".a { color: red; } .a { color: blue; }"); err == nil {
		t.Fatal("重复规则应拒绝")
	}
}

// TestCommentsAndMalformedNumbers:CSS 注释被忽略;畸形数字保持字符串而非截断。
func TestCommentsAndMalformedNumbers(t *testing.T) {
	tokens, err := Compile("/* 顶部说明 */\n.a {\n  /* 内联 */\n  font-size: 12;\n}\n")
	if err != nil {
		t.Fatalf("注释应被忽略: %v", err)
	}
	if tokens["a"]["font-size"] != 12.0 {
		t.Fatalf("font-size = %v", tokens["a"]["font-size"])
	}
	tokens, err = Compile(".a { gap: 1.2.3; }")
	if err != nil {
		t.Fatalf("畸形数字应保留为字符串而不是报错: %v", err)
	}
	if tokens["a"]["gap"] != "1.2.3" {
		t.Fatalf("畸形数字被截断: %v", tokens["a"]["gap"])
	}
}

// TestRejectInvalidHexColor:非法 #hex 必须在编译期拒绝(重放层 ui.Hex 会 panic)。
func TestRejectInvalidHexColor(t *testing.T) {
	for _, src := range []string{
		".a { color: #12; }",
		".a { background: #ggg; }",
		".a { color: #12345; }",
		".a { color: #1234567; }",
	} {
		if _, err := Compile(src); err == nil {
			t.Errorf("%q 应因非法颜色被拒绝", src)
		}
	}
	tokens, err := Compile(".a { color: #abc; background: #11223344; }")
	if err != nil {
		t.Fatalf("合法颜色应通过: %v", err)
	}
	if tokens["a"]["color"] != "#abc" || tokens["a"]["background"] != "#11223344" {
		t.Fatalf("颜色令牌 = %v", tokens["a"])
	}
}
