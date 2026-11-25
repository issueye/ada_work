package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Artifact 是编译产物:入口 + 每个文件的 JS(+ 可选 sourcemap)。
//
// 它是纯数据:可以落盘、可以做摘要、可以跨进程传输,
// 但**不包含字节码**——moejs 的字节码不可序列化(硬约束 C2)。
type Artifact struct {
	ID        string            `json:"id"`
	Entry     string            `json:"entry"`
	Files     map[string]string `json:"files"`
	SourceMap map[string]string `json:"sourcemap,omitempty"`
	Digest    string            `json:"digest"`
}

// Build 把源文件逐个转换为 Artifact。src 的 key 即模块路径(用 `/` 分隔)。
//
// 为什么按文件而不是打包:模块解析要留给宿主(隔离槽、虚拟模块、能力检查都在那),
// 打包会把解析权交给编译器,后面就再也拿不回来了。
func Build(id, entry string, src map[string]string, opts Options) (*Artifact, error) {
	if len(src) == 0 {
		return nil, fmt.Errorf("compile: 源文件集合为空")
	}
	entry = cleanPath(entry)
	if _, ok := src[entry]; !ok {
		return nil, fmt.Errorf("compile: 入口 %q 不在源文件集合里", entry)
	}
	files := make(map[string]string, len(src))
	maps := make(map[string]string, len(src))
	for _, name := range sortedKeys(src) {
		res, err := Transform(name, []byte(src[name]), opts)
		if err != nil {
			return nil, err
		}
		files[name] = string(res.Code)
		if len(res.Map) > 0 {
			maps[name] = string(res.Map)
		}
	}
	a := &Artifact{ID: id, Entry: entry, Files: files}
	if len(maps) > 0 {
		a.SourceMap = maps
	}
	a.Digest = digestOf(files)
	return a, nil
}

// JSON 序列化 Artifact,用于落盘或跨进程传输。
func (a *Artifact) JSON() ([]byte, error) { return json.MarshalIndent(a, "", "  ") }

// ParseArtifact 反序列化并做完整性校验(入口存在、摘要匹配)。
func ParseArtifact(b []byte) (*Artifact, error) {
	var a Artifact
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("compile: Artifact 解析失败: %w", err)
	}
	if err := a.Verify(); err != nil {
		return nil, err
	}
	return &a, nil
}

// Verify 校验 Artifact 的结构与摘要。
//
// 从磁盘/网络拿到的产物必须先过这一步:摘要不匹配说明文件被改动或损坏,
// 继续加载等于执行来路不明的代码。
func (a *Artifact) Verify() error {
	if a.Entry == "" || len(a.Files) == 0 {
		return fmt.Errorf("compile: Artifact 缺少 entry 或 files")
	}
	if _, ok := a.Files[a.Entry]; !ok {
		return fmt.Errorf("compile: Artifact 的 entry %q 不在 files 里", a.Entry)
	}
	if a.Digest == "" {
		return fmt.Errorf("compile: Artifact 缺少 digest")
	}
	if a.Digest != digestOf(a.Files) {
		return fmt.Errorf("compile: Artifact 摘要不匹配(文件被改动或损坏)")
	}
	return nil
}

// Resolve 解析一个模块说明符。
//
// 只支持相对导入:`./x`、`../x`,并允许省略扩展名与 `/index`。
// 裸说明符(如 `lodash`)一律拒绝——没有 node_modules,静默失败比报错更糟。
func (a *Artifact) Resolve(referrer, spec string) (string, bool) {
	if spec == "" {
		return "", false
	}
	if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
		return "", false
	}
	base := path.Dir(referrer)
	if base == "." {
		base = ""
	}
	candidate := cleanPath(path.Join(base, spec))
	for _, name := range candidateVariants(candidate) {
		if _, ok := a.Files[name]; ok {
			return name, true
		}
	}
	return "", false
}

func candidateVariants(p string) []string {
	out := []string{p}
	// TS 生态常用 `./util.js` 指向 `util.ts`(编译前源码与产物扩展名不同),
	// 所以带 JS 扩展名的说明符要先去掉扩展名再试 TS 变体。
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".mjs", ".jsx":
		base := strings.TrimSuffix(p, path.Ext(p))
		for _, ext := range []string{".ts", ".tsx", ".js", ".mjs", ".jsx"} {
			out = append(out, base+ext)
		}
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs"} {
		out = append(out, p+ext)
	}
	for _, ext := range []string{".ts", ".tsx", ".js"} {
		out = append(out, path.Join(p, "index"+ext))
	}
	return out
}

func cleanPath(p string) string {
	p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	return strings.TrimPrefix(p, "./")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// digestOf 对「路径 + 内容」按路径排序后做 sha256:
// 同一份产物无论 map 迭代顺序如何,摘要都相同。
func digestOf(files map[string]string) string {
	h := sha256.New()
	for _, name := range sortedKeys(files) {
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write([]byte(files[name]))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
