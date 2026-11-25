// caps 相关装配辅助:把清单声明的能力灌进授权表。
package assemble

import (
	"fmt"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

// GrantManifests 按清单的 caps/trust 为插件授予能力。
//
// 规则(design §13.2):能力必须满足最低信任等级;trust 缺省视为 third-party。
// 任一插件的能力与 trust 冲突时整批失败,不部分授予——避免「装了一半才发现权限不够」。
func GrantManifests(broker *caps.Broker, manifests []*plugin.Manifest) error {
	if broker == nil {
		return fmt.Errorf("assemble: broker 不能为 nil")
	}
	for _, m := range manifests {
		if len(m.Caps) == 0 {
			continue
		}
		trust := caps.Trust(m.Trust)
		if trust == "" {
			trust = caps.TrustThirdParty
		}
		if !caps.ValidTrust(trust) {
			return fmt.Errorf("assemble: 插件 %q 的 trust 非法: %q", m.ID, m.Trust)
		}
		for _, spec := range m.Caps {
			c, err := caps.Parse(spec)
			if err != nil {
				return fmt.Errorf("assemble: 插件 %q 的能力非法: %w", m.ID, err)
			}
			if !caps.AllowedByTrust(c, trust) {
				return fmt.Errorf("assemble: 插件 %q 的 trust %q 不足以持有能力 %q(需要 %q)",
					m.ID, trust, c, caps.Requirement(c))
			}
		}
		if err := broker.Grant(m.ID, m.Caps...); err != nil {
			return fmt.Errorf("assemble: 授予插件 %q 失败: %w", m.ID, err)
		}
	}
	return nil
}
