package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Hot update.
//
// The two platforms want different things, so the server just publishes a
// manifest and lets each client decide how to consume it:
//
//   android -> the full APK. Android has no way to swap code under a running
//              process, so "hot" means "download and hand it to the system
//              installer without leaving the app".
//   windows -> the app.asar only. The renderer and main process live entirely
//              inside that one file (~200KB) while the Electron runtime and
//              node_modules stay put, so an update is a fast download and a
//              file swap instead of re-shipping 80MB.
//
// Publishing is deliberately a plain PUT body rather than multipart, because
// the only publisher is our own build script.
// ---------------------------------------------------------------------------

type updateArtifact struct {
	Platform    string `json:"platform"`
	Version     string `json:"version"`
	VersionCode int64  `json:"version_code,omitempty"`
	File        string `json:"file"`
	Series      string `json:"series,omitempty"` // "stable" | "beta"
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Notes       string `json:"notes"`
	Mandatory   bool   `json:"mandatory"`
	PublishedAt int64  `json:"published_at"`

	// Enabled 是"这个包对外发不发"（2026-10-18）。
	//
	// 用指针而不是 bool：老 manifest 里没有这个键，反序列化后 nil。
	// 如果把 nil 当成 false，升级到这一版的服务器会**瞬间停掉所有分发**，
	// 而客户端只会说"没有新版本"，没有任何地方能看出原因。
	// nil 一律解释为"启用"，只有显式写了 false 才是停用。
	Enabled *bool `json:"enabled,omitempty"`

	// Source 记录这个包是怎么来的：空/"local" 是本机上传，"clone" 是从别的
	// 站点拉过来的。面板用它决定要不要显示"来源"这一列。
	Source string `json:"source,omitempty"`
}

// isEnabled 把 nil 解释成启用。见 Enabled 字段上的说明。
func (a *updateArtifact) isEnabled() bool {
	if a == nil {
		return false
	}
	return a.Enabled == nil || *a.Enabled
}

type updateManifest struct {
	Android *updateArtifact `json:"android,omitempty"`
	Windows *updateArtifact `json:"windows,omitempty"`
}

func (s *Server) updatesDir() string { return filepath.Join(s.cfg.DataDir, "updates") }

func (s *Server) manifestPath() string {
	return filepath.Join(s.updatesDir(), "manifest.json")
}

func (s *Server) loadManifest() updateManifest {
	var m updateManifest
	raw, err := os.ReadFile(s.manifestPath())
	if err != nil {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}

func (s *Server) saveManifest(m updateManifest) error {
	if err := os.MkdirAll(s.updatesDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.manifestPath(), raw, 0o644)
}

// compareVersions orders dotted numeric versions. Anything unparsable falls
// back to a plain string comparison so a weird version never crashes a check.
func compareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int64
		if i < len(pa) {
			x, _ = strconv.ParseInt(strings.TrimSpace(pa[i]), 10, 64)
		}
		if i < len(pb) {
			y, _ = strconv.ParseInt(strings.TrimSpace(pb[i]), 10, 64)
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// handleUpdatesPublic 是给官网用的**公开**产物列表（2026-09-30）。
//
// 官网的下载按钮要指向"最新包"，而它原本是去扫自己那台机器上的 downloads 目录 ——
// 那个目录是**手动放的**，于是长期停在旧版本（0.6.4 / 0.12.0），
// 配上 COS 之后更糟：拼出来的地址指向**对象存储里不存在的对象**，点了直接 404。
//
// 现在改成问这里。**只有一个数据源**（发布时写进 manifest 的那份），
// 官网再也不会和客户端看到不同的版本。
//
// ⚠️ 匿名可读，所以返回字段要**克制**：只给下载必需的（版本/文件名/大小/哈希/地址），
//
//	不要把 notes（发布备注）之类的内部信息顺手带出去。
func (s *Server) handleUpdatesPublic(w http.ResponseWriter, r *http.Request) {
	m := s.loadManifest()
	out := map[string]any{}
	for _, p := range []string{"android", "windows"} {
		var art *updateArtifact
		switch p {
		case "android":
			art = m.Android
		case "windows":
			art = m.Windows
		}
		if art == nil || !art.isEnabled() {
			out[p] = nil
			continue
		}
		out[p] = map[string]any{
			"version":      art.Version,
			"file":         filepath.Base(art.File),
			"size":         art.Size,
			"sha256":       art.SHA256,
			"published_at": art.PublishedAt,
			"download_url": s.downloadURLFor(p, art),
		}
	}
	writeJSON(w, 200, out)
}

// handleUpdateCheck answers "should this client upgrade?".
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	platform := strings.ToLower(strings.TrimSpace(q.Get("platform")))
	current := strings.TrimSpace(q.Get("version"))
	code, _ := strconv.ParseInt(q.Get("version_code"), 10, 64)

	m := s.loadManifest()
	out := map[string]any{
		"platform":     platform,
		"current":      map[string]any{"version": current, "version_code": code},
		"need_update":  false,
		"latest":       nil,
		"server_time":  time.Now().Unix(),
		"upload_notes": "客户端版本比对：android 看 version_code，windows 看 version 号。",
	}

	var art *updateArtifact
	switch platform {
	case "android":
		art = m.Android
	case "windows":
		art = m.Windows
	default:
		apiErr(w, 400, "platform 只能是 android 或 windows")
		return
	}
	if art == nil || !art.isEnabled() {
		writeJSON(w, 200, out)
		return
	}

	newer := false
	if platform == "android" {
		newer = art.VersionCode > code
	} else {
		newer = compareVersions(art.Version, current) > 0
	}
	out["latest"] = map[string]any{
		"version":      art.Version,
		"version_code": art.VersionCode,
		"notes":        art.Notes,
		"mandatory":    art.Mandatory,
		"size":         art.Size,
		"sha256":       art.SHA256,
		"published_at": art.PublishedAt,
		"download_url": s.downloadURLFor(platform, art),
	}
	out["need_update"] = newer
	writeJSON(w, 200, out)
}

// downloadURLFor 决定客户端从哪儿下这个包。
//
// 配了 `-cos-base` 就走对象存储（+CDN），否则回落到本机的下载接口。
// 客户端**不需要改**：它对"不是自家路径"的地址是原样使用的
// （见 `ApiClient.downloadUpdate` 里的 `ownDownloadPath` 判断）。
//
// ⚠️ 键的拼法 `<prefix>/<platform>/<文件名>` 必须和发布脚本
// `scripts/lib/cos.mjs` 的 `cosKeyFor()` 严格一致 —— 两边错一个字符就是 404，
// 而客户端只会报一句笼统的"下载失败"，很难查。
func (s *Server) downloadURLFor(platform string, art *updateArtifact) string {
	local := "/api/updates/download/" + platform
	if art == nil || art.File == "" {
		return local
	}
	base := strings.TrimRight(strings.TrimSpace(s.cfg.COSBase), "/")
	if base == "" {
		return local
	}
	return base + "/" + platform + "/" + filepath.Base(art.File)
}

// handleUpdateDownload streams the current artifact for a platform.
func (s *Server) handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	platform := strings.ToLower(r.PathValue("platform"))
	m := s.loadManifest()
	var art *updateArtifact
	switch platform {
	case "android":
		art = m.Android
	case "windows":
		art = m.Windows
	}
	if art == nil {
		apiErr(w, 404, "这个平台还没有可下载的更新包")
		return
	}
	if !art.isEnabled() {
		apiErr(w, 404, "这个平台的更新包已停止分发")
		return
	}
	// 联动路由：命中规则就交给别的服务器发（见 federation.go）。
	if s.tryRoute(w, r, "update", platform) {
		return
	}
	full := filepath.Join(s.updatesDir(), filepath.Base(art.File))
	f, err := os.Open(full)
	if err != nil {
		apiErr(w, 410, "更新包文件不在服务器上了："+err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, art.File))
	w.Header().Set("X-Version", art.Version)
	w.Header().Set("X-SHA256", art.SHA256)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	http.ServeContent(w, r, art.File, st.ModTime(), f)
}

// handleUpdatePublish takes a raw body plus query parameters. Admin only.
//
//	POST /api/updates/publish?platform=android&version=0.2.0&version_code=2&notes=...
func (s *Server) handleUpdatePublish(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(userIDOf(r)) {
		apiErr(w, 403, "只有管理员能发布更新")
		return
	}
	q := r.URL.Query()
	platform := strings.ToLower(strings.TrimSpace(q.Get("platform")))
	if platform != "android" && platform != "windows" {
		apiErr(w, 400, "platform 只能是 android 或 windows")
		return
	}
	vers := strings.TrimSpace(q.Get("version"))
	if vers == "" {
		apiErr(w, 400, "缺少 version")
		return
	}
	code, _ := strconv.ParseInt(q.Get("version_code"), 10, 64)
	if platform == "android" && code <= 0 {
		apiErr(w, 400, "android 更新必须带 version_code（整数，只能增不能减）")
		return
	}
	name := strings.TrimSpace(q.Get("filename"))
	if name == "" {
		name = q.Get("name")
	}
	if name == "" {
		ext := ".bin"
		if platform == "android" {
			ext = ".apk"
		} else {
			ext = ".asar"
		}
		name = fmt.Sprintf("%s-%s%s", platform, vers, ext)
	}
	// Never let a caller escape the updates directory.
	name = filepath.Base(name)
	if name == "." || name == ".." || name == "" {
		apiErr(w, 400, "文件名不合法")
		return
	}
	if err := os.MkdirAll(s.updatesDir(), 0o755); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	dst := filepath.Join(s.updatesDir(), name)
	f, err := os.Create(dst)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer f.Close()

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), r.Body)
	if err != nil {
		_ = os.Remove(dst)
		apiErr(w, 500, "写入失败："+err.Error())
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))

	art := updateArtifact{
		Platform:    platform,
		Version:     vers,
		VersionCode: code,
		File:        name,
		Series:      orDefaultStr(q.Get("series"), "stable"),
		SHA256:      sum,
		Size:        written,
		Notes:       q.Get("notes"),
		Mandatory:   q.Get("mandatory") == "1" || q.Get("mandatory") == "true",
		PublishedAt: time.Now().Unix(),
	}
	m := s.loadManifest()
	if platform == "android" {
		m.Android = &art
	} else {
		m.Windows = &art
	}
	if err := s.saveManifest(m); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	log.Printf("update published: %s %s (%d bytes, sha256=%s)", platform, vers, written, sum[:12])
	writeJSON(w, 200, map[string]any{"ok": true, "artifact": art})
}

// handleUpdateManifest lets the admin see what is currently published.
func (s *Server) handleUpdateManifest(w http.ResponseWriter, r *http.Request) {
	// ⚠️ 2026-10-16 补：路由上只包了 s.auth，而这里返回的是发布备注（notes）、
	//    updates/ 目录的完整文件清单，以及 `dir` —— 服务器上的**绝对路径**。
	//    同文件的 handleUpdatePublish 一直有闸门，这一处是漏的；
	//    main.go 注册路由那句注释自己都写着「那个是**管理员**接口」。
	if s.requireAdmin(w, r) {
		return
	}
	m := s.loadManifest()
	dir := s.updatesDir()
	files := []map[string]any{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || e.Name() == "manifest.json" {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, map[string]any{
				"name": e.Name(), "size": info.Size(),
				"modified_at": info.ModTime().Unix(),
			})
		}
	}
	writeJSON(w, 200, map[string]any{
		"android": m.Android, "windows": m.Windows,
		"files": files, "dir": dir,
	})
}

// ---------- device version reporting ----------

// handleDeviceReport records which build a device is running. Useful for the
// "did everyone actually upgrade" question, which is otherwise unanswerable.
func (s *Server) handleDeviceReport(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var body struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
		Version  string `json:"version"`
	}
	_ = readJSON(r, &body)
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = newID("dev_")
	}
	_, err := s.db.sql.Exec(`
		INSERT INTO devices(id,user_id,name,platform,last_seen_at,version)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, platform=excluded.platform,
			last_seen_at=excluded.last_seen_at, version=excluded.version`,
		id, uid, body.Name, body.Platform, time.Now().Unix(), body.Version)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "device_id": id})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(userIDOf(r)) {
		apiErr(w, 403, "只有管理员能查看设备列表")
		return
	}
	rows, err := s.db.sql.Query(`
		SELECT d.id, d.name, d.platform, d.version, d.last_seen_at, u.name
		FROM devices d LEFT JOIN users u ON u.id = d.user_id
		ORDER BY d.last_seen_at DESC`)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, platform, vers, uname string
		var seen int64
		if err := rows.Scan(&id, &name, &platform, &vers, &seen, &uname); err == nil {
			out = append(out, map[string]any{
				"id": id, "name": name, "platform": platform,
				"version": vers, "last_seen_at": seen, "user": uname,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}

func (s *Server) isAdmin(uid string) bool {
	var admin int64
	if err := s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&admin); err != nil {
		return false
	}
	return admin != 0
}

func orDefaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
