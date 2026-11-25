// Package loader 是 L2 装载层:把清单变成可挂载的 kernel.Plugin。
//
// 它只负责「按 kind 选路」:
//   - go 插件从宿主注册表取(Go 插件必须编译进宿主,CLI 无法动态加载 .so);
//   - ts 插件从 Artifact 反序列化,或从目录里的源码现场编译。
//
// 它不做依赖排序(assemble.Plan),也不做事务与回滚(assemble.Up)。
package loader

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_jsrt/compile"
)

// Options 控制装载行为。
type Options struct {
	// Go 是 kind=go 的注册表:插件 id → 构造函数。
	// 找不到时返回明确错误——「静默跳过」会让产品少装插件却看不出来。
	Go map[string]func() kernel.Plugin
	// Logf 接收 TS 插件 ctx.log 的输出。
	Logf func(format string, args ...any)
	// Compile 覆盖 TS 源码编译选项。
	Compile compile.Options
}

// sourceExts 是会被当作 TS 源码收集的扩展名。
var sourceExts = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true,
}

// Lookup 返回一个按清单选路的装配查找函数。
//
// 返回值在类型上与 assemble.Lookup 一致(未命名函数类型),可直接传入 assemble.Up,
// 这样装配层不需要知道 compile/bridge 的存在。
func Lookup(opts Options) func(m *plugin.Manifest) (kernel.Plugin, error) {
	return func(m *plugin.Manifest) (kernel.Plugin, error) {
		switch m.Kind {
		case "go":
			mk, ok := opts.Go[m.ID]
			if !ok {
				return nil, fmt.Errorf("loader: go 插件 %q 没有注册实现(Go 插件必须编译进宿主)", m.ID)
			}
			if mk == nil {
				return nil, fmt.Errorf("loader: go 插件 %q 的构造函数为 nil", m.ID)
			}
			return mk(), nil
		case "ts":
			artifact, err := LoadArtifact(m, opts)
			if err != nil {
				return nil, err
			}
			return bridge.New(bridge.Options{Name: m.ID, Artifact: artifact, Logf: opts.Logf,
				Inject: kernel.Inject{Names: m.Inject.Required, Optional: m.Inject.Optional}})
		default:
			return nil, fmt.Errorf("loader: 插件 %q 的 kind %q 未知", m.ID, m.Kind)
		}
	}
}

// LoadArtifact 取得 TS 插件的编译产物:优先读 artifact 文件,否则现场编译源码。
func LoadArtifact(m *plugin.Manifest, opts Options) (*compile.Artifact, error) {
	if m.Artifact != "" {
		path := m.Artifact
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.Dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("loader: 读取插件 %q 的 Artifact %s 失败: %w", m.ID, path, err)
		}
		artifact, err := compile.ParseArtifact(data)
		if err != nil {
			return nil, fmt.Errorf("loader: 插件 %q 的 Artifact 无效: %w", m.ID, err)
		}
		return artifact, nil
	}

	if m.Entry == "" {
		return nil, fmt.Errorf("loader: ts 插件 %q 既没有 artifact 也没有 entry", m.ID)
	}
	if m.Dir == "" {
		return nil, fmt.Errorf("loader: ts 插件 %q 缺少来源目录(清单是内联构造的?)", m.ID)
	}
	sources, err := CollectSources(m.Dir)
	if err != nil {
		return nil, err
	}
	artifact, err := compile.Build(m.ID, m.Entry, sources, opts.Compile)
	if err != nil {
		return nil, fmt.Errorf("loader: 编译插件 %q 失败: %w", m.ID, err)
	}
	return artifact, nil
}

// CollectSources 收集目录下的插件源码(相对路径用 `/` 分隔)。
//
// 跳过 node_modules 与隐藏目录:它们是依赖树,不是插件源码。
func CollectSources(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != dir && (name == "node_modules" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !sourceExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("loader: 扫描目录 %s 失败: %w", dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("loader: 目录 %s 下没有任何 TS/JS 源码", dir)
	}
	return files, nil
}
