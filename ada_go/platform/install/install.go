// Package install 是 M5 的安装与签名(L2 基础设施,不是插件)。
//
// 它只做四件事:清单校验、Artifact 摘要、Ed25519 验签、原子落地到
// <dest>/<id>@<version>/。信任等级来自「签名密钥」而不是清单自述。
package install

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/platform/version"
	"github.com/issueye/ada_jsrt/compile"
)

// SignatureFile 是插件目录里的签名文件名。
const SignatureFile = "plugin.sig.json"

// IndexFile 是安装根目录下的索引文件名(记录已安装插件)。
const IndexFile = "index.json"

// IndexEntry 是索引里的一条已安装插件记录。
type IndexEntry struct {
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	Kind      string    `json:"kind"`
	Dir       string    `json:"dir"`
	Digest    string    `json:"digest,omitempty"`
	Trust     string    `json:"trust"`
	Signed    bool      `json:"signed"`
	Caps      []string  `json:"caps,omitempty"`
	Installed time.Time `json:"installed"`
}

type indexDoc struct {
	Entries []IndexEntry `json:"entries"`
}

// MessagePrefix 是签名原文的前缀(区分「插件签名」与其它用途的签名)。
const MessagePrefix = "ada-plugin-v2\n"

// 默认安装上限。
const (
	DefaultMaxFiles = 1000
	DefaultMaxBytes = int64(16 << 20)
)

// Key 是公钥及其代表的信任等级。
type Key struct {
	Public ed25519.PublicKey
	Trust  caps.Trust
}

// Signature 是 plugin.sig.json 的内容。
type Signature struct {
	KeyID     string `json:"keyId"`
	Algo      string `json:"algo"`
	Signature string `json:"signature"` // base64
}

// Options 控制校验与安装。
type Options struct {
	Keys             map[string]Key
	RequireSignature bool
	MaxFiles         int
	MaxBytes         int64
	// KeepPrevious 让 Upgrade 保留旧版本目录与索引条目,作为回滚点。
	KeepPrevious bool
	// RepoKeys 是仓库索引签名的 keyring;RequireRepoSignature 要求仓库必须带签名。
	RepoKeys             map[string]Key
	RequireRepoSignature bool
}

// Info 是校验/安装结果。
type Info struct {
	ID       string
	Version  string
	Kind     string
	Digest   string
	Trust    caps.Trust
	Signed   bool
	Dir      string
	Manifest *plugin.Manifest
}

// Message 构造签名原文:前缀 + 清单原文 + digest。
func Message(manifestJSON []byte, digest string) []byte {
	return []byte(MessagePrefix + string(manifestJSON) + "\n" + digest)
}

// Sign 用私钥签名(测试与发布工具使用)。
func Sign(priv ed25519.PrivateKey, manifestJSON []byte, digest string) []byte {
	return ed25519.Sign(priv, Message(manifestJSON, digest))
}

// VerifySignature 校验签名。
func VerifySignature(pub ed25519.PublicKey, manifestJSON []byte, digest string, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("install: 公钥长度非法")
	}
	if !ed25519.Verify(pub, Message(manifestJSON, digest), sig) {
		return fmt.Errorf("install: 签名校验失败")
	}
	return nil
}

// VerifyDir 校验一个插件目录:清单、Artifact 摘要与签名。
func VerifyDir(srcDir string, opts Options) (*Info, error) {
	manifests, err := plugin.LoadDir(srcDir)
	if err != nil {
		return nil, err
	}
	if len(manifests) != 1 {
		return nil, fmt.Errorf("install: %s 必须恰好包含一个插件,实际 %d 个", srcDir, len(manifests))
	}
	m := manifests[0]
	raw, err := os.ReadFile(filepath.Join(srcDir, "plugin.json"))
	if err != nil {
		return nil, fmt.Errorf("install: 读取清单失败: %w", err)
	}

	info := &Info{ID: m.ID, Version: m.Version, Kind: m.Kind, Dir: srcDir, Manifest: m}
	switch m.Kind {
	case "ts":
		sources, err := loader.CollectSources(srcDir)
		if err != nil {
			return nil, err
		}
		artifact, err := compile.Build(m.ID, m.Entry, sources, compile.Options{})
		if err != nil {
			return nil, fmt.Errorf("install: 编译 %s 失败: %w", m.ID, err)
		}
		info.Digest = artifact.Digest
	case "go":
		// Go 插件编译进宿主,没有可签名的 Artifact;信任为 builtin。
		info.Trust = caps.TrustBuiltin
	}

	sigPath := filepath.Join(srcDir, SignatureFile)
	sigData, sigErr := os.ReadFile(sigPath)
	if sigErr != nil {
		if !os.IsNotExist(sigErr) {
			return nil, fmt.Errorf("install: 读取签名失败: %w", sigErr)
		}
		if opts.RequireSignature && m.Kind == "ts" {
			return nil, fmt.Errorf("install: 插件 %s 未签名,但要求签名", m.ID)
		}
		if m.Kind == "ts" {
			info.Trust = caps.TrustThirdParty
		}
		return info, nil
	}
	if m.Kind == "go" {
		return nil, fmt.Errorf("install: Go 插件不接受 Artifact 签名(%s)", m.ID)
	}

	var sig Signature
	if err := json.Unmarshal(sigData, &sig); err != nil {
		return nil, fmt.Errorf("install: 签名文件损坏: %w", err)
	}
	if sig.Algo != "" && sig.Algo != "ed25519" {
		return nil, fmt.Errorf("install: 不支持的签名算法 %q", sig.Algo)
	}
	key, ok := opts.Keys[sig.KeyID]
	if !ok {
		return nil, fmt.Errorf("install: 未知签名密钥 %q", sig.KeyID)
	}
	rawSig, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return nil, fmt.Errorf("install: 签名不是合法 base64: %w", err)
	}
	if err := VerifySignature(key.Public, raw, info.Digest, rawSig); err != nil {
		return nil, fmt.Errorf("install: 插件 %s 验签失败: %w", m.ID, err)
	}
	info.Signed = true
	info.Trust = key.Trust
	if !caps.ValidTrust(info.Trust) {
		return nil, fmt.Errorf("install: 密钥 %q 的信任等级非法: %q", sig.KeyID, info.Trust)
	}
	return info, nil
}

// List 读取安装根目录的索引;索引不存在时返回空列表。
func List(destRoot string) ([]IndexEntry, error) {
	data, err := os.ReadFile(filepath.Join(destRoot, IndexFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("install: 读取索引失败: %w", err)
	}
	var doc indexDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("install: 索引损坏: %w", err)
	}
	return doc.Entries, nil
}

func writeIndex(destRoot string, entries []IndexEntry) error {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ID != entries[j].ID {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Version < entries[j].Version
	})
	data, err := json.MarshalIndent(indexDoc{Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("install: 序列化索引失败: %w", err)
	}
	tmp, err := os.CreateTemp(destRoot, ".index-*")
	if err != nil {
		return fmt.Errorf("install: 创建索引临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("install: 写索引失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("install: 关闭索引临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(destRoot, IndexFile)); err != nil {
		return fmt.Errorf("install: 原子写索引失败: %w", err)
	}
	return nil
}

// addIndexEntry 把已安装插件写入索引(同 id@version 替换)。
func addIndexEntry(destRoot string, info *Info) error {
	entries, err := List(destRoot)
	if err != nil {
		return err
	}
	entry := IndexEntry{
		ID: info.ID, Version: info.Version, Kind: info.Kind, Dir: info.Dir,
		Digest: info.Digest, Trust: string(info.Trust), Signed: info.Signed,
		Caps: info.Manifest.Caps, Installed: time.Now().UTC(),
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.ID == entry.ID && e.Version == entry.Version {
			continue
		}
		kept = append(kept, e)
	}
	kept = append(kept, entry)
	return writeIndex(destRoot, kept)
}

// Uninstall 删除某插件的全部已安装版本,并更新索引。
//
// 安全:只删除索引里登记、且位于 destRoot 之下的目录;索引被篡改指向外部时拒绝。
func uninstallUnlocked(destRoot, id string) (int, error) {
	if id == "" {
		return 0, fmt.Errorf("install: 插件 id 不能为空")
	}
	entries, err := List(destRoot)
	if err != nil {
		return 0, err
	}
	rootAbs, err := filepath.Abs(destRoot)
	if err != nil {
		return 0, fmt.Errorf("install: 解析安装根目录失败: %w", err)
	}
	var kept []IndexEntry
	removed := 0
	for _, e := range entries {
		if e.ID != id {
			kept = append(kept, e)
			continue
		}
		dirAbs, err := filepath.Abs(e.Dir)
		if err != nil {
			return removed, fmt.Errorf("install: 解析已安装目录失败: %w", err)
		}
		rel, err := filepath.Rel(rootAbs, dirAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return removed, fmt.Errorf("install: 索引里的目录不在安装根之下,拒绝删除: %s", e.Dir)
		}
		if err := os.RemoveAll(dirAbs); err != nil {
			return removed, fmt.Errorf("install: 删除 %s 失败: %w", e.Dir, err)
		}
		removed++
	}
	if removed == 0 {
		return 0, fmt.Errorf("install: 插件 %q 未安装", id)
	}
	if err := writeIndex(destRoot, kept); err != nil {
		return removed, err
	}
	return removed, nil
}

// Upgrade 安装更高版本并清理旧版本目录。
//
// 语义:先安装新版本(原子),再删除同 id 的旧版本目录并把索引收敛到最新一条。
// 拒绝降级与同版本重复;分开两步意味着「清理失败时新版本仍已安装」,需要宿主提示。
func upgradeUnlocked(src, destRoot string, opts Options) (*Info, error) {
	preview, err := VerifyDir(src, opts)
	if err != nil {
		return nil, err
	}
	entries, err := List(destRoot)
	if err != nil {
		return nil, err
	}
	var current []IndexEntry
	for _, e := range entries {
		if e.ID == preview.ID {
			current = append(current, e)
		}
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("install: 插件 %q 未安装,请使用 install", preview.ID)
	}
	newest := current[0]
	for _, e := range current[1:] {
		cmp, err := compareVersionStrings(e.Version, newest.Version)
		if err != nil {
			return nil, err
		}
		if cmp > 0 {
			newest = e
		}
	}
	cmp, err := compareVersionStrings(preview.Version, newest.Version)
	if err != nil {
		return nil, err
	}
	switch {
	case cmp == 0:
		return nil, fmt.Errorf("install: 版本 %s 已安装,无需升级", preview.Version)
	case cmp < 0:
		return nil, fmt.Errorf("install: 拒绝降级(%s → %s)", newest.Version, preview.Version)
	}

	info, err := installUnlocked(src, destRoot, opts)
	if err != nil {
		return nil, err
	}
	if opts.KeepPrevious {
		return info, nil
	}
	if err := cleanupOlderVersions(destRoot, info); err != nil {
		return info, fmt.Errorf("install: 新版本已安装,但清理旧版本失败: %w", err)
	}
	return info, nil
}

// Rollback 删除当前最新版本,索引回退到次新版本;没有旧版本时报错。
//
// 前提:升级时使用了 KeepPrevious(否则旧目录已被清理,无从回滚)。
func rollbackUnlocked(destRoot, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("install: 插件 id 不能为空")
	}
	entries, err := List(destRoot)
	if err != nil {
		return "", err
	}
	var versions []IndexEntry
	for _, e := range entries {
		if e.ID == id {
			versions = append(versions, e)
		}
	}
	if len(versions) < 2 {
		return "", fmt.Errorf("install: 插件 %q 没有可回滚的旧版本", id)
	}
	// 按版本升序;最新是最后一个,回滚目标为次新。
	sort.Slice(versions, func(i, j int) bool {
		cmp, err := compareVersionStrings(versions[i].Version, versions[j].Version)
		return err == nil && cmp < 0
	})
	target := versions[len(versions)-2]
	if _, err := os.Stat(target.Dir); err != nil {
		return "", fmt.Errorf("install: 回滚目标目录不存在: %s", target.Dir)
	}

	rootAbs, err := filepath.Abs(destRoot)
	if err != nil {
		return "", fmt.Errorf("install: 解析安装根目录失败: %w", err)
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.ID == id {
			cmp, err := compareVersionStrings(e.Version, target.Version)
			if err != nil {
				return "", err
			}
			if cmp > 0 {
				dirAbs, err := filepath.Abs(e.Dir)
				if err != nil {
					return "", fmt.Errorf("install: 解析版本目录失败: %w", err)
				}
				rel, err := filepath.Rel(rootAbs, dirAbs)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return "", fmt.Errorf("install: 索引里的目录不在安装根之下,拒绝删除: %s", e.Dir)
				}
				if err := os.RemoveAll(dirAbs); err != nil {
					return "", fmt.Errorf("install: 删除版本 %s 失败: %w", e.Dir, err)
				}
				continue
			}
		}
		kept = append(kept, e)
	}
	if err := writeIndex(destRoot, kept); err != nil {
		return "", err
	}
	return target.Version, nil
}

func compareVersionStrings(a, b string) (int, error) {
	va, err := version.Parse(a)
	if err != nil {
		return 0, fmt.Errorf("install: 版本号非法 %q: %w", a, err)
	}
	vb, err := version.Parse(b)
	if err != nil {
		return 0, fmt.Errorf("install: 版本号非法 %q: %w", b, err)
	}
	return version.Compare(va, vb), nil
}

// cleanupOlderVersions 删除同 id 的旧版本目录,并把索引收敛到只保留新版本。
func cleanupOlderVersions(destRoot string, cur *Info) error {
	entries, err := List(destRoot)
	if err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(destRoot)
	if err != nil {
		return fmt.Errorf("install: 解析安装根目录失败: %w", err)
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.ID != cur.ID || e.Version == cur.Version {
			kept = append(kept, e)
			continue
		}
		dirAbs, err := filepath.Abs(e.Dir)
		if err != nil {
			return fmt.Errorf("install: 解析旧版本目录失败: %w", err)
		}
		rel, err := filepath.Rel(rootAbs, dirAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("install: 索引里的目录不在安装根之下,拒绝删除: %s", e.Dir)
		}
		if err := os.RemoveAll(dirAbs); err != nil {
			return fmt.Errorf("install: 删除旧版本 %s 失败: %w", e.Dir, err)
		}
	}
	return writeIndex(destRoot, kept)
}

// Install 校验后原子安装到 <DestRoot>/<id>@<version>/。
func installUnlocked(srcDir, destRoot string, opts Options) (*Info, error) {
	info, err := VerifyDir(srcDir, opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return nil, fmt.Errorf("install: 创建安装目录失败: %w", err)
	}
	final := filepath.Join(destRoot, info.ID+"@"+CleanVersion(info.Version))
	if _, err := os.Stat(final); err == nil {
		return nil, fmt.Errorf("install: 目标已存在,拒绝覆盖: %s", final)
	}

	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	tmp, err := os.MkdirTemp(destRoot, ".install-*")
	if err != nil {
		return nil, fmt.Errorf("install: 创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmp) // rename 成功后这里删除的是不存在的旧路径
	if err := copyTree(srcDir, tmp, maxFiles, maxBytes); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, fmt.Errorf("install: 原子重命名失败: %w", err)
	}
	info.Dir = final
	if err := addIndexEntry(destRoot, info); err != nil {
		// 索引写失败 → 回滚已安装目录,不留下「装了但没人知道」的状态。
		_ = os.RemoveAll(final)
		return nil, fmt.Errorf("install: 写入索引失败(已回滚): %w", err)
	}
	return info, nil
}

// copyTree 复制目录:拒绝符号链接,限制文件数与总字节数。
func copyTree(src, dst string, maxFiles int, maxBytes int64) error {
	files := 0
	var total int64
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("install: 目录里不允许符号链接: %s", p)
		}
		if d.IsDir() {
			return nil
		}
		files++
		if files > maxFiles {
			return fmt.Errorf("install: 文件数超过上限(%d)", maxFiles)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > maxBytes {
			return fmt.Errorf("install: 总大小超过上限(%d 字节)", maxBytes)
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(p, target)
	})
	if err != nil {
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// TrustFromKey 便于发布工具推导信任等级。
func TrustFromKey(trust string) (caps.Trust, error) {
	t := caps.Trust(trust)
	if !caps.ValidTrust(t) {
		return "", fmt.Errorf("install: 非法信任等级 %q", trust)
	}
	return t, nil
}

// CleanVersion 去掉版本号里的路径分隔符,避免目录逃逸(清单已校验 semver,这里再做一次防御)。
func CleanVersion(v string) string {
	return strings.NewReplacer("/", "_", `\`, "_", ":", "_").Replace(v)
}
