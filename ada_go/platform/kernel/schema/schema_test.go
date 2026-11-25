package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel/schema"
)

// numEqual 比较数值:Parse 用 UseNumber,默认值可能是 json.Number 而非 float64。
func numEqual(v any, want float64) bool {
	switch n := v.(type) {
	case float64:
		return n == want
	case int:
		return float64(n) == want
	case int64:
		return float64(n) == want
	case json.Number:
		f, err := n.Float64()
		return err == nil && f == want
	}
	return false
}

func mustParse(t *testing.T, src string) *schema.Schema {
	t.Helper()
	s, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatalf("解析 schema 失败: %v", err)
	}
	return s
}

// 1. 未知关键字在任意层级都必须报错,而不是静默忽略。
func TestRejectsUnknownKeywordAtAnyDepth(t *testing.T) {
	cases := map[string]string{
		"顶层":      `{"type":"object","unknownKeyword":1}`,
		"属性内":     `{"type":"object","properties":{"n":{"type":"integer","minimun":5}}}`,
		"items 内": `{"type":"array","items":{"type":"string","maxLenght":3}}`,
		"oneOf 内": `{"oneOf":[{"type":"string","patern":"x"}]}`,
	}
	for name, src := range cases {
		if _, err := schema.Parse([]byte(src)); err == nil {
			t.Fatalf("%s: 未知关键字应被拒绝,实际通过: %s", name, src)
		}
	}
}

// 2. type 取值必须来自白名单,否则整份 schema 会退化成「什么都通过」。
func TestRejectsUnknownType(t *testing.T) {
	for _, src := range []string{
		`{"type":"integerr"}`,
		`{"type":"object","properties":{"n":{"type":"integerr"}}}`,
		`{"type":["string","strng"]}`,
		`{"type":[]}`,
		`{"type":123}`,
	} {
		if _, err := schema.Parse([]byte(src)); err == nil {
			t.Fatalf("未知 type 应被拒绝,实际通过: %s", src)
		}
	}
}

// 3. 默认值、边界与 additionalProperties 仍然工作。
func TestDefaultsAndConstraints(t *testing.T) {
	s := mustParse(t, `{
	  "type": "object",
	  "properties": {
	    "n": {"type": "integer", "default": 3, "minimum": 1, "maximum": 10},
	    "name": {"type": "string", "default": "ada", "pattern": "^[a-z]+$"}
	  },
	  "additionalProperties": false
	}`)

	out, err := s.Apply(map[string]any{})
	if err != nil {
		t.Fatalf("补默认值失败: %v", err)
	}
	m := out.(map[string]any)
	if !numEqual(m["n"], 3) || m["name"] != "ada" {
		t.Fatalf("默认值不正确: %v", m)
	}
	if _, err := s.Apply(map[string]any{"n": 0}); err == nil {
		t.Fatalf("n=0 违反 minimum=1,应报错")
	}
	if _, err := s.Apply(map[string]any{"n": 11}); err == nil {
		t.Fatalf("n=11 违反 maximum=10,应报错")
	}
	if _, err := s.Apply(map[string]any{"name": "ADA"}); err == nil {
		t.Fatalf("name=ADA 不匹配 pattern,应报错")
	}
	if _, err := s.Apply(map[string]any{"extra": 1}); err == nil {
		t.Fatalf("additionalProperties=false 时未知字段应报错")
	}
}

// 4. 嵌套对象同样受 additionalProperties 约束。
func TestNestedAdditionalProperties(t *testing.T) {
	s := mustParse(t, `{
	  "type": "object",
	  "properties": {
	    "inner": {
	      "type": "object",
	      "properties": {"a": {"type": "integer"}},
	      "additionalProperties": false
	    }
	  }
	}`)
	if _, err := s.Apply(map[string]any{"inner": map[string]any{"a": 1}}); err != nil {
		t.Fatalf("合法嵌套对象应通过: %v", err)
	}
	if _, err := s.Apply(map[string]any{"inner": map[string]any{"b": 1}}); err == nil {
		t.Fatalf("嵌套未知字段应报错")
	}
}

// 5. $ref 必须真的解析:默认值与约束都要从被引用节点生效(曾经「占位不解析」= 全部放行)。
func TestRefResolvesDefaultsAndConstraints(t *testing.T) {
	s := mustParse(t, `{
	  "type": "object",
	  "$defs": {
	    "Port": {"type": "integer", "default": 8080, "minimum": 1, "maximum": 65535}
	  },
	  "properties": {"port": {"$ref": "#/$defs/Port"}}
	}`)

	out, err := s.Apply(map[string]any{})
	if err != nil {
		t.Fatalf("$ref 默认值应生效: %v", err)
	}
	if got := out.(map[string]any)["port"]; !numEqual(got, 8080) {
		t.Fatalf("$ref 指向的默认值应为 8080,实际 %v", got)
	}
	if _, err := s.Apply(map[string]any{"port": 0}); err == nil {
		t.Fatalf("$ref 指向的 minimum 应生效")
	}
	if _, err := s.Apply(map[string]any{"port": 70000}); err == nil {
		t.Fatalf("$ref 指向的 maximum 应生效")
	}
}

// 6. 坏的 $ref 必须报错:不存在、外部引用、成环都不能静默跳过。
func TestBadRefsFailLoudly(t *testing.T) {
	if _, err := schema.Parse([]byte(`{"$defs":{"A":{"$ref":"other.json#/A"}}}`)); err == nil {
		t.Fatalf("外部 $ref 应在解析期被拒绝")
	}
	s := mustParse(t, `{
	  "type": "object",
	  "$defs": {"A": {"$ref": "#/$defs/B"}, "B": {"$ref": "#/$defs/A"}},
	  "properties": {"x": {"$ref": "#/$defs/A"}}
	}`)
	if _, err := s.Apply(map[string]any{"x": 1}); err == nil || !strings.Contains(err.Error(), "$ref") {
		t.Fatalf("成环的 $ref 应报错,实际 %v", err)
	}
	missing := mustParse(t, `{"type":"object","properties":{"x":{"$ref":"#/$defs/Nope"}}}`)
	if _, err := missing.Apply(map[string]any{}); err == nil {
		t.Fatalf("指向不存在节点的 $ref 应报错")
	}
}

// 7. 联合类型仍然可用(白名单不误伤合法写法)。
func TestUnionType(t *testing.T) {
	s := mustParse(t, `{
	  "type": "object",
	  "properties": {"v": {"type": ["string", "null"]}}
	}`)
	if _, err := s.Apply(map[string]any{"v": "x"}); err != nil {
		t.Fatalf("联合类型应接受 string: %v", err)
	}
	if _, err := s.Apply(map[string]any{"v": nil}); err != nil {
		t.Fatalf("联合类型应接受 null: %v", err)
	}
	if _, err := s.Apply(map[string]any{"v": 3}); err == nil {
		t.Fatalf("联合类型应拒绝不在集合内的类型")
	}
}
