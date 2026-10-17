package panel

// 面板插件。
//
// 解决的问题：标注台（导航 / 外观）这类"只有一部分人用得上"的界面不该
// 长在主程序里 —— 它们体积大、更新节奏和主程序不同，而且一旦装进去就
// 永远跟着发版走。做成插件之后：
//
//   · 主程序只认一个目录约定（`data/panel-plugins/<id>/`）；
//   · 装不装、开不开由用户在面板里决定，卸载就是真的从磁盘上消失；
//   · 插件自己是一个完整的静态站，可以独立更新，不用重编主程序。
//
// ★ 边界必须先说清楚：**插件跑在面板的源上，等同于面板的权限**。
// 所以安装界面上写的是"只装你信任的来源"，而不是"插件是沙箱" ——
// 它不是沙箱，同源脚本能拿到会话能调所有接口。把这句话写在代码里，
// 是为了避免下一个人误以为这里有一层隔离。
//
// 目录约定：
//
//	data/panel-plugins/
//	  state.json                 ← 启用状态（卸载后该项会被删掉）
//	  navstudio/
//	    manifest.json
//	    index.html
//	    ...
//
// 请求路径：
//
//	GET  /plugin/<id>/<file>    静态文件（磁盘优先，内置包兜底）
//	ANY  /plugin/<id>/api/<...>  代理到服务核心（用会话里的管理员令牌）

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// pluginManifest 是插件包里唯一的必需文件。
type pluginManifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// Icon 是一个字符（emoji）或两三个汉字的缩写，面板侧边栏直接显示。
	Icon      string `json:"icon,omitempty"`
	Entry     string `json:"entry,omitempty"`
	Publisher string `json:"publisher,omitempty"`
	// APIPrefix 留空表示这个插件不需要访问服务端业务接口。
	// 非空时面板会把 `/plugin/<id>/api/*` 代理到服务核心。
	API bool `json:"api,omitempty"`
	// MinPanel 是要求的最低面板版本。留空不校验 —— 现阶段面板还没有
	// 破坏性的插件接口变更，先不做版本比较，免得拿一个写错的值把插件锁死。
	MinPanel string `json:"min_panel,omitempty"`
}

type pluginState struct {
	Enabled     bool   `json:"enabled"`
	Source      string `json:"source"` // builtin / upload
	InstalledAt int64  `json:"installed_at"`
}

type pluginRegistry struct {
	Installed map[string]pluginState `json:"installed"`
}

// pluginView 是给前端的一条。
type pluginView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Entry       string `json:"entry"`
	Publisher   string `json:"publisher,omitempty"`
	Source      string `json:"source,omitempty"`
	Installed   bool   `json:"installed"`
	Enabled     bool   `json:"enabled"`
	InstalledAt int64  `json:"installed_at,omitempty"`
	URL         string `json:"url,omitempty"`
	HasAPI      bool   `json:"has_api,omitempty"`
	Broken      string `json:"broken,omitempty"`
}

func (p *Panel) pluginsDir() string {
	return filepath.Join(p.cfg.DataDir, "panel-plugins")
}

func (p *Panel) pluginDir(id string) string {
	return filepath.Join(p.pluginsDir(), id)
}

func (p *Panel) pluginStatePath() string {
	return filepath.Join(p.pluginsDir(), "state.json")
}

// validPluginID 与 navpack 的 packId 同一条规则：它会变成磁盘上的目录名。
// 允许大写或 `/` 就等于允许写到任意位置。
func validPluginID(id string) bool {
	if len(id) < 2 || len(id) > 48 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' {
			continue
		}
		if i > 0 && (c >= '0' && c <= '9' || c == '-') {
			continue
		}
		return false
	}
	return true
}

// safeZipName 只接受"普通的相对路径"。
//
// 拒绝三类：绝对路径（`/x`、`C:\x`）、任何一段 `..`、以及反斜杠伪装的
// 分隔符（Windows 上 `..\..\x` 和 `../../x` 是一回事）。
func safeZipName(name string) (string, bool) {
	n := strings.ReplaceAll(strings.TrimPrefix(name, "./"), "\\", "/")
	if n == "" || n == "." {
		return "", false
	}
	if strings.HasPrefix(n, "/") {
		return "", false
	}
	// `C:foo` 这种在 Windows 上会被当成盘符相对路径。
	if len(n) >= 2 && n[1] == ':' {
		return "", false
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", false
		}
	}
	return n, true
}

func (p *Panel) loadPluginRegistry() pluginRegistry {
	reg := pluginRegistry{Installed: map[string]pluginState{}}
	raw, err := os.ReadFile(p.pluginStatePath())
	if err != nil {
		return reg
	}
	// 读坏了当空表：结果是"插件显示成没装"，而不是面板起不来。
	_ = json.Unmarshal(raw, &reg)
	if reg.Installed == nil {
		reg.Installed = map[string]pluginState{}
	}
	return reg
}

func (p *Panel) savePluginRegistry(reg pluginRegistry) error {
	if err := os.MkdirAll(p.pluginsDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.pluginStatePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.pluginStatePath())
}

// readManifest 读磁盘上的插件清单。读不到 / 解析失败都返回错误，
// 由调用方决定"跳过这一条"还是"报给用户"。
func (p *Panel) readManifest(id string) (*pluginManifest, error) {
	raw, err := os.ReadFile(filepath.Join(p.pluginDir(id), "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m pluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json 不是合法 JSON：%w", err)
	}
	if !validPluginID(m.ID) {
		return nil, fmt.Errorf("manifest.json 里的 id 不合法：%q", m.ID)
	}
	if m.ID != id {
		return nil, fmt.Errorf("目录名 %q 与 manifest.json 里的 id %q 不一致", id, m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		m.Name = m.ID
	}
	if m.Entry == "" {
		m.Entry = "index.html"
	}
	m.Entry = strings.TrimPrefix(path.Clean("/"+m.Entry), "/")
	if m.Entry == "" || m.Entry == "." || strings.Contains(m.Entry, "..") {
		return nil, errors.New("manifest.json 里的 entry 不合法")
	}
	return &m, nil
}

// listPlugins 汇总"已装的"与"内置但没装的"。
func (p *Panel) listPlugins() []pluginView {
	reg := p.loadPluginRegistry()
	seen := map[string]bool{}
	out := make([]pluginView, 0, 4)

	entries, _ := os.ReadDir(p.pluginsDir())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if !validPluginID(id) {
			continue
		}
		m, err := p.readManifest(id)
		if err != nil {
			// ★ 坏掉的插件要**出现**，不能静默消失 —— 否则用户看到的是
			// "我明明装了，列表里没有"，无从下手。
			st, ok := reg.Installed[id]
			out = append(out, pluginView{
				ID: id, Name: id, Installed: ok, Enabled: ok && st.Enabled,
				Source: st.Source, InstalledAt: st.InstalledAt,
				Broken: err.Error(),
			})
			seen[id] = true
			continue
		}
		st, ok := reg.Installed[id]
		if !ok {
			// 目录在、状态里没有：按"装了且启用"处理，并补一条状态。
			st = pluginState{Enabled: true, Source: "upload", InstalledAt: time.Now().Unix()}
			reg.Installed[id] = st
			ok = true
		}
		out = append(out, pluginView{
			ID: m.ID, Name: m.Name, Version: m.Version, Description: m.Description,
			Icon: m.Icon, Entry: m.Entry, Publisher: m.Publisher,
			Source: st.Source, Installed: true, Enabled: st.Enabled,
			InstalledAt: st.InstalledAt, HasAPI: m.API,
			URL: "/plugin/" + m.ID + "/" + m.Entry,
		})
		seen[id] = true
	}

	// 内置包：嵌在 exe 里，但**没有安装就没有任何作用**。
	for _, m := range builtinManifests() {
		if seen[m.ID] {
			continue
		}
		out = append(out, pluginView{
			ID: m.ID, Name: m.Name, Version: m.Version, Description: m.Description,
			Icon: m.Icon, Entry: m.Entry, Publisher: m.Publisher,
			Source: "builtin", Installed: false, Enabled: false, HasAPI: m.API,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Installed != out[j].Installed {
			return out[i].Installed
		}
		return out[i].Name < out[j].Name
	})

	// 顺手把补出来的状态落盘。失败不影响本次返回。
	_ = p.savePluginRegistry(reg)
	return out
}

// ---- 安装 / 卸载 ----

func (p *Panel) handlePluginList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"plugins": p.listPlugins()})
}

// handlePluginInstallBuiltin 把内置包展开到 data 目录。
func (p *Panel) handlePluginInstallBuiltin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !validPluginID(body.ID) {
		writeErr(w, http.StatusBadRequest, "插件 id 不合法")
		return
	}
	if err := p.installBuiltin(body.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "安装失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) installBuiltin(id string) error {
	dest := p.pluginDir(id)
	// 先写临时目录再改名：中途失败不会留下半个插件。
	tmp := dest + ".installing"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	n, err := extractBuiltin(id, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if n == 0 {
		_ = os.RemoveAll(tmp)
		return errors.New("内置包里没有文件")
	}
	_ = os.RemoveAll(dest + ".bak")
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, dest+".bak"); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		// 回滚，别让用户失去原来的版本。
		_ = os.Rename(dest+".bak", dest)
		_ = os.RemoveAll(tmp)
		return err
	}
	_ = os.RemoveAll(dest + ".bak")

	reg := p.loadPluginRegistry()
	reg.Installed[id] = pluginState{Enabled: true, Source: "builtin", InstalledAt: time.Now().Unix()}
	return p.savePluginRegistry(reg)
}

// handlePluginUpload 接收一个 zip。zip 里必须有一份 manifest.json。
//
// 大小上限 64MB —— 标注台这种带地图资源的包会到几 MB，留够余量；
// 再大就不该走面板上传了。
func (p *Panel) handlePluginUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "上传内容过大或格式不对（需要 zip）")
		return
	}
	file, hdr, err := r.FormFile("zip")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请选择一个 .zip 插件包")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 64<<20))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取上传文件失败")
		return
	}
	if hdr != nil && hdr.Size > 64<<20 {
		writeErr(w, http.StatusBadRequest, "插件包超过 64MB")
		return
	}
	id, err := p.installZip(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "安装失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// installZip 解包并安装。返回插件 id。
func (p *Panel) installZip(raw []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", errors.New("这不是一个 zip 文件")
	}

	// 找到 manifest.json，并确定包内的根前缀。
	// 允许两种形状：manifest.json 在根，或在一个顶层目录里（macOS 压缩常见）。
	prefix := ""
	var manifestFile *zip.File
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		if name == "manifest.json" || strings.HasSuffix(name, "/manifest.json") {
			if manifestFile == nil || len(name) < len(manifestFile.Name) {
				manifestFile = f
			}
		}
	}
	if manifestFile == nil {
		return "", errors.New("包里没有 manifest.json")
	}
	{
		name := strings.TrimPrefix(manifestFile.Name, "./")
		prefix = strings.TrimSuffix(name, "manifest.json")
	}

	mf, err := manifestFile.Open()
	if err != nil {
		return "", err
	}
	meta, err := io.ReadAll(io.LimitReader(mf, 256<<10))
	_ = mf.Close()
	if err != nil {
		return "", err
	}
	var m pluginManifest
	if err := json.Unmarshal(meta, &m); err != nil {
		return "", errors.New("manifest.json 不是合法 JSON")
	}
	if !validPluginID(m.ID) {
		return "", fmt.Errorf("manifest.json 里的 id 不合法：%q（小写字母开头，只能有小写字母/数字/连字符）", m.ID)
	}

	dest := p.pluginDir(m.ID)
	tmp := dest + ".installing"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }

	var total int64
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		if prefix != "" {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			name = strings.TrimPrefix(name, prefix)
		}
		if name == "" {
			continue
		}
		// ★ 解包必须自己判路径：zip 里写 `../../x` 是经典的任意写。
		//
		// ⚠️ 这里**先判再 Clean**。反过来的话 `path.Clean("/../evil.txt")`
		// 会得到 `/evil.txt` —— 不越界，但等于把一个恶意输入**悄悄改写成
		// 合法路径**。用户看到的结果是"装上了"，而包里那条路径被吃掉了。
		rel, ok := safeZipName(name)
		if !ok {
			cleanup()
			return "", fmt.Errorf("包里有一条不安全的路径：%s", f.Name)
		}
		out := filepath.Join(tmp, filepath.FromSlash(rel))
		if !strings.HasPrefix(out, tmp+string(os.PathSeparator)) {
			cleanup()
			return "", fmt.Errorf("包里有一条不安全的路径：%s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(out, 0o755); err != nil {
				cleanup()
				return "", err
			}
			continue
		}
		// 符号链接一律拒绝：它会绕过上面所有路径判断。
		if f.Mode()&os.ModeSymlink != 0 {
			cleanup()
			return "", fmt.Errorf("包里含有符号链接，已拒绝：%s", f.Name)
		}
		total += int64(f.UncompressedSize64)
		if total > 128<<20 {
			cleanup()
			return "", errors.New("解压后体积超过 128MB")
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			cleanup()
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			cleanup()
			return "", err
		}
		w, err := os.Create(out)
		if err != nil {
			_ = rc.Close()
			cleanup()
			return "", err
		}
		_, err = io.Copy(w, io.LimitReader(rc, 64<<20))
		_ = w.Close()
		_ = rc.Close()
		if err != nil {
			cleanup()
			return "", err
		}
	}

	if _, err := os.Stat(filepath.Join(tmp, "manifest.json")); err != nil {
		cleanup()
		return "", errors.New("包里没有 manifest.json")
	}

	_ = os.RemoveAll(dest + ".bak")
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, dest+".bak"); err != nil {
			cleanup()
			return "", err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Rename(dest+".bak", dest)
		cleanup()
		return "", err
	}
	_ = os.RemoveAll(dest + ".bak")

	reg := p.loadPluginRegistry()
	reg.Installed[m.ID] = pluginState{Enabled: true, Source: "upload", InstalledAt: time.Now().Unix()}
	if err := p.savePluginRegistry(reg); err != nil {
		return m.ID, err
	}
	return m.ID, nil
}

func (p *Panel) handlePluginToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPluginID(id) {
		writeErr(w, http.StatusBadRequest, "插件 id 不合法")
		return
	}
	if _, err := p.readManifest(id); err != nil {
		writeErr(w, http.StatusNotFound, "这个插件没有装，或者包是坏的："+err.Error())
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	reg := p.loadPluginRegistry()
	st := reg.Installed[id]
	st.Enabled = body.Enabled
	if st.Source == "" {
		st.Source = "upload"
	}
	if st.InstalledAt == 0 {
		st.InstalledAt = time.Now().Unix()
	}
	reg.Installed[id] = st
	if err := p.savePluginRegistry(reg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": st.Enabled})
}

func (p *Panel) handlePluginUninstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validPluginID(id) {
		writeErr(w, http.StatusBadRequest, "插件 id 不合法")
		return
	}
	if err := os.RemoveAll(p.pluginDir(id)); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败："+err.Error())
		return
	}
	reg := p.loadPluginRegistry()
	delete(reg.Installed, id)
	_ = p.savePluginRegistry(reg)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 静态与代理 ----

// handlePlugin 处理 /plugin/<id>/... 的全部请求。
func (p *Panel) handlePlugin(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/plugin/")
	id, tail, _ := strings.Cut(rest, "/")
	if !validPluginID(id) {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(tail, "api/") {
		p.pluginProxy(w, r, id, strings.TrimPrefix(tail, "api/"))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg := p.loadPluginRegistry()
	st, installed := reg.Installed[id]
	if installed && !st.Enabled {
		// 停用的插件不提供资源：否则"停用"只是从侧边栏消失了，直连还能用。
		http.Error(w, "插件已停用", http.StatusForbidden)
		return
	}
	p.servePluginFile(w, r, id, tail, installed)
}

// servePluginFile 伺服插件目录里的文件。
//
// installed 来自调用方已经读过的注册表：没装（比如刚卸载）就**不能**用内置包兜底，
// 否则"卸载"只是把目录删了，页面照样能打开、直连地址也还能用。
func (p *Panel) servePluginFile(w http.ResponseWriter, r *http.Request, id, tail string, installed bool) {
	clean := strings.TrimPrefix(path.Clean("/"+tail), "/")
	if strings.Contains(clean, "..") {
		http.NotFound(w, r)
		return
	}
	if clean == "" || clean == "." {
		m, err := p.readManifest(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		clean = m.Entry
	}
	root := p.pluginDir(id)
	full := filepath.Join(root, filepath.FromSlash(clean))
	if !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil && installed {
		// 内置包兜底：数据目录里的副本丢了（用户手动删过），
		// 还能从 exe 里读出来 —— 但**只对"已安装"的插件**这么做。
		if data2, ok := readBuiltinFile(id, clean); ok {
			data = data2
			err = nil
		}
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(path.Ext(clean)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else if strings.HasSuffix(clean, ".js") {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	} else if strings.HasSuffix(clean, ".json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	// 插件会自己更新（换版本），不缓存最省心。
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// pluginProxy 把 /plugin/<id>/api/... 转到服务核心。
//
// 与 /api/panel/proxy 的区别只有一个：这里的路径前缀属于插件自己，
// 插件因此可以继续用 `/api/...` 那套写法（把 /api 换成 api/ 即可），
// 不必知道面板的代理路径长什么样。
func (p *Panel) pluginProxy(w http.ResponseWriter, r *http.Request, id, rest string) {
	sess := p.sessionOf(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "请先登录面板")
		return
	}
	if !validPluginID(id) {
		http.NotFound(w, r)
		return
	}
	reg := p.loadPluginRegistry()
	if st, ok := reg.Installed[id]; !ok || !st.Enabled {
		writeErr(w, http.StatusForbidden, "插件没有安装或已停用")
		return
	}
	base := p.base()
	if base == "" {
		writeErr(w, http.StatusServiceUnavailable, "服务端没有可用的监听地址")
		return
	}
	target := base + "/api/" + strings.TrimPrefix(rest, "/")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "X-Live-Ticket"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	// ★ 用面板会话里的令牌**覆盖**插件自己带的 Authorization。
	// 这样插件页面即使用了一串过期的令牌也不会失败，同时浏览器里
	// 也拿不到真正的管理员令牌。
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	if r.ContentLength > 0 {
		req.ContentLength = r.ContentLength
	}

	resp, err := proxyHTTP().Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "连接服务端失败："+err.Error())
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Disposition", "Cache-Control", "X-Pack-Id", "X-Pack-Version", "X-SHA256"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
