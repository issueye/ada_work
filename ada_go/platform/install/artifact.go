// 在线 artifact:下载 zip 并安全解包(拒绝路径逃逸/符号链接/超限)。
package install

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/version"
)

// ArtifactLimit 是插件 zip 的最大字节数(16 MiB)。
const ArtifactLimit int64 = 16 << 20

// ArtifactTimeout 是下载 artfact 的超时。
const ArtifactTimeout = 30 * time.Second

// ArtifactMaxFiles 是解包文件数上限。
const ArtifactMaxFiles = 1000

// FetchArtifact 下载 zip;只允许 http/https,限制大小与超时。
func FetchArtifact(ctx context.Context, source string) ([]byte, error) {
	u, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("install: artifact URL 非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("install: artifact 只支持 http/https: %s", source)
	}
	actx, cancel := context.WithTimeout(ctx, ArtifactTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("install: 构造 artifact 请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("install: 下载 artifact 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("install: 下载 artifact 失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, ArtifactLimit+1))
	if err != nil {
		return nil, fmt.Errorf("install: 读取 artifact 失败: %w", err)
	}
	if int64(len(data)) > ArtifactLimit {
		return nil, fmt.Errorf("install: artifact 超过 %d 字节上限", ArtifactLimit)
	}
	return data, nil
}

// ExtractZip 安全解包到 destDir:拒绝绝对路径/.. 逃逸/符号链接/超限。
func ExtractZip(data []byte, destDir string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("install: artifact 不是合法 zip: %w", err)
	}
	if len(zr.File) > ArtifactMaxFiles {
		return fmt.Errorf("install: artifact 文件数超过上限 %d", ArtifactMaxFiles)
	}
	var total int64
	for _, f := range zr.File {
		name := f.Name
		if name == "" {
			return fmt.Errorf("install: artifact 里有空文件名")
		}
		clean := filepath.Clean(name)
		if filepath.IsAbs(name) || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("install: artifact 路径逃逸: %s", name)
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("install: artifact 不允许符号链接: %s", name)
		}
		dst := filepath.Join(destDir, clean)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("install: 创建目录失败: %w", err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("install: 创建目录失败: %w", err)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("install: 打开 zip 条目失败: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(rc, ArtifactLimit-total+1))
		_ = rc.Close()
		if readErr != nil {
			return fmt.Errorf("install: 读取 zip 条目失败: %w", readErr)
		}
		total += int64(len(data))
		if total > ArtifactLimit {
			return fmt.Errorf("install: artifact 解包后超过 %d 字节上限", ArtifactLimit)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("install: 写文件失败: %w", err)
		}
	}
	return nil
}

// UpdateFromRemote 从在线仓库更新:索引 → 下载 zip → 解包 → 校验 → Upgrade。
func UpdateFromRemote(ctx context.Context, repoURL, id, destRoot string, opts Options) (*Info, error) {
	entries, err := FetchRepoIndex(ctx, repoURL, opts.RepoKeys, opts.RequireRepoSignature)
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
	if latest.Source == "" {
		return nil, fmt.Errorf("install: 仓库条目 %q 缺少 source(artifact URL)", id)
	}
	if err := checkRemoteUpgrade(destRoot, id, latest.Version); err != nil {
		return nil, err
	}
	data, err := FetchArtifact(ctx, latest.Source)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "ada-artifact-*")
	if err != nil {
		return nil, fmt.Errorf("install: 创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := ExtractZip(data, tmp); err != nil {
		return nil, err
	}
	// 允许 zip 里带一层前缀目录:找到含 plugin.json 的目录。
	pluginDir := locatePluginDir(tmp)
	if pluginDir == "" {
		return nil, fmt.Errorf("install: artifact 里没有 plugin.json")
	}
	preview, err := VerifyDir(pluginDir, opts)
	if err != nil {
		return nil, err
	}
	if preview.ID != id {
		return nil, fmt.Errorf("install: artifact id %q 与仓库 id %q 不一致", preview.ID, id)
	}
	if latest.Digest != "" && preview.Digest != latest.Digest {
		return nil, fmt.Errorf("install: 摘要不匹配(期望 %s,实际 %s)", latest.Digest, preview.Digest)
	}
	return Upgrade(pluginDir, destRoot, opts)
}

// PreviewRemoteUpdate 只下载/校验在线 artifact,不落盘(供安装同意预览)。
func PreviewRemoteUpdate(ctx context.Context, repoURL, id string, opts Options) (*Info, error) {
	entries, err := FetchRepoIndex(ctx, repoURL, opts.RepoKeys, opts.RequireRepoSignature)
	if err != nil {
		return nil, err
	}
	latest, ok, err := LatestRepoEntry(entries, id)
	if err != nil || !ok {
		return nil, fmt.Errorf("install: 仓库里没有插件 %q", id)
	}
	if latest.Source == "" {
		return nil, fmt.Errorf("install: 仓库条目 %q 缺少 source(artifact URL)", id)
	}
	data, err := FetchArtifact(ctx, latest.Source)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "ada-artifact-*")
	if err != nil {
		return nil, fmt.Errorf("install: 创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := ExtractZip(data, tmp); err != nil {
		return nil, err
	}
	pluginDir := locatePluginDir(tmp)
	if pluginDir == "" {
		return nil, fmt.Errorf("install: artifact 里没有 plugin.json")
	}
	preview, err := VerifyDir(pluginDir, opts)
	if err != nil {
		return nil, err
	}
	if preview.ID != id {
		return nil, fmt.Errorf("install: artifact id %q 与仓库 id %q 不一致", preview.ID, id)
	}
	if latest.Digest != "" && preview.Digest != latest.Digest {
		return nil, fmt.Errorf("install: 摘要不匹配(期望 %s,实际 %s)", latest.Digest, preview.Digest)
	}
	return preview, nil
}

// locatePluginDir 返回含 plugin.json 的目录;带一层前缀时向下找一层。
func locatePluginDir(root string) string {
	if _, err := os.Stat(filepath.Join(root, "plugin.json")); err == nil {
		return root
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			sub := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(sub, "plugin.json")); err == nil {
				return sub
			}
		}
	}
	return ""
}

// checkRemoteUpgrade 校验已安装版本低于仓库版本(与 UpdateFromRepo 同语义)。
func checkRemoteUpgrade(destRoot, id, latestVersion string) error {
	installed, err := List(destRoot)
	if err != nil {
		return err
	}
	newest := ""
	var newestVer version.Version
	for _, e := range installed {
		if e.ID != id {
			continue
		}
		v, err := version.Parse(e.Version)
		if err != nil {
			return err
		}
		if newest == "" || version.Compare(v, newestVer) > 0 {
			newest = e.Version
			newestVer = v
		}
	}
	if newest == "" {
		return fmt.Errorf("install: 插件 %q 未安装,请使用 install", id)
	}
	cmp, err := compareVersionStrings(latestVersion, newest)
	if err != nil {
		return err
	}
	switch {
	case cmp == 0:
		return fmt.Errorf("install: 插件 %q 已是最新版本 %s", id, newest)
	case cmp < 0:
		return fmt.Errorf("install: 仓库版本 %s 低于已安装版本 %s,拒绝降级", latestVersion, newest)
	}
	return nil
}
