package main

// 参数重排回归:`adat run -inspector examples/chat` 这类「布尔 flag 在位置
// 参数之前」的写法不能被吞掉目录参数。

import (
	"flag"
	"testing"
)

func TestReorderArgsBoolFlag(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	insp := fs.Bool("inspector", false, "")
	out := fs.String("o", "", "")
	width := fs.Int("width", 360, "")

	args := reorderArgs([]string{"examples/chat", "-inspector", "-o", "dist/x", "-width", "800"}, fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if !*insp {
		t.Fatal("-inspector 未生效")
	}
	if *out != "dist/x" {
		t.Fatalf("-o = %q", *out)
	}
	if *width != 800 {
		t.Fatalf("-width = %d", *width)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "examples/chat" {
		t.Fatalf("位置参数 = %v, want examples/chat", fs.Args())
	}
}

func TestReorderArgsValueBeforePositional(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	out := fs.String("o", "", "")
	args := reorderArgs([]string{"-o", "out", "examples/counter"}, fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if *out != "out" || fs.NArg() != 1 || fs.Arg(0) != "examples/counter" {
		t.Fatalf("重排结果错误: -o=%q args=%v", *out, fs.Args())
	}
}
