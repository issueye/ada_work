package sfc

import (
	"strings"
	"testing"
)

const sampleAUI = `<meta>
{ "name": "Hello" }
</meta>

<template>
  <Column gap="4">
    <Text>{{ msg }}</Text>
  </Column>
</template>

<script lang="ts">
import { reactive } from 'ada:surface'
export default { setup() { return {} } }
</script>

<style scoped>
.hi { font-size: 20; }
</style>

<i18n>
{ "zh-CN": { "hi": "你好" } }
</i18n>
`

func TestSplitOK(t *testing.T) {
	f, err := Split(sampleAUI)
	if err != nil {
		t.Fatalf("Split 失败: %v", err)
	}
	if f.Meta != `{ "name": "Hello" }` {
		t.Errorf("meta: %q", f.Meta)
	}
	if !strings.Contains(f.Template.Body, "<Column") {
		t.Errorf("template 内容缺失: %q", f.Template.Body)
	}
	if f.Script.Lang != "ts" {
		t.Errorf("script lang: %q", f.Script.Lang)
	}
	if f.Style.Lang != "scoped" {
		t.Errorf("style scoped 标记丢失: %q", f.Style.Lang)
	}
	if !strings.Contains(f.I18n, "你好") {
		t.Errorf("i18n 内容缺失")
	}
}

func TestSplitMissingTemplate(t *testing.T) {
	_, err := Split(`<script lang="ts">export default {}</script>`)
	if err == nil || !strings.Contains(err.Error(), "缺少必需的 <template>") {
		t.Fatalf("缺 template 应报错: %v", err)
	}
}

func TestSplitDuplicateBlock(t *testing.T) {
	src := `<template><Column /></template><template><Row /></template>`
	_, err := Split(src)
	if err == nil || !strings.Contains(err.Error(), "重复的 <template>") {
		t.Fatalf("重复块应报错: %v", err)
	}
}

func TestSplitUnknownBlock(t *testing.T) {
	src := `<template><Column /></template><foo>x</foo>`
	_, err := Split(src)
	if err == nil || !strings.Contains(err.Error(), "未知块") {
		t.Fatalf("未知块应报错: %v", err)
	}
}

func TestSplitUnclosed(t *testing.T) {
	src := `<template><Column /></template><script lang="ts">export default {}`
	_, err := Split(src)
	if err == nil || !strings.Contains(err.Error(), "没有闭合") {
		t.Fatalf("未闭合应报错: %v", err)
	}
}

func TestSplitTopLevelComment(t *testing.T) {
	src := "<!-- 说明 -->\n<template>\n  <Column />\n</template>\n"
	f, err := Split(src)
	if err != nil {
		t.Fatalf("顶层注释不应影响切块: %v", err)
	}
	if !strings.Contains(f.Template.Body, "<Column") {
		t.Fatalf("template 内容缺失: %q", f.Template.Body)
	}
	// 第一个非空白 content 在源文件第 3 行(<template> 在第 2 行)。
	if f.Template.Line != 3 {
		t.Fatalf("template body 首行 = %d, want 3", f.Template.Line)
	}
}

func TestSplitBlockCommentBetweenBlocks(t *testing.T) {
	src := `<template><Column /></template><!-- c --><meta>{ "name": "X" }</meta>`
	if _, err := Split(src); err != nil {
		t.Fatalf("块间注释不应影响切块: %v", err)
	}
}

func TestSplitScriptWithoutLang(t *testing.T) {
	src := `<template><Column /></template><script>export default {}</script>`
	f, err := Split(src)
	if err != nil {
		t.Fatal(err)
	}
	if f.Script.Lang != "" {
		t.Errorf("无 lang 属性应为空: %q", f.Script.Lang)
	}
}
