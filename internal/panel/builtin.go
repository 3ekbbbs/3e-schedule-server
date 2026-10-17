package panel

// 内置插件包。
//
// "内置"不等于"已安装"：这些文件只是**随 exe 带了一份安装源**，
// 用户在面板里点「安装」之后才被展开到 `data/panel-plugins/<id>/`。
// 这样做的两个好处：
//
//  1. 不装的人不用付出任何运行时成本（不加载、不出现在侧边栏）；
//  2. 卸载以后想装回来，不需要再去网上找包。
//
// ⚠️ 内置包里的文件是**构建产物**，不要在这里直接改 ——
//    源在 `family-schedule/web/public/`，由 `scripts/build-panel-plugins.mjs`
//    生成，改了会被下次构建覆盖。

import (
	"embed"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed builtin
var builtinFS embed.FS

const builtinRoot = "builtin/plugins"

// builtinManifests 列出内置包的清单（按 id 排序，保证面板里顺序稳定）。
func builtinManifests() []pluginManifest {
	entries, err := builtinFS.ReadDir(builtinRoot)
	if err != nil {
		return nil
	}
	out := make([]pluginManifest, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := builtinFS.ReadFile(path.Join(builtinRoot, e.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var m pluginManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if !validPluginID(m.ID) {
			continue
		}
		if m.Name == "" {
			m.Name = m.ID
		}
		if m.Entry == "" {
			m.Entry = "index.html"
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// extractBuiltin 把内置包展开到 dest，返回写出的文件数。
func extractBuiltin(id, dest string) (int, error) {
	root := path.Join(builtinRoot, id)
	sub, err := fs.Sub(builtinFS, root)
	if err != nil {
		return 0, err
	}
	n := 0
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "." {
			return err
		}
		raw, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dest, filepath.FromSlash(p))), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(p)), raw, 0o644); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

// readBuiltinFile 从内置包里读一个文件（数据目录里的副本缺失时兜底）。
func readBuiltinFile(id, name string) ([]byte, bool) {
	if id == "" || strings.Contains(name, "..") {
		return nil, false
	}
	raw, err := builtinFS.ReadFile(path.Join(builtinRoot, id, name))
	if err != nil {
		return nil, false
	}
	return raw, true
}
