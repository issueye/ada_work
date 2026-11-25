package kernel

import (
	"encoding/json"
	"time"
)

// Config 是插件配置的只读快照：schema 的默认值已补齐、已校验。
//
// 它不直接暴露 map，是为了让插件用类型化读取而不是到处断言；
// Decode 用于把配置一次性解到插件自己的 struct。
type Config struct {
	raw map[string]any
}

// NewConfig 用原始 map 构造配置；通常由内核在装配时调用。
func NewConfig(m map[string]any) Config {
	if m == nil {
		m = map[string]any{}
	}
	return Config{raw: m}
}

// Raw 返回底层 map。调用方不得修改（内核保证同一份配置可能被多个读者共享）。
func (c Config) Raw() map[string]any { return c.raw }

// Has 判断字段是否存在。
func (c Config) Has(key string) bool {
	_, ok := c.raw[key]
	return ok
}

// String 读取字符串，缺省返回 ""。
func (c Config) String(key string) string {
	if v, ok := c.raw[key].(string); ok {
		return v
	}
	return ""
}

// Bool 读取布尔值。
func (c Config) Bool(key string) bool {
	if v, ok := c.raw[key].(bool); ok {
		return v
	}
	return false
}

// Int 读取整数；JSON 解码出的数字是 float64，这里一并接受。
func (c Config) Int(key string) int {
	switch v := c.raw[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

// Int64 读取 64 位整数。
func (c Config) Int64(key string) int64 {
	switch v := c.raw[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// Duration 读取时长：接受 "10s" 这类字符串，也接受毫秒数字。
func (c Config) Duration(key string) time.Duration {
	switch v := c.raw[key].(type) {
	case string:
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	case float64:
		return time.Duration(v) * time.Millisecond
	case int:
		return time.Duration(v) * time.Millisecond
	case int64:
		return time.Duration(v) * time.Millisecond
	}
	return 0
}

// Strings 读取字符串数组。
func (c Config) Strings(key string) []string {
	arr, ok := c.raw[key].([]any)
	if !ok {
		if ss, ok := c.raw[key].([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Decode 把配置解到插件自己的结构体。字段名按 JSON tag 匹配。
func (c Config) Decode(v any) error {
	b, err := json.Marshal(c.raw)
	if err != nil {
		return Wrap(CodeConfig, err, "配置无法序列化")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return Wrap(CodeConfig, err, "配置无法解码到目标结构")
	}
	return nil
}
