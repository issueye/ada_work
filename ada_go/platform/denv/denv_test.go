package denv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/issueye/ada_go_v2/platform/denv"
)

// TestParse 覆盖语法面:注释、export、引号、转义、行内注释、空值、BOM/CRLF、重复键。
func TestParse(t *testing.T) {
	text := "\ufeff# 注释行\r\n" +
		"ADA_LLM_API_KEY=sk-plain\r\n" +
		"export ADA_LLM_MODEL = ada-1\r\n" +
		"ADA_LLM_ENDPOINT=\"https://api.example/v1\"\r\n" +
		"QUOTED='a b # not comment'\r\n" +
		"ESCAPED=\"line1\\nline2\\t\\\"q\\\"\\\\end\"\r\n" +
		"EMPTY=\r\n" +
		"HASH=https://example.com/a#frag\r\n" +
		"INLINE=value # 行内注释\r\n" +
		"INLINE2=value#frag\r\n" +
		"DUP=first\r\n" +
		"DUP=second\r\n"
	vars, err := denv.Parse(text)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := map[string]string{
		"ADA_LLM_API_KEY":  "sk-plain",
		"ADA_LLM_MODEL":    "ada-1",
		"ADA_LLM_ENDPOINT": "https://api.example/v1",
		"QUOTED":           "a b # not comment",
		"ESCAPED":          "line1\nline2\t\"q\"\\end",
		"EMPTY":            "",
		"HASH":             "https://example.com/a#frag",
		"INLINE":           "value",
		"INLINE2":          "value#frag",
		"DUP":              "second",
	}
	if len(vars) != len(want) {
		t.Fatalf("键数不对: %d vs %d (%v)", len(vars), len(want), vars)
	}
	for k, v := range want {
		if vars[k] != v {
			t.Fatalf("%s = %q,期望 %q", k, vars[k], v)
		}
	}
}

// TestParseErrors 守住 fail-closed:坏行带行号报错,而不是静默跳过。
func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"缺等号":    "KEY\n",
		"非法键名":   "1KEY=x\n",
		"引号未闭合":  "K=\"abc\n",
		"引号后有多余": "K=\"abc\"x\n",
		"单引号未闭合": "K='abc\n",
	}
	for name, text := range cases {
		if _, err := denv.Parse(text); err == nil {
			t.Fatalf("%s 应报错", name)
		}
	}
	// 行号必须指向出错的那一行。
	if _, err := denv.Parse("OK=1\nBAD\n"); err == nil {
		t.Fatal("应报错")
	}
}

// TestApplyPrecedence 验证真实环境优先、文件之间后者覆盖前者。
func TestApplyPrecedence(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.env")
	over := filepath.Join(dir, "over.env")
	if err := os.WriteFile(base, []byte("FROM_FILE=base\nLAYERED=base\nREAL=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte("LAYERED=over\nREAL=file-over\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("REAL", "process")
	// 清掉可能存在的键,避免宿主机环境影响断言。
	t.Setenv("FROM_FILE", "")
	os.Unsetenv("FROM_FILE")
	os.Unsetenv("LAYERED")

	applied, err := denv.Apply(base, over)
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if applied != 2 {
		t.Fatalf("应设置 2 个键(LAYERED 后覆盖前),实际 %d", applied)
	}
	if got := os.Getenv("FROM_FILE"); got != "base" {
		t.Fatalf("FROM_FILE = %q", got)
	}
	if got := os.Getenv("LAYERED"); got != "over" {
		t.Fatalf("LAYERED = %q,后一个文件应覆盖前一个", got)
	}
	if got := os.Getenv("REAL"); got != "process" {
		t.Fatalf("REAL = %q,真实环境必须优先", got)
	}
}

// TestLoadFileMissing 不存在不是错误。
func TestLoadFileMissing(t *testing.T) {
	vars, err := denv.LoadFile(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil || vars != nil {
		t.Fatalf("缺失文件应返回 (nil,nil),得到 %v %v", vars, err)
	}
}
