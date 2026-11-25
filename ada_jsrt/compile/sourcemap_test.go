package compile

import "testing"

func TestSourceMapLookup(t *testing.T) {
	sm, err := ParseSourceMap([]byte(`{"version":3,"sources":["plugin.ts"],"names":[],"mappings":"AAAA;AAyCA"}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	source, line, col, ok := sm.Lookup(2, 5)
	if !ok || source != "plugin.ts" || line != 42 || col != 1 {
		t.Fatalf("映射不符: source=%q line=%d col=%d ok=%v", source, line, col, ok)
	}
	if _, _, _, ok := sm.Lookup(3, 0); ok {
		t.Fatal("超出 mappings 的行不应命中")
	}
}

func TestSourceMapInvalidVLQ(t *testing.T) {
	_, err := ParseSourceMap([]byte(`{"version":3,"sources":[],"names":[],"mappings":"!"}`))
	if err == nil {
		t.Fatal("非法 VLQ 应返回错误")
	}
}
