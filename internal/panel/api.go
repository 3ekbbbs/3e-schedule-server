package panel

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/3ekbbbs/schedule-server/internal/certgen"
	"github.com/3ekbbbs/schedule-server/internal/config"
)

func (p *Panel) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/panel/login", p.handleLogin)
	mux.HandleFunc("POST /api/panel/register", p.handleRegister)
	mux.HandleFunc("POST /api/panel/logout", p.handleLogout)

	// /state 不要求登录：登录页需要知道“服务是否在运行 / 是不是首次运行”。
	// 未登录时不返回配置，避免向未授权访问者暴露监听地址与目录。
	mux.HandleFunc("GET /api/panel/state", p.handleState)
	mux.HandleFunc("GET /api/panel/system", p.auth(p.handleSystem))

	mux.HandleFunc("POST /api/panel/server/start", p.write(p.handleServerStart))
	mux.HandleFunc("POST /api/panel/server/stop", p.write(p.handleServerStop))
	mux.HandleFunc("POST /api/panel/server/restart", p.write(p.handleServerRestart))
	mux.HandleFunc("GET /api/panel/logs", p.auth(p.handleLogs))

	mux.HandleFunc("GET /api/panel/config", p.auth(p.handleGetConfig))
	mux.HandleFunc("PUT /api/panel/config", p.write(p.handlePutConfig))
	mux.HandleFunc("POST /api/panel/cert/regenerate", p.write(p.handleCertRegenerate))
	mux.HandleFunc("POST /api/panel/cert/upload", p.write(p.handleCertUpload))

	mux.HandleFunc("POST /api/panel/backup", p.write(p.handleBackup))
	mux.HandleFunc("GET /api/panel/backups", p.auth(p.handleBackups))
	mux.HandleFunc("GET /api/panel/backup/download", p.auth(p.handleBackupDownload))
	mux.HandleFunc("POST /api/panel/restore", p.write(p.handleRestore))
	mux.HandleFunc("GET /api/panel/deploy-token", p.auth(p.handleGetDeployToken))
	mux.HandleFunc("POST /api/panel/deploy-token", p.write(p.handleNewDeployToken))

	// 插件（标注台这类可选界面）。安装、启停、卸载都走面板会话；
	// 插件自己的静态资源与业务接口在 /plugin/<id>/ 下，见 plugins.go。
	mux.HandleFunc("GET /api/panel/plugins", p.auth(p.handlePluginList))
	mux.HandleFunc("POST /api/panel/plugins/install-builtin", p.write(p.handlePluginInstallBuiltin))
	mux.HandleFunc("POST /api/panel/plugins/upload", p.write(p.handlePluginUpload))
	mux.HandleFunc("POST /api/panel/plugins/{id}/toggle", p.write(p.handlePluginToggle))
	mux.HandleFunc("DELETE /api/panel/plugins/{id}", p.write(p.handlePluginUninstall))
	mux.HandleFunc("/plugin/", p.handlePlugin)

	mux.HandleFunc("GET /api/panel/proxy/", p.auth(p.handleProxy))
	mux.HandleFunc("/", p.handleStatic)
}

// readJSON 读一个小的 JSON 请求体。失败时已经写过响应，调用方直接 return。
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		writeErr(w, http.StatusBadRequest, "请求体是空的")
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对："+err.Error())
		return false
	}
	return true
}

// ---- 鉴权 ----

func (p *Panel) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.sessionOf(r) == nil {
			writeErr(w, http.StatusUnauthorized, "请先登录")
			return
		}
		next(w, r)
	}
}

// write 在鉴权之外还要求自定义请求头，挡住跨站表单提交。
func (p *Panel) write(next http.HandlerFunc) http.HandlerFunc {
	return p.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Panel-Request") != "1" {
			writeErr(w, http.StatusForbidden, "缺少面板请求头")
			return
		}
		next(w, r)
	})
}

func (p *Panel) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || in.Pass == "" {
		writeErr(w, http.StatusBadRequest, "请输入用户名和密码")
		return
	}
	body, _ := json.Marshal(map[string]string{"name": in.Name, "pass": in.Pass})
	resp, err := p.callServer(http.MethodPost, "/api/auth/login", "", body)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "服务端未就绪："+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		writeErr(w, resp.StatusCode, serverMessage(raw, "登录失败"))
		return
	}
	var out struct {
		Token string `json:"token"`
		User  struct {
			Name    string `json:"name"`
			IsAdmin bool   `json:"is_admin"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		writeErr(w, http.StatusBadGateway, "服务端返回格式异常")
		return
	}
	if !out.User.IsAdmin {
		// 不是管理员就没必要留着这个会话，顺手注销。
		req, _ := http.NewRequest(http.MethodPost, p.base()+"/api/auth/logout", nil)
		req.Header.Set("Authorization", "Bearer "+out.Token)
		if resp2, err := proxyHTTP().Do(req); err == nil {
			_ = resp2.Body.Close()
		}
		writeErr(w, http.StatusForbidden, "该账号不是管理员，无法进入管理面板")
		return
	}
	p.createSession(w, out.Token, out.User.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"user": map[string]any{"name": out.User.Name, "is_admin": true},
	})
}

func (p *Panel) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	body, _ := json.Marshal(map[string]string{"name": strings.TrimSpace(in.Name), "pass": in.Pass})
	resp, err := p.callServer(http.MethodPost, "/api/auth/register", "", body)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "服务端未就绪："+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		writeErr(w, resp.StatusCode, serverMessage(raw, "注册失败"))
		return
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Token != "" {
		p.createSession(w, out.Token, in.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s := p.sessionOf(r); s != nil {
		req, _ := http.NewRequest(http.MethodPost, p.base()+"/api/auth/logout", nil)
		req.Header.Set("Authorization", "Bearer "+s.Token)
		if resp, err := proxyHTTP().Do(req); err == nil {
			_ = resp.Body.Close()
		}
		if c, err := r.Cookie("sp_session"); err == nil {
			p.mu.Lock()
			delete(p.sessions, c.Value)
			p.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: "sp_session", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) createSession(w http.ResponseWriter, token, name string) {
	sid := randomToken(32)
	p.mu.Lock()
	p.sessions[sid] = &session{Token: token, Name: name, IsAdmin: true, Expires: time.Now().Add(12 * time.Hour)}
	// 顺手清掉过期会话，避免长期运行越积越多。
	for k, v := range p.sessions {
		if time.Now().After(v.Expires) {
			delete(p.sessions, k)
		}
	}
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "sp_session", Value: sid, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600,
	})
}

// ---- 状态 ----

func (p *Panel) handleState(w http.ResponseWriter, r *http.Request) {
	sess := p.sessionOf(r)
	h := p.health()
	running, pid, uptime := p.running()
	p.mu.Lock()
	restarts := p.restarts
	lastErr := p.lastErr
	p.mu.Unlock()

	cfg := p.publicConfig(p.snapshotConfig())
	firstRun := false
	if h != nil {
		if v, ok := h["users"].(float64); ok && v == 0 {
			firstRun = true
		}
	}
	resp := map[string]any{
		"panel": map[string]any{
			"version":     Version,
			"url":         p.panelURL,
			"config_path": p.opt.ConfigPath,
			"data_dir":    p.cfg.DataDir,
			"cert_dir":    p.cfg.CertDir,
			"os":          runtime.GOOS,
			"arch":        runtime.GOARCH,
		},
		"server": map[string]any{
			"running":    running,
			"pid":        pid,
			"uptime":     uptime,
			"health":     h,
			"restarts":   restarts,
			"last_error": lastErr,
			"base_url":   p.base(),
		},
		"firstRun": firstRun,
		"session": map[string]any{
			"loggedIn": sess != nil,
			"name":     "",
		},
	}
	if sess != nil {
		resp["config"] = cfg
		// new map 而不是改原 map，避免登录态字段被后续写入影响。
		resp["session"] = map[string]any{"loggedIn": true, "name": sess.Name}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Panel) handleServerStart(w http.ResponseWriter, r *http.Request) {
	if err := p.StartServer(); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) handleServerStop(w http.ResponseWriter, r *http.Request) {
	if err := p.StopServer(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) handleServerRestart(w http.ResponseWriter, r *http.Request) {
	if err := p.RestartServer(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 日志 ----

func (p *Panel) handleLogs(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	path := p.serverLog
	if kind == "console" {
		path = p.consoleLog
	}
	lines := 300
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			lines = n
		}
	}
	b, err := tailFile(path, int64(lines)*512)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"text": "", "error": err.Error(), "path": path})
		return
	}
	text := string(b)
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"text": strings.Join(all, "\n"),
		"path": path,
		"size": fileSize(path),
	})
}

// ---- 配置 ----

func (p *Panel) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"config": p.publicConfig(p.snapshotConfig())})
}

func (p *Panel) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	base := p.snapshotConfig()
	next := base
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&next); err != nil {
		writeErr(w, http.StatusBadRequest, "配置格式不对："+err.Error())
		return
	}
	// 密钥字段留空表示“不修改”，避免前端回显后又被空值覆盖。
	if strings.TrimSpace(next.LiveSecret) == "" {
		next.LiveSecret = base.LiveSecret
	}
	if err := next.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.Save(p.opt.ConfigPath, next); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存配置失败："+err.Error())
		return
	}
	listenChanged := next.Addr != base.Addr || next.HTTPAddr != base.HTTPAddr ||
		next.WordbookAddr != base.WordbookAddr || next.InternalAddr != base.InternalAddr
	p.reloadConfig(next)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"restartRequired": listenChanged,
		"config":          p.publicConfig(next),
	})
}

// publicConfig 隐藏密钥，只告诉前端“是否已设置”。
func (p *Panel) publicConfig(c config.Config) map[string]any {
	b, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["live_secret"] = ""
	m["live_secret_set"] = strings.TrimSpace(c.LiveSecret) != ""
	return m
}

// ---- 证书 ----

func (p *Panel) handleCertRegenerate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ServerName string `json:"server_name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
	name := strings.TrimSpace(in.ServerName)
	if name == "" {
		name = p.snapshotConfig().ServerName
	}
	res, err := certgen.Generate(certgen.Options{Dir: p.cfg.CertDir, ServerName: name})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成证书失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "cert": res.CertFile, "key": res.KeyFile, "ca": res.CAFile,
		"restartRequired": true,
	})
}

func (p *Panel) handleCertUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "解析上传内容失败")
		return
	}
	certBytes, err1 := readPart(r, "cert")
	keyBytes, err2 := readPart(r, "key")
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, "需要同时上传证书与私钥")
		return
	}
	if _, err := tls.X509KeyPair(certBytes, keyBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "证书与私钥不匹配："+err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(p.cfg.CertDir, "server.crt"), certBytes, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(p.cfg.CertDir, "server.key"), keyBytes, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restartRequired": true})
}

// ---- 备份与恢复 ----

func (p *Panel) handleBackup(w http.ResponseWriter, r *http.Request) {
	addr, token, ok := p.internalAddr()
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "服务未运行，无法生成一致性备份")
		return
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/internal/backup", nil)
	req.Header.Set("X-Internal-Token", token)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "备份请求失败："+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		writeErr(w, resp.StatusCode, serverMessage(raw, "备份失败"))
		return
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	writeJSON(w, http.StatusOK, out)
}

func (p *Panel) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" || filepath.Base(name) != name || !strings.HasSuffix(name, ".db") {
		writeErr(w, http.StatusBadRequest, "备份文件名不合法")
		return
	}
	path := filepath.Join(p.cfg.DataDir, "backups", name)
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "备份不存在")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	_, _ = io.Copy(w, f)
}

func (p *Panel) handleRestore(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(512 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "上传文件过大或格式不对")
		return
	}
	file, _, err := r.FormFile("db")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请选择要恢复的 .db 文件")
		return
	}
	defer file.Close()

	tmp := filepath.Join(p.cfg.DataDir, "restore-upload.db")
	out, err := os.Create(tmp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := io.Copy(out, io.LimitReader(file, 512<<20)); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, "写入上传文件失败")
		return
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, "写入上传文件失败："+err.Error())
		return
	}
	_ = out.Close()

	// 只接受 SQLite 文件，避免把任意二进制覆盖到库上。
	head := make([]byte, 16)
	f, err := os.Open(tmp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = io.ReadFull(f, head)
	_ = f.Close()
	if string(head) != "SQLite format 3\x00" {
		_ = os.Remove(tmp)
		writeErr(w, http.StatusBadRequest, "这不是一个 SQLite 数据库文件")
		return
	}

	// ★ 先体检再动手。认不出结构的库直接拒绝 ——
	// 让它覆盖上去的结果是"服务起不来，而且原来的库已经改名了"。
	stats, err := inspectDB(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		writeErr(w, http.StatusBadRequest, "这个库读不出来："+err.Error())
		return
	}
	if !stats.Compatible {
		_ = os.Remove(tmp)
		writeErr(w, http.StatusBadRequest, stats.SchemaNote)
		return
	}

	// dry=1：只报体检结果，不换库。界面上"先看一眼"走的是这条路。
	if strings.TrimSpace(r.URL.Query().Get("dry")) == "1" {
		_ = os.Remove(tmp)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dry": true, "stats": stats})
		return
	}

	res, err := p.migrateFrom(tmp)
	_ = os.Remove(tmp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- 系统信息 ----

func (p *Panel) handleSystem(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	writeJSON(w, http.StatusOK, map[string]any{
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"go":         runtime.Version(),
		"cpus":       runtime.NumCPU(),
		"goroutines": runtime.NumGoroutine(),
		"mem_alloc":  mem.Alloc,
		"mem_sys":    mem.Sys,
		"data_size":  dirSize(p.cfg.DataDir),
		"data_dir":   p.cfg.DataDir,
		"time":       time.Now().Format(time.RFC3339),
	})
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// serverMessage 从服务端的错误响应里取一句可读的话。
func serverMessage(raw []byte, fallback string) string {
	var v struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &v) == nil {
		if v.Detail != "" {
			return v.Detail
		}
		if v.Error != "" {
			return v.Error
		}
	}
	if len(raw) > 0 && len(raw) < 300 {
		return strings.TrimSpace(string(raw))
	}
	return fallback
}

func readPart(r *http.Request, field string) ([]byte, error) {
	f, _, err := r.FormFile(field)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 4<<20))
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// tailFile 只读文件末尾 maxBytes，避免把几百 MB 日志读进内存。
func tailFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := int64(0)
	if st.Size() > maxBytes {
		off = st.Size() - maxBytes
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, nil
}

func dirSize(root string) int64 {
	var total int64
	count := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
			count++
		}
		if count > 200000 {
			return errors.New("too many files")
		}
		return nil
	})
	return total
}

func randomToken(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		// 极端情况下退化成时间噪声。会话仍然只在内存里，影响有限。
		for i := range raw {
			raw[i] = byte(time.Now().UnixNano() >> (uint(i%8) * 8))
		}
	}
	out := make([]byte, n)
	for i, v := range raw {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out)
}
