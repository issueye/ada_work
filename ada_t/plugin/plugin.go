// Package plugin 定义 ada_t 插件清单(manifest)与能力声明。
//
// 插件是一个编译好的组件包(.aui)加上一份 plugin.json:
//
//	{
//	  "id": "hello",
//	  "name": "示例插件",
//	  "version": "0.1.0",
//	  "entry": "Hello",
//	  "deps": [],
//	  "capabilities": ["host.bots.list"],
//	  "slots": [
//	    { "slot": "sidebar.item", "component": "HelloSidebar", "order": 10 }
//	  ],
//	  "sha256": "sha256:…",              // 可选:产物摘要(内部插件校验完整性)
//	  "signature": "base64…",            // 可选:对 sha256 的 ed25519 签名(第三方)
//	  "publisher": "issueye"
//	}
//
// 插件只声明「槽位 + 组件」,放在哪里、多大、什么主题由宿主决定;
// 能调哪些宿主服务由 capabilities 白名单决定(默认拒绝)。
package plugin

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ManifestName 是插件目录里的清单文件名。
const ManifestName = "plugin.json"

// 宿主可注册的标准插槽名。宿主也可以自定义其它名字(通过 App.RegisterSlot)。
const (
	SlotSidebarItem     = "sidebar.item"      // 侧栏条目
	SlotChatHeader      = "chat.header_action" // 会话头部动作按钮
	SlotComposerAction  = "composer.action"    // 输入框旁的动作
	SlotMessageRenderer = "message.renderer"   // 自定义消息渲染
	SlotSettingsTab     = "settings.tab"       // 设置页签
	SlotCommand         = "command"            // 命令面板条目
)

// Contribution 是插件声明的一个槽位贡献。
type Contribution struct {
	Slot      string `json:"slot"`
	Component string `json:"component"`
	// Order 是同一槽位内的排序(小的在前,缺省 0)。
	Order int `json:"order,omitempty"`
	// Width/Height 是建议尺寸(DIP);0 表示由宿主决定。
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Title 是宿主可展示的标题(可选)。
	Title string `json:"title,omitempty"`
}

// Manifest 是插件清单。
type Manifest struct {
	ID          string         `json:"id"`
	Name        string         `json:"name,omitempty"`
	Version     string         `json:"version"`
	Entry       string         `json:"entry"`
	Deps        []string       `json:"deps,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Slots       []Contribution `json:"slots,omitempty"`
	// SHA256 是编译产物摘要("sha256:<hex>");存在即校验。
	SHA256 string `json:"sha256,omitempty"`
	// Signature 是 manifest.SHA256(字符串本身)的 ed25519 签名(base64)。
	Signature string `json:"signature,omitempty"`
	Publisher string `json:"publisher,omitempty"`
	Description string `json:"description,omitempty"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Validate 校验清单的基本形状;不做信任判断(那由宿主策略决定)。
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("plugin: 清单为 nil")
	}
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("plugin: 非法插件 id %q(只允许字母数字与 _.-,以字母数字开头,≤64)", m.ID)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("plugin %s: 缺少 version", m.ID)
	}
	if strings.TrimSpace(m.Entry) == "" {
		return fmt.Errorf("plugin %s: 缺少 entry(入口组件名)", m.ID)
	}
	seenDeps := map[string]bool{}
	for _, d := range m.Deps {
		if d == m.ID {
			return fmt.Errorf("plugin %s: deps 不能包含自己", m.ID)
		}
		if !idPattern.MatchString(d) {
			return fmt.Errorf("plugin %s: 非法依赖 id %q", m.ID, d)
		}
		if seenDeps[d] {
			return fmt.Errorf("plugin %s: 依赖 %q 重复", m.ID, d)
		}
		seenDeps[d] = true
	}
	seenCap := map[string]bool{}
	for _, c := range m.Capabilities {
		c = strings.TrimSpace(c)
		if c == "" || strings.ContainsAny(c, " \t\n") {
			return fmt.Errorf("plugin %s: 非法能力名 %q", m.ID, c)
		}
		if seenCap[c] {
			return fmt.Errorf("plugin %s: 能力 %q 重复", m.ID, c)
		}
		seenCap[c] = true
	}
	seenSlot := map[string]bool{}
	for i, c := range m.Slots {
		if strings.TrimSpace(c.Slot) == "" {
			return fmt.Errorf("plugin %s: slots[%d] 缺少 slot", m.ID, i)
		}
		if strings.TrimSpace(c.Component) == "" {
			return fmt.Errorf("plugin %s: slots[%d] 缺少 component", m.ID, i)
		}
		if c.Width < 0 || c.Height < 0 {
			return fmt.Errorf("plugin %s: slots[%d] 的尺寸不能为负", m.ID, i)
		}
		key := c.Slot + "/" + c.Component
		if seenSlot[key] {
			return fmt.Errorf("plugin %s: 槽位贡献 %s 重复", m.ID, key)
		}
		seenSlot[key] = true
	}
	if m.SHA256 != "" {
		h := strings.TrimPrefix(m.SHA256, "sha256:")
		if len(h) != 64 || !isHex(h) {
			return fmt.Errorf("plugin %s: sha256 必须是 64 位十六进制", m.ID)
		}
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// Allows 报告清单是否声明了某个能力。支持结尾通配:
// "host.*" 匹配 "host.bots.list","*" 匹配全部。
func (m *Manifest) Allows(capability string) bool {
	if m == nil {
		return false
	}
	for _, c := range m.Capabilities {
		if c == capability || wildcardMatch(c, capability) {
			return true
		}
	}
	return false
}

// wildcardMatch 按 "*" 分段顺序匹配,如 "host.*.append" 匹配
// "host.bots.append";"*" 匹配任意。
func wildcardMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return false
	}
	parts := strings.Split(pattern, "*")
	if parts[0] != "" && !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = strings.TrimPrefix(s, parts[0])
	last := parts[len(parts)-1]
	if last != "" {
		if !strings.HasSuffix(s, last) || len(s) < len(last) {
			return false
		}
		s = s[:len(s)-len(last)]
	}
	for _, p := range parts[1 : len(parts)-1] {
		if p == "" {
			continue
		}
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return true
}

// AllowsCall 报告是否允许调用 service.method。
func (m *Manifest) AllowsCall(service, method string) bool {
	return m.Allows(service + "." + strings.TrimPrefix(method, service+"."))
}

// Parse 解析清单 JSON。
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("plugin: 清单不是合法 JSON: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Load 读取目录里的 plugin.json。
func Load(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestName)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plugin: 读取清单失败 %s: %w", path, err)
	}
	return Parse(b)
}

// DigestHex 返回不带 "sha256:" 前缀的摘要串(签名消息体)。
func (m *Manifest) DigestHex() string {
	return strings.TrimPrefix(m.SHA256, "sha256:")
}

// VerifySignature 校验 manifest.Signature 是 publisher 对摘要字符串的 ed25519 签名。
// keys 为空时返回错误(调用方要求签名却未配置公钥)。
func (m *Manifest) VerifySignature(keys ...ed25519.PublicKey) error {
	if m == nil || m.Signature == "" {
		return fmt.Errorf("plugin: 清单没有签名")
	}
	if m.SHA256 == "" {
		return fmt.Errorf("plugin %s: 签名前必须先有 sha256 摘要", m.ID)
	}
	if len(keys) == 0 {
		return fmt.Errorf("plugin %s: 未配置发布者公钥", m.ID)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return fmt.Errorf("plugin %s: 签名不是合法 base64: %w", m.ID, err)
	}
	msg := []byte(m.SHA256)
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(k, msg, sig) {
			return nil
		}
	}
	return fmt.Errorf("plugin %s: 签名校验失败(没有匹配的发布者公钥)", m.ID)
}

// Sign 用私钥为摘要生成签名(打包工具与测试用)。
func Sign(m *Manifest, priv ed25519.PrivateKey) error {
	if m.SHA256 == "" {
		return fmt.Errorf("plugin %s: 签名前必须先有 sha256 摘要", m.ID)
	}
	sig := ed25519.Sign(priv, []byte(m.SHA256))
	m.Signature = base64.StdEncoding.EncodeToString(sig)
	return nil
}
