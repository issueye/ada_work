// Package install 的已安装插件加载层。
//
// 安装目录里的 plugin.json 只是清单自身;真正的 trust 来自 index.json(由安装签名决定)。
// LoadDir 先按索引选出每个 id 的最新版本,再把索引 trust 写回清单,最后才交给 caps 授权。
package install

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

// LoadDir 加载安装根目录下的最新已安装插件,并用索引里的 trust 覆盖清单自述。
//
// 只接受索引登记、且位于 destRoot 之下的目录;索引条目与清单 id/version 不一致时拒绝。
// 返回值按 id 排序,每个 id 只返回最新版本。
func LoadDir(destRoot string) ([]*plugin.Manifest, error) {
	entries, err := List(destRoot)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	latest := make(map[string]IndexEntry, len(entries))
	for _, e := range entries {
		cur, ok := latest[e.ID]
		if !ok {
			latest[e.ID] = e
			continue
		}
		cmp, err := compareVersionStrings(e.Version, cur.Version)
		if err != nil {
			return nil, err
		}
		if cmp > 0 {
			latest[e.ID] = e
		}
	}

	rootAbs, err := filepath.Abs(destRoot)
	if err != nil {
		return nil, fmt.Errorf("install: 解析安装根目录失败: %w", err)
	}
	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]*plugin.Manifest, 0, len(ids))
	for _, id := range ids {
		e := latest[id]
		dirAbs, err := filepath.Abs(e.Dir)
		if err != nil {
			return nil, fmt.Errorf("install: 解析已安装目录失败: %w", err)
		}
		rel, err := filepath.Rel(rootAbs, dirAbs)
		if err != nil || rel == ".." || filepath.IsAbs(rel) ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("install: 索引里的目录不在安装根之下,拒绝加载: %s", e.Dir)
		}
		ms, err := plugin.LoadDir(e.Dir)
		if err != nil {
			return nil, err
		}
		if len(ms) != 1 {
			return nil, fmt.Errorf("install: 已安装目录 %s 必须恰好包含一个插件,实际 %d 个", e.Dir, len(ms))
		}
		m := ms[0]
		if m.ID != e.ID || m.Version != e.Version {
			return nil, fmt.Errorf("install: 清单 %s@%s 与索引 %s@%s 不一致", m.ID, m.Version, e.ID, e.Version)
		}
		trust := caps.Trust(e.Trust)
		if trust == "" {
			trust = caps.TrustThirdParty
		}
		if !caps.ValidTrust(trust) {
			return nil, fmt.Errorf("install: 索引里 %s 的 trust 非法: %q", e.ID, e.Trust)
		}
		if trust == caps.TrustBuiltin && m.Kind != "go" {
			return nil, fmt.Errorf("install: 非 Go 插件 %s 不能使用 builtin trust", e.ID)
		}
		if !e.Signed && (trust == caps.TrustVerified || trust == caps.TrustOfficial) {
			return nil, fmt.Errorf("install: 索引里 %s 标记 %s 但未签名", e.ID, trust)
		}
		// LoadDir 已把磁盘 TS 插件降为 third-party;这里是安装来源的最终裁决。
		m.Trust = string(trust)
		out = append(out, m)
	}
	return out, nil
}
