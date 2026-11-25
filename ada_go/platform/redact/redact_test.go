package redact_test

import (
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/redact"
)

// 1. 默认模式命中常见密钥。
func TestDefaultPatterns(t *testing.T) {
	r, err := redact.New(nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	cases := []string{
		"token=ghp_0123456789abcdefghijABCDEFGHIJ",
		"key=AKIAABCDEFGHIJKLMNOP",
		"sk-abcdefghijklmnopqrstuvwxyz",
	}
	for _, in := range cases {
		out := r.Apply(in)
		if out == in || !strings.Contains(out, redact.DefaultReplacement) {
			t.Fatalf("%q 未被脱敏: %q", in, out)
		}
	}
}

// 2. 无命中时原样返回。
func TestNoMatch(t *testing.T) {
	r, err := redact.New(nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if got := r.Apply("hello world"); got != "hello world" {
		t.Fatalf("无命中不应改写: %q", got)
	}
}

// 3. 自定义模式与替换文本。
func TestCustom(t *testing.T) {
	r, err := redact.New([]string{`secret-\d+`}, "***")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if got := r.Apply("a secret-42 b"); got != "a *** b" {
		t.Fatalf("自定义替换不正确: %q", got)
	}
}

// 4. 非法正则必须让构造失败(配置期早失败)。
func TestInvalidPattern(t *testing.T) {
	if _, err := redact.New([]string{"("}, ""); err == nil {
		t.Fatalf("非法正则应报错")
	}
}

// 5. 字节版本与字符串版本一致,空切片原样返回。
func TestApplyBytes(t *testing.T) {
	r, err := redact.New(nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	in := []byte("key=AKIAABCDEFGHIJKLMNOP")
	out := r.ApplyBytes(in)
	if strings.Contains(string(out), "AKIA") {
		t.Fatalf("字节版本未脱敏: %q", out)
	}
	if empty := r.ApplyBytes(nil); empty != nil {
		t.Fatalf("空输入应原样返回")
	}
}
