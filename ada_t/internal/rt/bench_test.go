package rt_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/issueye/ada_t/backend/headless"
	"github.com/issueye/ada_t/internal/rt"
)

// 渲染性能基准:量「完整 Plan 求值 + JSON 跨边界」的每帧成本。
// 两种写法当前走同一条全量求值路径(a-key 只做告警,不参与实例身份);
// 保留 keyed/unkeyed 两种源码是为了将来做 keyed 记忆化时可直接对比。

func listComponent(keyed bool, n int) string {
	var items strings.Builder
	for i := 0; i < n; i++ {
		items.WriteString(fmt.Sprintf("{ id: %d, name: 'item-%d', desc: '描述文本 %d,用于撑起单元格内容长度' }", i, i, i))
		if i < n-1 {
			items.WriteString(", ")
		}
	}
	keyAttr := ""
	if keyed {
		keyAttr = ` a-key="it.id"`
	}
	return fmt.Sprintf(`
<template>
  <Column fill gap="4">
    <Text>{{ state.title }}</Text>
    <Row a-for="it in state.items"%s gap="2" padding="4">
      <Text bold>{{ it.id }}</Text>
      <Text>{{ it.name }}</Text>
      <Text color="gray">{{ it.desc }}</Text>
    </Row>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup() {
    const state = reactive({ title: '列表', items: [%s] })
    return { state }
  },
}
</script>
`, keyAttr, items.String())
}

func benchRender(b *testing.B, keyed bool, n int) {
	files := map[string]string{
		"L.aui":   listComponent(keyed, n),
		"util.ts": "export function fmt(n: number): string { return '#' + n }",
	}
	pkg := mustCompile(b, "L.aui", files)
	h := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: h.Apply})
	if err != nil {
		b.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	if _, err := r.MountWindow("main", "L", nil); err != nil {
		b.Fatalf("挂载: %v", err)
	}
	// 等首帧落地(挂载任务 + 渲染任务都执行过)
	for i := 0; i < 100; i++ {
		if _, err := r.RenderOnce("main"); err == nil {
			break
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.RenderOnce("main"); err != nil {
			b.Fatalf("渲染: %v", err)
		}
	}
}

// 100 条目(3 文本/条目)与 500 条目:量渲染 pass 的 JS+JSON 全链路成本。
func BenchmarkRenderList100(b *testing.B) { benchRender(b, true, 100) }
func BenchmarkRenderList500(b *testing.B) { benchRender(b, true, 500) }
