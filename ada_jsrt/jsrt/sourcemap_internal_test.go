package jsrt

import (
	"testing"

	"github.com/issueye/ada_jsrt/compile"
)

func TestMapStackUsesSourceMap(t *testing.T) {
	r := &Runtime{art: &compile.Artifact{SourceMap: map[string]string{
		"plugin.ts": `{"version":3,"sources":["plugin.ts"],"names":[],"mappings":"AAAA;AAyCA"}`,
	}}}
	stack := "    at boom (plugin.ts:2:5)"
	got := r.mapStack(stack)
	want := "    at boom (plugin.ts:42:1)"
	if got != want {
		t.Fatalf("stack 映射不符:\n got: %s\nwant: %s", got, want)
	}
}

func TestMapStackWithoutMap(t *testing.T) {
	r := &Runtime{art: &compile.Artifact{}}
	stack := "    at boom (plugin.ts:2:5)"
	if got := r.mapStack(stack); got != stack {
		t.Fatalf("没有 sourcemap 时应原样返回: %s", got)
	}
}
