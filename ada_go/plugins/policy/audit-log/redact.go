// Package auditlog 的 payload 脱敏。
//
// 审计是最后一道可观测边界:观察事件可能带路径/错误文本,审批事件可能带手动构造的
// target。这里按「敏感 key + URL」做通用脱敏,避免依赖每个 emitter 自己记得脱敏。
package auditlog

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

var sensitiveKeys = map[string]bool{
	"authorization": true,
	"cookie":        true,
	"set-cookie":    true,
	"token":         true,
	"access_token":  true,
	"refresh_token": true,
	"password":      true,
	"passwd":        true,
	"secret":        true,
	"client_secret": true,
	"api_key":       true,
	"apikey":        true,
	"private_key":   true,
	"credential":    true,
	"credentials":   true,
}

var urlKeys = map[string]bool{
	"url": true, "uri": true, "target": true, "endpoint": true, "href": true, "location": true,
}

var embeddedURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

// redactPayload 把任意事件 payload 归一成 JSON 值并递归脱敏。
//
// 非 JSON 可序列化的值原样返回(审计不能因为脱敏失败而丢事件)。
func redactPayload(v any) any {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return v
	}
	return redactValue(out)
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			key := strings.ToLower(k)
			if sensitiveKeys[key] {
				x[k] = "REDACTED"
				continue
			}
			if urlKeys[key] {
				if s, ok := val.(string); ok {
					x[k] = redactURL(s)
					continue
				}
			}
			x[k] = redactValue(val)
		}
		return x
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
		return x
	case string:
		return redactText(x)
	default:
		return v
	}
}

func redactText(s string) string {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return redactURL(s)
	}
	return embeddedURL.ReplaceAllStringFunc(s, redactURL)
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	return u.String()
}
