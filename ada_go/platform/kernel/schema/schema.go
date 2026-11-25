// Package schema 实现插件配置所用的 JSON Schema 子集。
//
// 为什么自己实现:配置校验必须能在纯 Go 里跑、不引入第三方依赖,
// 且 TS 侧与 Go 侧共用同一份 schema 文件(插件目录里的 config.schema.json)。
// 支持的关键字见 docs/kernel-api.md 的 D2,超出范围的关键字会被明确报错,
// 而不是静默忽略——静默忽略会让作者以为约束生效了。
//
// 同样的原则适用于嵌套节点与 type 取值:任一层的拼写错误都必须拒绝,
// 否则一个 "integerr" 会让整份 schema 退化成「什么都通过」。
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Schema 是一份 JSON Schema(子集)。它不可变,可被多个插件并发读取。
type Schema struct {
	root map[string]any
}

// Parse 从 JSON 字节解析 schema,并递归校验关键字与 type 取值。
func Parse(b []byte) (*Schema, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("schema: 解析失败: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("schema: 顶层必须是对象")
	}
	if err := checkKeywords(m, "$"); err != nil {
		return nil, err
	}
	normalizeNumbers(m)
	return &Schema{root: m}, nil
}

// FromMap 把已经是 Go 值的 schema 包起来,供测试与内联配置使用。
// 它不做关键字校验(调用方已持有构造好的 map);解析期校验请走 Parse。
func FromMap(m map[string]any) *Schema {
	return &Schema{root: normalizeNumbers(m).(map[string]any)}
}

// Raw 返回底层 map,调用方不得修改它。
func (s *Schema) Raw() map[string]any { return s.root }

// Apply 先按默认值补全配置,再校验;返回补全后的对象。
//
// 默认值只作用于对象属性;数组的 items 默认值不补(避免把不存在的元素凭空造出来)。
func (s *Schema) Apply(v any) (any, error) {
	out, err := s.apply(s.root, v, "$")
	if err != nil {
		return nil, err
	}
	if err := s.validate(s.root, out, "$"); err != nil {
		return nil, err
	}
	return out, nil
}

// Validate 只校验,不补默认值。
func (s *Schema) Validate(v any) error { return s.validate(s.root, v, "$") }

var knownKeywords = map[string]bool{
	"$schema": true, "$id": true, "$defs": true, "$ref": true, "title": true,
	"description": true, "type": true, "properties": true, "required": true,
	"default": true, "enum": true, "const": true, "items": true,
	"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
	"pattern": true, "additionalProperties": true, "oneOf": true, "minItems": true, "maxItems": true,
}

// allowedTypes 是 type 允许的取值。白名单之外一律报错:
// oneType 对未知类型返回 true 会让校验整体失效,这是最危险的静默失败。
var allowedTypes = map[string]bool{
	"object": true, "array": true, "string": true,
	"boolean": true, "null": true, "integer": true, "number": true,
}

// checkKeywords 递归校验:每一层都拒绝未知关键字,并检查 type 取值合法。
func checkKeywords(node map[string]any, path string) error {
	for k, v := range node {
		if !knownKeywords[k] && !strings.HasPrefix(k, "$") {
			return fmt.Errorf("schema: %s: 不支持的关键字 %q", path, k)
		}
		switch k {
		case "type":
			if err := checkTypeValue(v, path); err != nil {
				return err
			}
		case "$ref":
			ref, ok := v.(string)
			if !ok || !strings.HasPrefix(ref, "#/") {
				return fmt.Errorf("schema: %s.$ref: 只支持文档内引用(#/...),得到 %v", path, v)
			}
		case "properties", "$defs":
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("schema: %s.%s: 必须是对象", path, k)
			}
			for name, child := range sub {
				cm, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("schema: %s.%s.%s: 必须是 schema 对象", path, k, name)
				}
				if err := checkKeywords(cm, path+"."+k+"."+name); err != nil {
					return err
				}
			}
		case "items", "additionalProperties":
			if cm, ok := v.(map[string]any); ok {
				if err := checkKeywords(cm, path+"."+k); err != nil {
					return err
				}
			}
		case "oneOf":
			arr, ok := v.([]any)
			if !ok {
				return fmt.Errorf("schema: %s.oneOf: 必须是数组", path)
			}
			for i, child := range arr {
				cm, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("schema: %s.oneOf[%d]: 必须是 schema 对象", path, i)
				}
				if err := checkKeywords(cm, fmt.Sprintf("%s.oneOf[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkTypeValue(v any, path string) error {
	switch t := v.(type) {
	case string:
		if !allowedTypes[t] {
			return fmt.Errorf("schema: %s.type: 未知类型 %q", path, t)
		}
	case []any:
		if len(t) == 0 {
			return fmt.Errorf("schema: %s.type: 类型数组不能为空", path)
		}
		for _, x := range t {
			s, ok := x.(string)
			if !ok || !allowedTypes[s] {
				return fmt.Errorf("schema: %s.type: 未知类型 %v", path, x)
			}
		}
	default:
		return fmt.Errorf("schema: %s.type: 必须是字符串或字符串数组", path)
	}
	return nil
}

func (s *Schema) apply(node map[string]any, v any, path string) (any, error) {
	node, err := s.resolve(node, path)
	if err != nil {
		return nil, err
	}
	types := typeSet(node)
	_, hasType := node["type"]
	objectLike := types["object"] || !hasType
	arrayLike := types["array"]

	if v == nil {
		if !objectLike {
			return v, nil
		}
		v = map[string]any{}
	}
	switch {
	case objectLike:
		m, ok := v.(map[string]any)
		if !ok {
			// 无 type 时保持宽松;联合类型里匹配到别的分支也原样返回。
			if !hasType || matchesAnyType(types, v) {
				return v, nil
			}
			return nil, fmt.Errorf("%s: 期望 object,得到 %T", path, v)
		}
		out := make(map[string]any, len(m)+4)
		for k, val := range m {
			out[k] = val
		}
		props, _ := node["properties"].(map[string]any)
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, _ := props[k].(map[string]any)
			if sub == nil {
				continue
			}
			// 先展开 $ref,默认值定义在被引用节点上,不能只看引用节点本身。
			sub, err := s.resolve(sub, path+"."+k)
			if err != nil {
				return nil, err
			}
			cur, exists := out[k]
			if !exists {
				if def, has := sub["default"]; has {
					out[k] = def
					continue
				}
				if t, _ := sub["type"].(string); t == "object" {
					out[k] = map[string]any{}
					cur, exists = out[k], true
				} else {
					continue
				}
			}
			nv, err := s.apply(sub, cur, path+"."+k)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case arrayLike:
		arr, ok := v.([]any)
		if !ok {
			if hasType && !matchesAnyType(types, v) {
				return nil, fmt.Errorf("%s: 期望 array,得到 %T", path, v)
			}
			return v, nil
		}
		items, _ := node["items"].(map[string]any)
		if items == nil {
			return v, nil
		}
		out := make([]any, len(arr))
		for i, item := range arr {
			nv, err := s.apply(items, item, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	default:
		return v, nil
	}
}

func (s *Schema) validate(node map[string]any, v any, path string) error {
	node, err := s.resolve(node, path)
	if err != nil {
		return err
	}
	if typ, ok := node["type"]; ok {
		if !typeMatches(typ, v) {
			return fmt.Errorf("%s: 期望类型 %v,得到 %T", path, typ, v)
		}
	}
	if c, ok := node["const"]; ok && !equalValue(c, v) {
		return fmt.Errorf("%s: 必须是常量 %v", path, c)
	}
	if enum, ok := node["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if equalValue(e, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: 不在枚举范围内", path)
		}
	}
	if ones, ok := node["oneOf"].([]any); ok {
		matched := 0
		for _, sub := range ones {
			if m, ok := sub.(map[string]any); ok {
				if err := s.validate(m, v, path); err == nil {
					matched++
				}
			}
		}
		if matched != 1 {
			return fmt.Errorf("%s: oneOf 必须恰好匹配一个分支(实际 %d)", path, matched)
		}
	}

	switch val := v.(type) {
	case map[string]any:
		props, _ := node["properties"].(map[string]any)
		required, _ := node["required"].([]any)
		for _, r := range required {
			name, _ := r.(string)
			if _, ok := val[name]; !ok {
				return fmt.Errorf("%s: 缺少必填字段 %q", path, name)
			}
		}
		for k, sub := range props {
			cur, ok := val[k]
			if !ok {
				continue
			}
			if m, ok := sub.(map[string]any); ok {
				if err := s.validate(m, cur, path+"."+k); err != nil {
					return err
				}
			}
		}
		if ap, ok := node["additionalProperties"].(bool); ok && !ap {
			for k := range val {
				if _, known := props[k]; !known {
					return fmt.Errorf("%s: 未知字段 %q", path, k)
				}
			}
		}
	case []any:
		items, _ := node["items"].(map[string]any)
		limit, _ := toFloat(node["maxItems"])
		if low, ok := toFloat(node["minItems"]); ok && float64(len(val)) < low {
			return fmt.Errorf("%s: 元素数量少于 %v", path, low)
		}
		if limit > 0 && float64(len(val)) > limit {
			return fmt.Errorf("%s: 元素数量超过 %v", path, limit)
		}
		if items != nil {
			for i, item := range val {
				if err := s.validate(items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case string:
		if n, ok := toFloat(node["minLength"]); ok && float64(len([]rune(val))) < n {
			return fmt.Errorf("%s: 长度小于 %v", path, n)
		}
		if n, ok := toFloat(node["maxLength"]); ok && float64(len([]rune(val))) > n {
			return fmt.Errorf("%s: 长度大于 %v", path, n)
		}
		if pat, ok := node["pattern"].(string); ok && pat != "" {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("%s: schema 的 pattern 非法: %w", path, err)
			}
			if !re.MatchString(val) {
				return fmt.Errorf("%s: 不匹配 pattern %q", path, pat)
			}
		}
	case float64, json.Number, int, int64, int32:
		num, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("%s: 期望数字,得到 %T", path, v)
		}
		if min, ok := toFloat(node["minimum"]); ok && num < min {
			return fmt.Errorf("%s: 小于最小值 %v", path, min)
		}
		if max, ok := toFloat(node["maximum"]); ok && num > max {
			return fmt.Errorf("%s: 大于最大值 %v", path, max)
		}
	}
	return nil
}

// resolve 展开 $ref。只支持文档内引用(#/...),链过深按循环引用报错。
//
// 为什么必须解析而不是「占位跳过」:跳过等于该节点不做任何校验,
// 一份写了 $ref 的 schema 会静默退化成「什么都通过」。
func (s *Schema) resolve(node map[string]any, path string) (map[string]any, error) {
	for depth := 0; ; depth++ {
		ref, ok := node["$ref"].(string)
		if !ok || ref == "" {
			return node, nil
		}
		if depth >= 32 {
			return nil, fmt.Errorf("%s: $ref 链过深(可能成环)", path)
		}
		if !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("%s: 只支持文档内 $ref(#/...),得到 %q", path, ref)
		}
		var cur any = s.root
		for _, raw := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			seg := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: $ref %q 指向的路径不是对象", path, ref)
			}
			next, ok := m[seg]
			if !ok {
				return nil, fmt.Errorf("%s: $ref %q 指向不存在的节点 %q", path, ref, seg)
			}
			cur = next
		}
		target, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: $ref %q 必须指向对象", path, ref)
		}
		node = target
	}
}

// typeSet 把 type 归一成集合;未声明 type 时返回空集合。
func typeSet(node map[string]any) map[string]bool {
	out := map[string]bool{}
	switch t := node["type"].(type) {
	case string:
		out[t] = true
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func matchesAnyType(types map[string]bool, v any) bool {
	for t := range types {
		if oneType(t, v) {
			return true
		}
	}
	return false
}

func typeMatches(typ any, v any) bool {
	switch t := typ.(type) {
	case string:
		return oneType(t, v)
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && oneType(s, v) {
				return true
			}
		}
		return false
	}
	return true
}

func oneType(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "integer":
		n, ok := toFloat(v)
		return ok && n == float64(int64(n))
	case "number":
		_, ok := toFloat(v)
		return ok
	}
	// 未知类型一律不匹配:typeMatches 的默认 true 会让校验整体失效。
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func equalValue(a, b any) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return af == bf
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

// normalizeNumbers 递归把结构体里出现的 int 归一为 float64,
// 避免 Go 侧构造的配置与 JSON 解码出的配置在校验时表现不一致。
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = normalizeNumbers(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = normalizeNumbers(val)
		}
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	}
	return v
}
