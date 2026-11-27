// Package lobe 把 lobe-mygo(Lobe UI for mygo,`github.com/lobehub/lobe-mygo`)的视觉令牌接入 ada_t。
//
// 同一套 lobe-mygo 主题同时喂两层:
//   - 原生渲染层:mygo *ui.Theme(ApplyNative/NativeTheme),mygoui / backend/lobe 后端据此绘制控件;
//   - 插件层:ada_t 扁平主题表(FlatTokens/ApplyFlat),写入 ctx.theme() 与模板 `theme.*`。
//
// 设计令牌(颜色/圆角/字号)沿用 lobe-mygo `theme.Tokens`(由 @lobehub/ui 生成),
// 与 ada_chat 手工映射的约定保持一致;本包把映射下沉成 ada_t 的一等能力。
package lobe

import (
	"fmt"

	"github.com/egoist/mygo/ui"
	lobetheme "github.com/lobehub/lobe-mygo/pkg/theme"
)

// Theme 是一套 lobe-mygo 视觉主题(明/暗两套令牌 + 跟随系统明暗)。
//
// 应用级创建一次并复用,不要每帧构造;零值不可用,用 Default/New/NewTokens。
type Theme struct {
	cfg *lobetheme.Config
}

// Default 返回 lobe-mygo 内置的 Lobe UI 令牌(明/暗自动跟随系统外观)。
func Default() *Theme { return &Theme{cfg: lobetheme.Default()} }

// New 包装一个已构造的 lobe-mygo 主题配置;nil 等价于 Default。
func New(cfg *lobetheme.Config) *Theme {
	if cfg == nil {
		return Default()
	}
	return &Theme{cfg: cfg}
}

// NewTokens 从明/暗两套 lobe-mygo 令牌构造主题(自定义品牌色等)。
func NewTokens(light, dark lobetheme.Tokens) *Theme {
	return &Theme{cfg: lobetheme.NewConfig(light, dark)}
}

// Config 返回底层 lobe-mygo 主题配置。
func (t *Theme) Config() *lobetheme.Config {
	if t == nil || t.cfg == nil {
		return lobetheme.Default()
	}
	return t.cfg
}

// ApplyNative 在 immediate-mode 帧起始应用 mygo 原生主题:
// 设置 c.SetTheme(...),明/暗跟随当前窗口外观(c.Theme().Dark)。
func (t *Theme) ApplyNative(c *ui.Context) { t.Config().Apply(c) }

// NativeTheme 返回 mygo *ui.Theme(不改动上下文;供自行 c.SetTheme 使用)。
func (t *Theme) NativeTheme(dark bool) *ui.Theme { return t.Config().Theme(dark) }

// Tokens 返回 lobe-mygo 原始令牌副本(明或暗)。
func (t *Theme) Tokens(dark bool) lobetheme.Tokens { return *t.Config().Tokens(dark) }

// FlatTokens 返回 ada_t 扁平主题表(明或暗),键与 ada_chat 约定一致:
//
//	dark、bg、panel、elevated、fg、muted、tertiary、accent、accentHover、border、radius。
//
// 表里颜色为 #rrggbb;radius 为数值。传给 adat.App.SetTheme / embed.SetTheme 后,
// 插件可在 ctx.theme() 与模板 `theme.<key>` 里读取。
func (t *Theme) FlatTokens(dark bool) map[string]any {
	tk := t.Config().Tokens(dark)
	return map[string]any{
		"dark":        tk.Dark,
		"bg":          hexOf(tk.BgLayout),
		"panel":       hexOf(tk.BgContainer),
		"elevated":    hexOf(tk.BgElevated),
		"fg":          hexOf(tk.Text),
		"muted":       hexOf(tk.TextSecondary),
		"tertiary":    hexOf(tk.TextTertiary),
		"accent":      hexOf(tk.Primary),
		"accentHover": hexOf(tk.PrimaryHover),
		"border":      hexOf(tk.Border),
		"radius":      float64(tk.BorderRadiusSM),
	}
}

// Sink 是 ada_t 主题令牌接收端(如 *adat.App;实现 SetTheme(map[string]any) error)。
type Sink interface {
	SetTheme(tokens map[string]any) error
}

// ApplyFlat 把扁平令牌(FlatTokens)下发给 sink。
func (t *Theme) ApplyFlat(s Sink, dark bool) error {
	if s == nil {
		return fmt.Errorf("lobe: nil Sink")
	}
	return s.SetTheme(t.FlatTokens(dark))
}

// hexOf 把 mygo 颜色转为 #rrggbb(忽略 alpha)。
func hexOf(c ui.Color) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }
