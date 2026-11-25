package shellredact_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	shellredact "github.com/issueye/ada_go_v2/plugins/policy/shell-redact"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

type fakeShell struct {
	result seamshell.Result
	err    error
	chunks []string
}

func (f *fakeShell) Run(context.Context, seamshell.Command) (seamshell.Result, error) {
	return f.result, f.err
}
func (f *fakeShell) Stream(_ context.Context, _ seamshell.Command, onChunk func(seamshell.Chunk) error) (seamshell.Result, error) {
	for i, text := range f.chunks {
		if err := onChunk(seamshell.Chunk{Stream: "stdout", Text: text, Seq: i + 1}); err != nil {
			return f.result, err
		}
	}
	return f.result, f.err
}
func (f *fakeShell) Env() seamshell.Env { return nil }

// 1. stdout/stderr 命中脱敏。
func TestRedactsResult(t *testing.T) {
	fake := &fakeShell{result: seamshell.Result{
		Stdout: []byte("token=ghp_0123456789abcdefghijABCDEFGHIJ"),
		Stderr: []byte("key=AKIAABCDEFGHIJKLMNOP"),
	}}
	sh, err := shellredact.New(fake, nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	res, err := sh.Run(context.Background(), seamshell.Command{Program: "echo"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if strings.Contains(string(res.Stdout), "ghp_0123456789") || !strings.Contains(string(res.Stdout), shellredact.DefaultReplacement) {
		t.Fatalf("stdout 未脱敏: %q", res.Stdout)
	}
	if strings.Contains(string(res.Stderr), "AKIAABCDEFGHIJKLMNOP") {
		t.Fatalf("stderr 未脱敏: %q", res.Stderr)
	}
}

// 2. 无命中原样返回。
func TestNoMatch(t *testing.T) {
	fake := &fakeShell{result: seamshell.Result{Stdout: []byte("hello"), Stderr: []byte("bye")}}
	sh, err := shellredact.New(fake, nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	res, _ := sh.Run(context.Background(), seamshell.Command{Program: "echo"})
	if string(res.Stdout) != "hello" || string(res.Stderr) != "bye" {
		t.Fatalf("无命中不应改写: %q %q", res.Stdout, res.Stderr)
	}
}

// 3. 非法正则配置期报错。
func TestInvalidPattern(t *testing.T) {
	if _, err := shellredact.New(&fakeShell{}, []string{"("}, ""); err == nil {
		t.Fatalf("非法正则应报错")
	}
}

// 4. 流式块按块脱敏。
func TestRedactsChunks(t *testing.T) {
	fake := &fakeShell{chunks: []string{"ghp_0123456789abcdefghijABCDEFGHIJ", "ok"}}
	sh, err := shellredact.New(fake, nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	var got []string
	_, _ = sh.Stream(context.Background(), seamshell.Command{Program: "cat"}, func(c seamshell.Chunk) error {
		got = append(got, c.Text)
		return nil
	})
	if len(got) != 2 || strings.Contains(got[0], "ghp_") || got[1] != "ok" {
		t.Fatalf("流式块未脱敏: %v", got)
	}
}

// 5. 失败返回时结果仍脱敏。
func TestErrorResultRedacted(t *testing.T) {
	fake := &fakeShell{
		result: seamshell.Result{Stdout: []byte("ghp_0123456789abcdefghijABCDEFGHIJ")},
		err:    errors.New("boom"),
	}
	sh, err := shellredact.New(fake, nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	res, err := sh.Run(context.Background(), seamshell.Command{Program: "bad"})
	if err == nil {
		t.Fatalf("应返回错误")
	}
	if strings.Contains(string(res.Stdout), "ghp_") {
		t.Fatalf("失败结果也应脱敏: %q", res.Stdout)
	}
}
