package ui

import (
	mui "github.com/egoist/mygo/ui"
	"github.com/issueye/ada_t/plugin"
	"github.com/lobehub/lobe-mygo/pkg/display"
)

// pluginSlot 把某个槽位的插件视图画进当前布局。
func (u *UI) pluginSlot(c *mui.Context, slot string) {
	if u.Plugins == nil {
		return
	}
	u.Plugins.Draw(c, slot)
}

func (u *UI) hasPluginSlot(slot string) bool {
	return u.Plugins != nil && u.Plugins.Has(slot)
}

// pluginSidebarSection 在侧栏会话列表后加一段「插件」内容。
func (u *UI) pluginSidebarSection(c *mui.Context) {
	if !u.hasPluginSlot(plugin.SlotSidebarItem) {
		return
	}
	mui.Row(c).Padding(10, 8, 2, 8).Gap(6).AlignItems(mui.Center).Children(func() {
		display.Text(c, "插件", display.TextProps{Type: display.TextSecondary, FontSize: 11, Strong: true})
	})
	u.pluginSlot(c, plugin.SlotSidebarItem)
}

// pluginSettingsPane 是设置页的「插件」页签:插件自己决定内容(只读宿主能力)。
func (u *UI) pluginSettingsPane(c *mui.Context) {
	mui.Column(c).Gap(10).Children(func() {
		display.Text(c, "插件通过 host.* 服务读取会话与设置;能力白名单由 plugin.json 决定。",
			display.TextProps{Type: display.TextSecondary, FontSize: 12})
		u.pluginSlot(c, plugin.SlotSettingsTab)
	})
}
