// Package compile 是运行时编译层的入口:把 .aui 组件包编译为可执行产物。
//
// 管线(对齐 ui-framework.md §5.1,native 化):
//
//	.aui 文件集
//	  → 1. sfc 切块(meta/template/script/style/i18n)
//	  → 2. templatec:模板 → RenderPlan(JSON AST)
//	  → 3. scriptc:esbuild 把 <script> TS 降级为 ESM JS
//	  → 4. stylec:<style scoped> → class→控件属性令牌
//	  → 5. i18n/meta 解析
//	  → 6. 相对导入解析(./util.ts、./Tree.aui 的 script)
//	  → 7. 产物:Plan/模块 JS/manifest(sha256 摘要)
//
// 编译不执行组件代码;同输入同字节,可 golden。
package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/issueye/ada_t/internal/expr"
	"github.com/issueye/ada_t/internal/scriptc"
	"github.com/issueye/ada_t/internal/sfc"
	"github.com/issueye/ada_t/internal/stylec"
	"github.com/issueye/ada_t/internal/templatec"
)

// PlanVersion 与 widget 包的契约版本一致。
const PlanVersion = 1

// Component 是一个 .aui 的编译产物。
type Component struct {
	Name      string                       // 组件名(meta.name 或文件名)
	Plan      []byte                       // RenderPlan JSON
	Module    string                       // 入口脚本模块名(如 "Counter.aui.js")
	Script    []byte                       // 入口脚本(降级后的 JS);可为空
	Style     map[string]map[string]any    // class → 属性令牌
	I18n      map[string]map[string]string // locale → key → 文案
	HasScript bool
}

// Package 是一个组件目录的完整编译产物。
type Package struct {
	Dir        string
	Entry      string // 入口组件名
	Components []*Component
	// Modules 是全部脚本模块(含组件脚本与相对导入的 .ts/.js 文件),
	// 名字是目录内相对路径(组件脚本为 "<File>.js")。
	Modules map[string][]byte
	// Warnings 是非致命诊断(缺 a-key 等)。
	Warnings []string
}

// Options 是编译参数;Entry 为空时:唯一 .aui 即入口,否则报错。
type Options struct {
	Entry string
}

// —— 公共入口 ——

// CompileDir 编译一个目录:目录内所有 .aui 是组件,其余 .ts/.js 是可导入模块。
func CompileDir(dir string, opts Options) (*Package, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("compile: 读取目录失败: %w", err)
	}
	files := map[string]string{}
	var auis []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".aui" || ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".mjs" {
			b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				return nil, fmt.Errorf("compile: 读取 %s 失败: %w", e.Name(), rerr)
			}
			files[e.Name()] = string(b)
			if ext == ".aui" {
				auis = append(auis, e.Name())
			}
		}
	}
	if len(auis) == 0 {
		return nil, fmt.Errorf("compile: %s 里没有 .aui 组件文件", dir)
	}
	sort.Strings(auis)
	if len(auis) > 1 && opts.Entry == "" {
		return nil, fmt.Errorf("compile: %s 里有多个 .aui 组件 %v:请用 Options.Entry(CLI 的 -entry)指定入口", dir, auis)
	}
	return CompileFiles(auis[0], files, opts)
}

// CompileFiles 从内存文件集编译;entryFile 是入口 .aui 的相对路径。
func CompileFiles(entryFile string, files map[string]string, opts Options) (*Package, error) {
	if _, ok := files[entryFile]; !ok {
		return nil, fmt.Errorf("compile: 入口文件 %q 不存在", entryFile)
	}
	if !strings.HasSuffix(entryFile, ".aui") {
		return nil, fmt.Errorf("compile: 入口 %q 不是 .aui 文件", entryFile)
	}
	p := &Package{Dir: ".", Modules: map[string][]byte{}}

	// 1. 编译每个 .aui 组件
	var names []string
	compByFile := map[string]*Component{}
	byName := map[string]string{}
	for file, text := range files {
		if !strings.HasSuffix(file, ".aui") {
			continue
		}
		comp, warns, err := compileComponent(file, text, p)
		if err != nil {
			return nil, err
		}
		if prev, ok := byName[comp.Name]; ok {
			return nil, fmt.Errorf("compile: 组件名 %q 重复(%s 与 %s):<meta> 的 name 必须唯一", comp.Name, prev, file)
		}
		byName[comp.Name] = file
		p.Components = append(p.Components, comp)
		p.Warnings = append(p.Warnings, qualifyWarns(file, warns)...)
		compByFile[file] = comp
		names = append(names, comp.Name)
	}
	// 组件顺序影响 manifest/digest:固定按名字排序,保证同输入同字节。
	sort.Slice(p.Components, func(i, j int) bool { return p.Components[i].Name < p.Components[j].Name })
	sort.Strings(names)
	// 入口组件:显式指定 > entryFile 对应的组件
	if opts.Entry != "" {
		found := false
		for _, c := range p.Components {
			if c.Name == opts.Entry {
				p.Entry = opts.Entry
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("compile: 入口组件 %q 不存在(有 %v)", opts.Entry, names)
		}
	} else if entryComp := compByFile[entryFile]; entryComp != nil {
		p.Entry = entryComp.Name
	} else {
		return nil, fmt.Errorf("compile: 无法确定入口组件(有 %v):设置 Options.Entry", names)
	}

	// 2. 编译其余脚本文件(可被相对导入)
	for file, text := range files {
		if strings.HasSuffix(file, ".aui") || !scriptFile(file) {
			continue
		}
		res, err := scriptc.Transform(file, []byte(text))
		if err != nil {
			return nil, err
		}
		p.Modules[file] = res.Code
	}
	return p, nil
}

func scriptFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".ts", ".tsx", ".js", ".mjs":
		return true
	}
	return false
}

func qualifyWarns(file string, warns []templatec.Diagnostic) []string {
	out := make([]string, 0, len(warns))
	for _, w := range warns {
		out = append(out, fmt.Sprintf("%s:%d: [%s] %s", file, w.Line, w.Code, w.Message))
	}
	return out
}

// scriptDiagnostic 把 esbuild 的逐条诊断从「<script> 块体坐标」换算到源文件坐标,
// 避免 CLI 报虚拟模块名与块内行号。
func scriptDiagnostic(file string, bodyLine int, err error) error {
	var se *scriptc.Error
	if !errors.As(err, &se) {
		return fmt.Errorf("compile: %s: %w", file, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "compile: %s: ESBUILD: TS/JS 降级错误", file)
	for _, m := range se.Messages {
		if m.Location != nil {
			fmt.Fprintf(&b, "\n  %s:%d:%d: %s", file, bodyLine+m.Location.Line-1, m.Location.Column, m.Text)
		} else {
			b.WriteString("\n  " + m.Text)
		}
	}
	return errors.New(b.String())
}

// styleDiagnostic 把样式错误从「<style> 块体坐标」换算到源文件坐标。
func styleDiagnostic(file string, bodyLine int, err error) error {
	var se *stylec.Error
	if !errors.As(err, &se) {
		return fmt.Errorf("compile: %s: %w", file, err)
	}
	line := se.Line
	if line > 0 {
		line += bodyLine - 1
	}
	return fmt.Errorf("compile: %s:%d: EUISTYLE: %s", file, line, se.Message)
}

// compileComponent 编译单个 .aui。
func compileComponent(file, src string, p *Package) (*Component, []templatec.Diagnostic, error) {
	sf, err := sfc.Split(src)
	if err != nil {
		return nil, nil, fmt.Errorf("compile: %s: %w", file, err)
	}
	// meta → 组件名
	name := strings.TrimSuffix(filepath.Base(file), ".aui")
	if sf.Meta != "" {
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(sf.Meta), &meta); err != nil {
			return nil, nil, fmt.Errorf("compile: %s: <meta> 不是合法 JSON: %w", file, err)
		}
		if meta.Name != "" {
			name = meta.Name
		}
	}
	if !validName(name) {
		return nil, nil, fmt.Errorf("compile: %s: 组件名 %q 非法(需要标识符)", file, name)
	}

	// 模板 → Plan
	plan, warns, err := templatec.Compile(sf.Template.Body, templatec.Options{Component: name})
	if err != nil {
		if e, ok := err.(*templatec.Error); ok {
			// templatec 的行号相对于 <template> 的 body;补上块首行偏移,
			// 让 CLI 指向源文件真实行。
			ee := *e
			if ee.Line > 0 {
				ee.Line += sf.Template.Line - 1
			} else {
				ee.Line = sf.Template.Line
			}
			if ee.Col < 1 {
				ee.Col = 1
			}
			return nil, nil, fmt.Errorf("compile: %s: %s", file, ee.Error())
		}
		return nil, nil, fmt.Errorf("compile: %s: %w", file, err)
	}
	// 告警行号与模板体一致(相对 <template> 块首),回填块偏移,让 CLI 指向源文件。
	for i := range warns {
		if warns[i].Line > 0 {
			warns[i].Line += sf.Template.Line - 1
		}
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, nil, fmt.Errorf("compile: %s: Plan 序列化失败: %w", file, err)
	}

	comp := &Component{Name: name, Plan: planJSON}

	// script → ESM JS(虚拟模块名 = "<file>.js",供相对导入与注册;
	// loader 按 lang 决定:默认 ts,不按 ".aui.js" 的扩展名误判)
	if sf.Has("script") && sf.Script.Body != "" {
		lang := strings.ToLower(sf.Script.Lang)
		loader := api.LoaderTS
		switch lang {
		case "", "ts":
			loader = api.LoaderTS
		case "js":
			loader = api.LoaderJS
		default:
			return nil, nil, fmt.Errorf("compile: %s: script lang=%q 不支持(只允许 ts/js)", file, sf.Script.Lang)
		}
		modName := file + ".js"
		res, err := scriptc.TransformWith(modName, []byte(sf.Script.Body), loader)
		if err != nil {
			return nil, nil, scriptDiagnostic(file, sf.Script.Line, err)
		}
		comp.Module = modName
		comp.Script = res.Code
		comp.HasScript = true
		p.Modules[modName] = res.Code
		for _, w := range res.Warnings {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: %s", modName, w))
		}
	}

	// style → 令牌
	if sf.Has("style") && sf.Style.Body != "" {
		tokens, err := stylec.Compile(sf.Style.Body)
		if err != nil {
			return nil, nil, styleDiagnostic(file, sf.Style.Line, err)
		}
		comp.Style = tokens
	}

	// i18n
	if sf.I18n != "" {
		var i18n map[string]map[string]string
		if err := json.Unmarshal([]byte(sf.I18n), &i18n); err != nil {
			return nil, nil, fmt.Errorf("compile: %s: <i18n> 不是合法 JSON: %w", file, err)
		}
		comp.I18n = i18n
	}

	// 无脚本组件:生成占位模块(export default { name }),
	// 让 `import Sub from './Sub'`(components 注册)对纯声明式组件同样可用
	if !comp.HasScript {
		modName := file + ".js"
		nameJSON, _ := json.Marshal(name)
		code := fmt.Sprintf("export default { name: %s }\n", nameJSON)
		comp.Module = modName
		comp.Script = []byte(code)
		p.Modules[modName] = []byte(code)
	}
	return comp, warns, nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && (r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			continue
		}
		if r == '_' || r == '$' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		return false
	}
	return true
}

// —— 相对导入解析(moejs.Link 的 resolve 回调) ——

// Resolve 把 referrer 模块里的 import spec 解析成 Modules 的键。
// 只允许相对导入与 'ada:surface'(后者由调用方处理);裸包名与 Node 内置拒绝。
// 返回 ok=false 表示解析失败。
func (p *Package) Resolve(referrer, spec string) (string, bool) {
	if spec == "ada:surface" {
		return "", true // 外部:由 rt 注入模块处理
	}
	if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") && !strings.HasPrefix(spec, "/") {
		return "", false // 裸包名拒绝(EUIIMPORT 语义,信息在错误里给)
	}
	base := referrer
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[:i]
	} else {
		base = ""
	}
	target := spec
	switch {
	case strings.HasPrefix(target, "/"):
		target = strings.TrimPrefix(target, "/")
	case strings.HasPrefix(target, "./"):
		target = joinPath(base, target[2:])
	case strings.HasPrefix(target, "../"):
		// 简单归一化(禁越界)
		up := 0
		t := target
		for strings.HasPrefix(t, "../") {
			up++
			t = t[3:]
		}
		segs := splitPath(base)
		if len(segs) < up {
			return "", false // 越界
		}
		target = joinPath(strings.Join(segs[:len(segs)-up], "/"), t)
	}
	target = strings.TrimPrefix(target, "/")
	// 候选:原样、加扩展名、.aui 的脚本模块
	cands := []string{target}
	if scriptFile(target) {
		// 已带扩展名
	} else {
		for _, ext := range []string{".ts", ".tsx", ".js", ".mjs"} {
			cands = append(cands, target+ext)
		}
	}
	// ./Tree → Tree.aui 的脚本
	if _, ok := p.Modules[target+".aui.js"]; ok {
		cands = append([]string{target + ".aui.js"}, cands...)
	}
	for _, c := range cands {
		if _, ok := p.Modules[c]; ok {
			return c, true
		}
	}
	return "", false
}

func splitPath(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}

// Digest 返回组件包内容的规范摘要(sha256:hex),用于插件完整性校验:
// 覆盖 entry、每个组件的 Plan/Style/I18n/脚本模块,以及其余可导入模块。
// 同输入同字节,与落盘产物无关(不依赖文件名大小写或临时目录)。
func (p *Package) Digest() string {
	type comp struct {
		Name   string
		Module string
		Plan   []byte
		Style  map[string]map[string]any
		I18n   map[string]map[string]string
	}
	h := sha256.New()
	fmt.Fprintf(h, "entry:%s\n", p.Entry)
	comps := make([]comp, 0, len(p.Components))
	for _, c := range p.Components {
		comps = append(comps, comp{Name: c.Name, Module: c.Module, Plan: c.Plan, Style: c.Style, I18n: c.I18n})
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	for _, c := range comps {
		sum := sha256.Sum256(c.Plan)
		fmt.Fprintf(h, "c:%s|%s|%x|%s|%s\n", c.Name, c.Module, sum,
			canonJSON(c.Style), canonJSON(c.I18n))
	}
	// 组件脚本已在组件行覆盖;这里只算其余相对导入模块。
	compMods := map[string]bool{}
	for _, c := range comps {
		compMods[c.Module] = true
	}
	names := make([]string, 0, len(p.Modules))
	for n := range p.Modules {
		if !compMods[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		sum := sha256.Sum256(p.Modules[n])
		fmt.Fprintf(h, "m:%s|%x\n", n, sum)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func canonJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// DirSignature 返回组件目录的变更指纹(路径 + mtime + 大小),供
// app.WatchDir / adat watch 这类轮询热重载使用。扩展名集合与 CompileDir
// 的可编译输入保持一致(.aui/.ts/.tsx/.js/.mjs)。
func DirSignature(dir string) string {
	type fv struct {
		Name string    `json:"n"`
		Mod  time.Time `json:"m"`
		Size int64     `json:"s"`
	}
	var files []fv
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".aui", ".ts", ".tsx", ".js", ".mjs":
			files = append(files, fv{path, info.ModTime(), info.Size()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	b, _ := json.Marshal(files)
	return string(b)
}

// —— 产物落盘与校验(§5.6 简化版) ——

// FileEntry 是 manifest 的文件表项。
type FileEntry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// CompEntry 是 manifest 的组件表项。
type CompEntry struct {
	Name   string `json:"name"`
	Plan   string `json:"plan"`
	Module string `json:"module"`
}

// Manifest 是组件包清单(ui.manifest.json)。
type Manifest struct {
	PlanVersion int         `json:"planVersion"`
	Entry       string      `json:"entry"`
	Components  []CompEntry `json:"components"`
	Files       []FileEntry `json:"files"`
	Digest      string      `json:"digest"`
}

// rawManifest 是计算 digest 的规范化视图(不含 digest 字段)。
type rawManifest struct {
	PlanVersion int         `json:"planVersion"`
	Entry       string      `json:"entry"`
	Components  []CompEntry `json:"components"`
	Files       []FileEntry `json:"files"`
}

// digest 计算 manifest 的规范化 sha256。Build 与 Verify 共用同一实现,
// 避免「写和校验用的是两套规则」。
func (m *Manifest) digest() (string, error) {
	raw, err := json.Marshal(rawManifest{
		PlanVersion: m.PlanVersion,
		Entry:       m.Entry,
		Components:  m.Components,
		Files:       m.Files,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Build 把产物写到 outDir:<Name>.plan.json、<Name>.module.js、ui.manifest.json。
// digest = sha256(规范化 manifest,不含 digest 字段)。
func (p *Package) Build(outDir string) (*Manifest, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("compile: 创建输出目录失败: %w", err)
	}
	files := map[string][]byte{}
	var comps []CompEntry
	for _, c := range p.Components {
		planName := c.Name + ".plan.json"
		files[planName] = c.Plan
		modName := c.Name + ".module.js"
		files[modName] = c.Script
		comps = append(comps, CompEntry{Name: c.Name, Plan: planName, Module: modName})
	}
	// 其余模块(相对导入的脚本)
	for name, code := range p.Modules {
		base := name
		if strings.HasSuffix(base, ".aui.js") {
			continue // 组件脚本已写入 <Name>.module.js
		}
		files["dep_"+strings.ReplaceAll(base, "/", "_")] = code
	}
	// 附加模块表进 manifest:文件表 = 全部产物
	m := &Manifest{PlanVersion: PlanVersion, Entry: p.Entry, Components: comps}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b := files[n]
		sum := sha256.Sum256(b)
		m.Files = append(m.Files, FileEntry{Name: n, SHA256: hex.EncodeToString(sum[:]), Bytes: len(b)})
	}
	// digest:先算无 digest 的规范 JSON
	digest, err := m.digest()
	if err != nil {
		return nil, err
	}
	m.Digest = digest
	// 落盘
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(outDir, n), files[n], 0o644); err != nil {
			return nil, fmt.Errorf("compile: 写 %s 失败: %w", n, err)
		}
	}
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "ui.manifest.json"), mj, 0o644); err != nil {
		return nil, fmt.Errorf("compile: 写 ui.manifest.json 失败: %w", err)
	}
	return m, nil
}

// Verify 校验 outDir 里的产物与 manifest 一致(发布端/安装端共用)。
func Verify(outDir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(outDir, "ui.manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("compile: 读取 manifest 失败: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("compile: manifest 不是合法 JSON: %w", err)
	}
	if m.PlanVersion != PlanVersion {
		return nil, fmt.Errorf("compile: planVersion %d 不支持(当前 %d)", m.PlanVersion, PlanVersion)
	}
	// digest 覆盖 planVersion/entry/components/files 四项:清单被改写即失败。
	want, err := m.digest()
	if err != nil {
		return nil, err
	}
	if m.Digest != want {
		return nil, fmt.Errorf("compile: manifest 摘要不符(EUIARTIFACT)")
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if !safeArtifactName(f.Name) {
			return nil, fmt.Errorf("compile: manifest 含非法文件名 %q(EUIARTIFACT)", f.Name)
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("compile: manifest 文件名重复 %q(EUIARTIFACT)", f.Name)
		}
		seen[f.Name] = true
		fb, err := os.ReadFile(filepath.Join(outDir, f.Name))
		if err != nil {
			return nil, fmt.Errorf("compile: 产物 %s 缺失: %w", f.Name, err)
		}
		sum := sha256.Sum256(fb)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, fmt.Errorf("compile: 产物 %s 摘要不符(EUIARTIFACT)", f.Name)
		}
		if len(fb) != f.Bytes {
			return nil, fmt.Errorf("compile: 产物 %s 字节数不符(EUIARTIFACT)", f.Name)
		}
	}
	return &m, nil
}

// safeArtifactName 只接受产物目录内的单层文件名,拒绝绝对路径、驱动盘/冒号、
// 目录分隔与 .. 穿越(manifest 可能来自外部,不能借 Verify 读目录外文件)。
func safeArtifactName(name string) bool {
	if name == "" || filepath.IsAbs(name) || strings.ContainsAny(name, `/\:`) {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	return filepath.Clean(name) == name
}

// —— 诊断码导出(供 CLI 展示) ——

// 语义上属于 import 类错误(EUIIMPORT):Resolve 失败时由调用方包装。
const CodeImport = "EUIIMPORT"

// ExprError 提供表达式诊断类型断言(供 CLI 归类)。
func ExprError(err error) (*expr.Error, bool) {
	e, ok := err.(*expr.Error)
	return e, ok
}
