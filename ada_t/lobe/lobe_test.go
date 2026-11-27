package lobe

import (
	"testing"

	lobetheme "github.com/lobehub/lobe-mygo/pkg/theme"
)

func TestDefaultTokensMatchLobeMyGo(t *testing.T) {
	th := Default()
	light := th.FlatTokens(false)
	dark := th.FlatTokens(true)

	if light["dark"] != false || dark["dark"] != true {
		t.Fatalf("dark 标记错误: light=%v dark=%v", light["dark"], dark["dark"])
	}
	// 明/暗背景应不同。
	if light["bg"] == dark["bg"] {
		t.Fatalf("明/暗 bg 应不同: %v", light["bg"])
	}
	// 关键键齐全。
	for _, k := range []string{"dark", "bg", "panel", "elevated", "fg", "muted", "tertiary", "accent", "accentHover", "border", "radius"} {
		if _, ok := light[k]; !ok {
			t.Fatalf("缺少键 %q", k)
		}
	}
	// 与 lobe-mygo 原始令牌一致。
	if got, want := light["accent"], hexOf(lobetheme.Light().Primary); got != want {
		t.Fatalf("accent 映射错误: got %v want %v", got, want)
	}
	if got, want := dark["bg"], hexOf(lobetheme.Dark().BgLayout); got != want {
		t.Fatalf("dark bg 映射错误: got %v want %v", got, want)
	}
	// radius 与 lobe-mygo BorderRadiusSM 一致。
	if got, want := light["radius"], float64(lobetheme.Light().BorderRadiusSM); got != want {
		t.Fatalf("radius 映射错误: got %v want %v", got, want)
	}
}

func TestNativeThemeDarkFlag(t *testing.T) {
	th := Default()
	if th.NativeTheme(false).Dark {
		t.Fatalf("NativeTheme(false) 应为亮色")
	}
	if !th.NativeTheme(true).Dark {
		t.Fatalf("NativeTheme(true) 应为暗色")
	}
}

func TestNilSafe(t *testing.T) {
	var th *Theme
	if th.Config() == nil {
		t.Fatalf("nil Theme 应回退 Default")
	}
	if th.FlatTokens(false)["dark"] != false {
		t.Fatalf("nil Theme FlatTokens 应可用")
	}
}

type sink struct{ got map[string]any }

func (s *sink) SetTheme(tokens map[string]any) error { s.got = tokens; return nil }

func TestApplyFlat(t *testing.T) {
	s := &sink{}
	if err := Default().ApplyFlat(s, true); err != nil {
		t.Fatalf("ApplyFlat: %v", err)
	}
	if s.got["dark"] != true {
		t.Fatalf("ApplyFlat 未下发 dark=true: %v", s.got)
	}
	if err := Default().ApplyFlat(nil, false); err == nil {
		t.Fatalf("nil sink 应报错")
	}
}

func TestNewTokens(t *testing.T) {
	light := *lobetheme.Light()
	dark := *lobetheme.Dark()
	light.Primary = light.Primary.Alpha(1) // 保持合法
	th := NewTokens(light, dark)
	if th.FlatTokens(false)["accent"] != hexOf(light.Primary) {
		t.Fatalf("自定义 light.Primary 未生效")
	}
}

func TestNewNilIsDefault(t *testing.T) {
	if New(nil).Config() == nil {
		t.Fatalf("New(nil) 应回退 Default")
	}
}
