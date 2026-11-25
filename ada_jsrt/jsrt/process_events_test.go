package jsrt_test

import (
	"context"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/jsrt"
)

func waitProcessValue(t *testing.T, rt *jsrt.Runtime, export, want string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := rt.Call(ctx, export)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", export, err)
		}
		if s, _ := got.(string); s == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 未变为 %q，最后 %v", export, want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProcessUnhandledRejectionEvent(t *testing.T) {
	rt := newResident(t, `
import process from "node:process";
let seen = "";
process.on("unhandledRejection", (err: any) => {
  seen = err && err.message ? err.message : String(err);
});
setTimeout(() => { Promise.reject(new Error("boom")) }, 5);
export function get(): string { return seen; }
`, jsrt.Options{})
	waitProcessValue(t, rt, "get", "boom")
}

func TestProcessUncaughtExceptionEvent(t *testing.T) {
	rt := newResident(t, `
import process from "node:process";
let seen = "";
process.on("uncaughtException", (err: any) => {
  seen = err && err.message ? err.message : String(err);
});
setTimeout(() => { throw new Error("kaboom") }, 5);
export function get(): string { return seen; }
`, jsrt.Options{})
	waitProcessValue(t, rt, "get", "kaboom")
}
