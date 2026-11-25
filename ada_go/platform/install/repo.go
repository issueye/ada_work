// 离线仓库更新:repo.json 列出可用版本,按 semver 选择最新并复用 Upgrade。
package install

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/issueye/ada_go_v2/platform/version"
)

// RepoFile 是仓库索引文件名。
const RepoFile = "repo.json"

// RepoSignatureFile 是仓库索引的签名文件名。
const RepoSignatureFile = "repo.sig.json"

const repoMessagePrefix = "ada-repo-v2\n"

// RepoMessage 是仓库索引签名覆盖的消息。
func RepoMessage(repoJSON []byte) []byte {
	return append([]byte(repoMessagePrefix), repoJSON...)
}

// SignRepo 对仓库索引签名。
func SignRepo(priv ed25519.PrivateKey, repoJSON []byte) []byte {
	return ed25519.Sign(priv, RepoMessage(repoJSON))
}

// RepoEntry 是仓库里的一个插件版本。
type RepoEntry struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Dir     string `json:"dir"` // 相对仓库根目录
	Digest  string `json:"digest,omitempty"`
	// Source 是可选 artifact zip 的 URL(在线更新用)。
	Source string `json:"source,omitempty"`
}

type repoDoc struct {
	Entries []RepoEntry `json:"entries"`
}

func readRepoIndex(repoRoot string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, RepoFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("install: 读取仓库索引失败: %w", err)
	}
	return data, nil
}

func parseRepoIndex(data []byte) ([]RepoEntry, error) {
	var doc repoDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("install: 仓库索引损坏: %w", err)
	}
	return doc.Entries, nil
}

// LoadRepo 读取仓库索引(不存在时返回空)。
func LoadRepo(repoRoot string) ([]RepoEntry, error) {
	data, err := readRepoIndex(repoRoot)
	if err != nil || data == nil {
		return nil, err
	}
	return parseRepoIndex(data)
}

// VerifyRepoSignature 校验仓库索引签名。
func VerifyRepoSignature(pub ed25519.PublicKey, repoJSON, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("install: 仓库公钥长度非法")
	}
	if !ed25519.Verify(pub, RepoMessage(repoJSON), sig) {
		return fmt.Errorf("install: 仓库签名校验失败")
	}
	return nil
}

// LoadRepoVerified 读取仓库索引并按需验签。
//
// - 签名存在但 key 不在 keyring → 拒绝(不能悄悄接受未知签名);
// - 签名缺失且 requireRepo → 拒绝;否则允许(离线自用仓库);
// - 验签失败 → 拒绝。
func LoadRepoVerified(repoRoot string, keys map[string]Key, requireRepo bool) ([]RepoEntry, error) {
	data, err := readRepoIndex(repoRoot)
	if err != nil || data == nil {
		return nil, err
	}
	sigData, sigErr := os.ReadFile(filepath.Join(repoRoot, RepoSignatureFile))
	if sigErr != nil {
		if !os.IsNotExist(sigErr) {
			return nil, fmt.Errorf("install: 读取仓库签名失败: %w", sigErr)
		}
		if requireRepo {
			return nil, fmt.Errorf("install: 仓库缺少签名文件 %s", RepoSignatureFile)
		}
		return parseRepoIndex(data)
	}
	var sig Signature
	if err := json.Unmarshal(sigData, &sig); err != nil {
		return nil, fmt.Errorf("install: 仓库签名损坏: %w", err)
	}
	key, ok := keys[sig.KeyID]
	if !ok {
		return nil, fmt.Errorf("install: 仓库签名的 keyId %q 不在 keyring", sig.KeyID)
	}
	rawSig, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return nil, fmt.Errorf("install: 仓库签名不是合法 base64: %w", err)
	}
	if err := VerifyRepoSignature(key.Public, data, rawSig); err != nil {
		return nil, err
	}
	return parseRepoIndex(data)
}

// LatestRepoEntry 返回某插件在仓库里的最高版本。
func LatestRepoEntry(entries []RepoEntry, id string) (RepoEntry, bool, error) {
	var candidates []RepoEntry
	for _, e := range entries {
		if e.ID == id {
			if _, err := version.Parse(e.Version); err != nil {
				return RepoEntry{}, false, fmt.Errorf("install: 仓库版本号非法 %q: %w", e.Version, err)
			}
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return RepoEntry{}, false, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, _ := version.Parse(candidates[i].Version)
		b, _ := version.Parse(candidates[j].Version)
		return version.Compare(a, b) < 0
	})
	return candidates[len(candidates)-1], true, nil
}

// resolveRepoDir 把仓库条目目录限制在仓库根之下。
func resolveRepoDir(repoRoot, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("install: 仓库条目缺少 dir")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("install: 仓库 dir 只允许相对路径: %s", rel)
	}
	rootAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("install: 解析仓库根失败: %w", err)
	}
	abs, err := filepath.Abs(filepath.Join(rootAbs, filepath.Clean(rel)))
	if err != nil {
		return "", fmt.Errorf("install: 解析仓库目录失败: %w", err)
	}
	inside, err := filepath.Rel(rootAbs, abs)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("install: 仓库 dir 不能逃逸仓库根: %s", rel)
	}
	return abs, nil
}

// PreviewRepoUpdate 只校验仓库里该插件的最新版本,不落盘(供安装同意预览)。
func PreviewRepoUpdate(repoRoot, id string, opts Options) (*Info, error) {
	entries, err := LoadRepoVerified(repoRoot, opts.RepoKeys, opts.RequireRepoSignature)
	if err != nil {
		return nil, err
	}
	latest, ok, err := LatestRepoEntry(entries, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("install: 仓库里没有插件 %q", id)
	}
	dir, err := resolveRepoDir(repoRoot, latest.Dir)
	if err != nil {
		return nil, err
	}
	info, err := VerifyDir(dir, opts)
	if err != nil {
		return nil, err
	}
	if info.ID != id {
		return nil, fmt.Errorf("install: 仓库条目 id %q 与清单 id %q 不一致", id, info.ID)
	}
	if latest.Digest != "" && info.Digest != latest.Digest {
		return nil, fmt.Errorf("install: 摘要不匹配(期望 %s,实际 %s)", latest.Digest, info.Digest)
	}
	return info, nil
}

// UpdateFromRepo 从离线仓库安装最新版本;已安装版本不旧于仓库最新时拒绝。
func UpdateFromRepo(repoRoot, id, destRoot string, opts Options) (*Info, error) {
	if id == "" {
		return nil, fmt.Errorf("install: 插件 id 不能为空")
	}
	entries, err := LoadRepoVerified(repoRoot, opts.RepoKeys, opts.RequireRepoSignature)
	if err != nil {
		return nil, err
	}
	latest, ok, err := LatestRepoEntry(entries, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("install: 仓库里没有插件 %q", id)
	}

	installed, err := List(destRoot)
	if err != nil {
		return nil, err
	}
	newest := ""
	var newestVer version.Version
	for _, e := range installed {
		if e.ID != id {
			continue
		}
		v, err := version.Parse(e.Version)
		if err != nil {
			return nil, fmt.Errorf("install: 已安装版本号非法 %q: %w", e.Version, err)
		}
		if newest == "" || version.Compare(v, newestVer) > 0 {
			newest, newestVer = e.Version, v
		}
	}
	if newest == "" {
		return nil, fmt.Errorf("install: 插件 %q 未安装,请使用 install", id)
	}
	cmp, err := compareVersionStrings(latest.Version, newest)
	if err != nil {
		return nil, err
	}
	switch {
	case cmp == 0:
		return nil, fmt.Errorf("install: 插件 %q 已是最新版本 %s", id, newest)
	case cmp < 0:
		return nil, fmt.Errorf("install: 仓库版本 %s 低于已安装版本 %s,拒绝降级", latest.Version, newest)
	}

	dir, err := resolveRepoDir(repoRoot, latest.Dir)
	if err != nil {
		return nil, err
	}
	preview, err := VerifyDir(dir, opts)
	if err != nil {
		return nil, err
	}
	if preview.ID != id {
		return nil, fmt.Errorf("install: 仓库条目 id %q 与清单 id %q 不一致", id, preview.ID)
	}
	if latest.Digest != "" && preview.Digest != latest.Digest {
		return nil, fmt.Errorf("install: 摘要不匹配(期望 %s,实际 %s)", latest.Digest, preview.Digest)
	}
	info, err := Upgrade(dir, destRoot, opts)
	if err != nil {
		return nil, err
	}
	return info, nil
}
