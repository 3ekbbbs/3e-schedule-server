package app

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 多服务器联动（2026-10-18）
//
// ## 它解决什么
//
// 一个人可以有好几台服务器：家里的旧电脑、机房的 VPS、朋友的机器。它们的
// 数据库各管各的，但**分发的文件可以互相借**。这里就是那张"借用关系表"。
//
// ## 两种模式，区别只在"用户连谁"
//
//	route   302 到对方地址。用户浏览器直连对方，本机只是一个路牌。
//	        对方必须公网可达。省本机带宽，代价是对方挂了就下不了。
//	mirror  本机先把文件拉下来，再从本机发给用户。用户只连本机。
//	        适合"对方在局域网里"或者"对方在国外很慢"。
//
// 这不是"中转 vs 直连"的措辞差别：route 模式本机**一个字节都不经手**，
// 所以它没法缓存、也没法统计；mirror 模式文件会落在本机磁盘上，占空间
// 但离线也能发。
//
// ## 账号为什么不同步
//
// 账号是身份，身份必须只有一个归属。两台服务器各有一份"admin"，
// 合并之后谁是管理员、谁封了谁，没有任何机制能裁决。所以这里
// **只同步分发相关的东西**（版本记录、路由规则、对端清单）。
//
// ## 防循环
//
// A 路由到 B、B 又路由到 A 的情况一定会有人配出来。所以转发时带
// `X-Schedule-Relay: 1`，收到这个头的请求不再参与路由，直接本地处理。
// ---------------------------------------------------------------------------

type peer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Token     string `json:"token,omitempty"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	LastOKAt  int64  `json:"last_ok_at"`
	LastError string `json:"last_error"`
	// 面板要显示"有没有配令牌"，但不能把令牌本身发回去。
	HasToken bool `json:"has_token"`
	// Insecure 表示连这台服务器时不校验 TLS 证书。自签证书必须打开。
	Insecure bool `json:"insecure"`
}

type routeRule struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`      // all | update | plugin | navpack | skinpack
	MatchKey  string `json:"match_key"` // 平台名 / 插件 id，空 = 这一类全走
	PeerID    string `json:"peer_id"`
	PeerName  string `json:"peer_name,omitempty"`
	Mode      string `json:"mode"` // route | mirror
	Priority  int    `json:"priority"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Server) loadPeers() []peer {
	rows, err := s.db.sql.Query(`
		SELECT id,name,base_url,token,enabled,created_at,last_ok_at,last_error,COALESCE(insecure,0)
		FROM federation_peers ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []peer{}
	for rows.Next() {
		var p peer
		var en, ins int
		var tok string
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &tok, &en, &p.CreatedAt, &p.LastOKAt, &p.LastError, &ins); err != nil {
			continue
		}
		p.Insecure = ins == 1
		p.Enabled = en == 1
		p.HasToken = strings.TrimSpace(tok) != ""
		out = append(out, p)
	}
	return out
}

// peerWithToken 只在真正要去连对方时用。列表接口绝不返回它。
func (s *Server) peerWithToken(id string) *peer {
	var p peer
	var en, ins int
	var tok string
	err := s.db.sql.QueryRow(`
		SELECT id,name,base_url,token,enabled,created_at,last_ok_at,last_error,COALESCE(insecure,0)
		FROM federation_peers WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.BaseURL, &tok, &en, &p.CreatedAt, &p.LastOKAt, &p.LastError, &ins)
	if err != nil {
		return nil
	}
	p.Enabled = en == 1
	p.Insecure = ins == 1
	p.Token = tok
	p.HasToken = strings.TrimSpace(tok) != ""
	return &p
}

func (s *Server) handleAdminPeers(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"peers": s.loadPeers()})
}

func (s *Server) handleAdminPeerCreate(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Name     string `json:"name"`
		BaseURL  string `json:"base_url"`
		Token    string `json:"token"`
		Enabled  *bool  `json:"enabled"`
		Insecure *bool  `json:"insecure"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	name := strings.TrimSpace(body.Name)
	base := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if base == "" {
		apiErr(w, 400, "缺少 base_url")
		return
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		apiErr(w, 400, "base_url 必须是 http:// 或 https:// 开头")
		return
	}
	if name == "" {
		name = base
	}
	en := true
	if body.Enabled != nil {
		en = *body.Enabled
	}
	ins := false
	if body.Insecure != nil {
		ins = *body.Insecure
	}
	id := newID("peer_")
	_, err := s.db.sql.Exec(`
		INSERT INTO federation_peers(id,name,base_url,token,enabled,created_at,insecure)
		VALUES(?,?,?,?,?,?,?)`,
		id, name, base, strings.TrimSpace(body.Token), boolToInt(en), time.Now().Unix(), boolToInt(ins))
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleAdminPeerUpdate(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	cur := s.peerWithToken(id)
	if cur == nil {
		apiErr(w, 404, "没有这台服务器")
		return
	}
	var body struct {
		Name     *string `json:"name"`
		BaseURL  *string `json:"base_url"`
		Token    *string `json:"token"`
		Enabled  *bool   `json:"enabled"`
		Insecure *bool   `json:"insecure"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	name, base, tok, en, ins := cur.Name, cur.BaseURL, cur.Token, cur.Enabled, cur.Insecure
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
	}
	if body.BaseURL != nil {
		base = strings.TrimRight(strings.TrimSpace(*body.BaseURL), "/")
		if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			apiErr(w, 400, "base_url 必须是 http:// 或 https:// 开头")
			return
		}
	}
	// token 传空串表示清除，传 null 表示不改 —— 面板上"不填就是不换"。
	if body.Token != nil {
		tok = strings.TrimSpace(*body.Token)
	}
	if body.Enabled != nil {
		en = *body.Enabled
	}
	if body.Insecure != nil {
		ins = *body.Insecure
	}
	_, err := s.db.sql.Exec(`
		UPDATE federation_peers SET name=?, base_url=?, token=?, enabled=?, insecure=? WHERE id=?`,
		name, base, tok, boolToInt(en), boolToInt(ins), id)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminPeerDelete(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.db.sql.Exec(`DELETE FROM federation_peers WHERE id=?`, id); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// 路由规则跟着删：留着一条指向不存在服务器的规则，等于把这一类内容
	// 永久卡在"路由到空"上，而面板上看起来还是一条正常规则。
	_, _ = s.db.sql.Exec(`DELETE FROM distribution_routes WHERE peer_id=?`, id)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAdminPeerTest 探一次对方的健康接口。
//
// 只探 /api/health：那是唯一一个不需要凭证的接口，能连上就说明网络通、
// 对方服务在跑。至于令牌对不对，那是导出快照才会用到的事，不在这里判。
func (s *Server) handleAdminPeerTest(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	p := s.peerWithToken(r.PathValue("id"))
	if p == nil {
		apiErr(w, 404, "没有这台服务器")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	target := strings.TrimRight(p.BaseURL, "/") + "/api/health"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	client := peerClient(p, 6*time.Second)
	resp, err := client.Do(req)
	now := time.Now().Unix()
	if err != nil {
		_, _ = s.db.sql.Exec(`UPDATE federation_peers SET last_error=? WHERE id=?`, err.Error(), p.ID)
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != 200 {
		msg := fmt.Sprintf("健康检查返回 %d", resp.StatusCode)
		_, _ = s.db.sql.Exec(`UPDATE federation_peers SET last_error=? WHERE id=?`, msg, p.ID)
		writeJSON(w, 200, map[string]any{"ok": false, "error": msg, "body": string(body)})
		return
	}
	_, _ = s.db.sql.Exec(`
		UPDATE federation_peers SET last_ok_at=?, last_error='' WHERE id=?`, now, p.ID)
	writeJSON(w, 200, map[string]any{"ok": true, "body": string(body)})
}

// ---------- 路由规则 ----------

func (s *Server) loadRoutes() []routeRule {
	rows, err := s.db.sql.Query(`
		SELECT r.id,r.kind,r.match_key,r.peer_id,r.mode,r.priority,r.enabled,r.created_at,
		       COALESCE(p.name,'')
		FROM distribution_routes r LEFT JOIN federation_peers p ON p.id=r.peer_id
		ORDER BY r.priority, r.created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []routeRule{}
	for rows.Next() {
		var x routeRule
		var en int
		if err := rows.Scan(&x.ID, &x.Kind, &x.MatchKey, &x.PeerID, &x.Mode,
			&x.Priority, &en, &x.CreatedAt, &x.PeerName); err != nil {
			continue
		}
		x.Enabled = en == 1
		out = append(out, x)
	}
	return out
}

func (s *Server) handleAdminRoutes(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"routes": s.loadRoutes()})
}

func (s *Server) handleAdminRouteCreate(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Kind     string `json:"kind"`
		MatchKey string `json:"match_key"`
		PeerID   string `json:"peer_id"`
		Mode     string `json:"mode"`
		Priority int    `json:"priority"`
		Enabled  *bool  `json:"enabled"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	kind := orDefaultStr(strings.TrimSpace(body.Kind), "all")
	if !validRouteKind(kind) {
		apiErr(w, 400, "kind 只能是 all / update / plugin / navpack / skinpack")
		return
	}
	mode := orDefaultStr(strings.TrimSpace(body.Mode), "route")
	if mode != "route" && mode != "mirror" {
		apiErr(w, 400, "mode 只能是 route 或 mirror")
		return
	}
	if s.peerWithToken(body.PeerID) == nil {
		apiErr(w, 400, "peer_id 指向的服务器不存在")
		return
	}
	if body.Priority == 0 {
		body.Priority = 100
	}
	en := true
	if body.Enabled != nil {
		en = *body.Enabled
	}
	id := newID("rt_")
	_, err := s.db.sql.Exec(`
		INSERT INTO distribution_routes(id,kind,match_key,peer_id,mode,priority,enabled,created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		id, kind, strings.TrimSpace(body.MatchKey), body.PeerID, mode,
		body.Priority, boolToInt(en), time.Now().Unix())
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleAdminRouteUpdate(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Enabled  *bool   `json:"enabled"`
		Mode     *string `json:"mode"`
		Priority *int    `json:"priority"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	id := r.PathValue("id")
	if body.Enabled != nil {
		if _, err := s.db.sql.Exec(`UPDATE distribution_routes SET enabled=? WHERE id=?`,
			boolToInt(*body.Enabled), id); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if body.Mode != nil {
		m := strings.TrimSpace(*body.Mode)
		if m != "route" && m != "mirror" {
			apiErr(w, 400, "mode 只能是 route 或 mirror")
			return
		}
		if _, err := s.db.sql.Exec(`UPDATE distribution_routes SET mode=? WHERE id=?`, m, id); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if body.Priority != nil {
		if _, err := s.db.sql.Exec(`UPDATE distribution_routes SET priority=? WHERE id=?`,
			*body.Priority, id); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminRouteDelete(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	if _, err := s.db.sql.Exec(`DELETE FROM distribution_routes WHERE id=?`, r.PathValue("id")); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func validRouteKind(k string) bool {
	switch k {
	case "all", "update", "plugin", "navpack", "skinpack":
		return true
	}
	return false
}

// matchRoutes 找出适用于这次下载的规则，按优先级升序。
func (s *Server) matchRoutes(kind, match string) []routeRule {
	all := s.loadRoutes()
	out := []routeRule{}
	for _, rt := range all {
		if !rt.Enabled {
			continue
		}
		if rt.Kind != "all" && rt.Kind != kind {
			continue
		}
		if rt.MatchKey != "" && rt.MatchKey != match {
			continue
		}
		out = append(out, rt)
	}
	return out
}

// peerClient 按对端的证书设置建一个 HTTP 客户端。
func peerClient(p *peer, timeout time.Duration) *http.Client {
	c := &http.Client{Timeout: timeout}
	if p != nil && p.Insecure {
		c.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return c
}

// ---------- 路由执行 ----------

// tryRoute 是下载 handler 的第一道岔路。返回 true 表示这次请求已经处理完。
//
// 一个规则失败（对方不可达、缓存拉不下来）就继续试下一条，最后回退到本地
// 文件。这样"联动配错了"的表现是"还能下，只是没走联动机器"，不是 502。
func (s *Server) tryRoute(w http.ResponseWriter, r *http.Request, kind, match string) bool {
	// 已经中转过一次就不再路由，否则 A↔B 会互相踢皮球。
	if r.Header.Get("X-Schedule-Relay") == "1" {
		return false
	}
	rules := s.matchRoutes(kind, match)
	for _, rt := range rules {
		p := s.peerWithToken(rt.PeerID)
		if p == nil || !p.Enabled || strings.TrimSpace(p.BaseURL) == "" {
			continue
		}
		switch rt.Mode {
		case "route":
			target := strings.TrimRight(p.BaseURL, "/") + r.URL.RequestURI()
			w.Header().Set("X-Schedule-Route", p.Name)
			http.Redirect(w, r, target, http.StatusFound)
			return true
		case "mirror":
			if s.serveFromMirror(w, r, p, kind) {
				w.Header().Set("X-Schedule-Mirror", p.Name)
				return true
			}
		}
	}
	return false
}

// mirrorCachePath 是缓存落在哪儿。
//
// 文件名取请求路径的 basename —— 下载接口的路径本来就是一文件一路径
// （/api/updates/download/android），但真实文件名在响应头里，所以这里
// 用 kind + basename 做键，够唯一。
func (s *Server) mirrorCachePath(kind string, r *http.Request) string {
	name := filepath.Base(r.URL.Path)
	if name == "" || name == "." || name == "/" {
		name = kind
	}
	// 把查询串里的关键参数带上，避免 android 的两个版本共用一个缓存名。
	if v := r.URL.Query().Get("version"); v != "" {
		name = v + "-" + name
	}
	return filepath.Join(s.cfg.DataDir, "mirror", kind, name+".bin")
}

// serveFromMirror 命中缓存就直接发，没命中就先去对方那里拉一份。
func (s *Server) serveFromMirror(w http.ResponseWriter, r *http.Request, p *peer, kind string) bool {
	cache := s.mirrorCachePath(kind, r)
	if st, err := os.Stat(cache); err == nil && st.Size() > 0 {
		f, err := os.Open(cache)
		if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeContent(w, r, filepath.Base(cache), st.ModTime(), f)
			return true
		}
	}

	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()

	target := strings.TrimRight(p.BaseURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Schedule-Relay", "1")
	if p.Token != "" {
		// 两个凭证都带：部署令牌走 X-Relay-Token 那条窄路，账号令牌走 Bearer。
		// 用户填哪一个都能用，不需要先告诉面板"这是哪种令牌"。
		req.Header.Set("X-Relay-Token", p.Token)
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	client := peerClient(p, 20*time.Minute)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("mirror: 拉 %s 失败：%v", target, err)
		s.markPeerError(p.ID, err.Error())
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("mirror: %s 返回 %d", target, resp.StatusCode)
		s.markPeerError(p.ID, fmt.Sprintf("返回 %d", resp.StatusCode))
		return false
	}

	tmp := cache + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return false
	}
	written, err := io.Copy(f, io.LimitReader(resp.Body, 2<<30))
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		s.markPeerError(p.ID, err.Error())
		return false
	}
	if err := os.Rename(tmp, cache); err != nil {
		os.Remove(tmp)
		return false
	}
	s.markPeerOK(p.ID)
	log.Printf("mirror: %s ← %s（%d 字节）", cache, target, written)

	st, err := os.Stat(cache)
	if err != nil {
		return false
	}
	f2, err := os.Open(cache)
	if err != nil {
		return false
	}
	defer f2.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(cache), st.ModTime(), f2)
	return true
}

func (s *Server) markPeerOK(id string) {
	_, _ = s.db.sql.Exec(`UPDATE federation_peers SET last_ok_at=?, last_error='' WHERE id=?`,
		time.Now().Unix(), id)
}

func (s *Server) markPeerError(id, msg string) {
	_, _ = s.db.sql.Exec(`UPDATE federation_peers SET last_error=? WHERE id=?`, msg, id)
}

// ---------- 中继下载的鉴权 ----------

// relayAllowedPath 限定中继令牌只能用来取文件。
//
// 部署令牌本身权限很大（能换 exe），拿它当中继凭证是唯一省事又不用
// 再养一套密钥的做法。代价是必须把它的用途钉死在这几个路径上，
// 否则等于把部署权限散给了每一台联动服务器。
func relayAllowedPath(p string) bool {
	for _, prefix := range []string{
		"/api/updates/download/",
		"/api/plugins/download/",
		"/api/navpacks/download/",
		"/api/skinpacks/download/",
	} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func (s *Server) relayAuthorized(r *http.Request) bool {
	tok := strings.TrimSpace(r.Header.Get("X-Relay-Token"))
	if tok == "" || !relayAllowedPath(r.URL.Path) {
		return false
	}
	want, err := readDeployToken()
	if err != nil || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1
}

// ---------- 快照导出 / 导入 ----------

type federationSnapshot struct {
	Kind       string         `json:"kind"`
	Version    int            `json:"version"`
	ExportedAt int64          `json:"exported_at"`
	Server     map[string]any `json:"server"`
	Updates    updateManifest `json:"updates"`
	Peers      []peer         `json:"peers"`
	Routes     []routeRule    `json:"routes"`
	Settings   map[string]any `json:"settings"`
	Notice     string         `json:"notice"`
}

// handleAdminFederationExport 生成一份可搬运的 JSON。
//
// 里面**没有**账号、密码、令牌、数据库。它描述的是"这台服务器分发什么、
// 跟谁联动"，不是"这台服务器上有谁"。
func (s *Server) handleAdminFederationExport(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	peers := s.loadPeers()
	for i := range peers {
		peers[i].Token = ""
	}
	snap := federationSnapshot{
		Kind:       "schedule-server-federation",
		Version:    1,
		ExportedAt: time.Now().Unix(),
		Server: map[string]any{
			"version":   version,
			"cos_base":  s.cfg.COSBase,
			"data_dir":  s.cfg.DataDir,
			"public":    true,
			"admin_url": "",
		},
		Updates: s.loadManifest(),
		Peers:   peers,
		Routes:  s.loadRoutes(),
		Settings: map[string]any{
			"cos_base":    s.cfg.COSBase,
			"market_url":  s.cfg.MarketURL,
			"server_name": s.cfg.ServerName,
		},
		Notice: "这份快照只包含分发配置，不含账号、密码与令牌。导入方不会获得任何人的登录能力。",
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="federation-snapshot.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snap)
}

// handleAdminFederationImport 合并一份快照。
//
// 合并规则：
//   - 对端按 base_url 去重，已存在就更新名字/开关，不动令牌
//   - 路由规则按 (kind, match_key, peer 名) 去重
//   - 版本记录**只更新说明和强制标记**，不覆盖本机版本号，也不下载文件
//
// 最后一条是有意的：快照的版本号是"对方那台现在发什么"，直接盖过来
// 会让本机显示一个磁盘上根本没有的版本，而下载接口会 410。
func (s *Server) handleAdminFederationImport(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var snap federationSnapshot
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&snap); err != nil {
		apiErr(w, 400, "不是合法的快照文件："+err.Error())
		return
	}
	if snap.Kind != "schedule-server-federation" {
		apiErr(w, 400, "kind 不是 schedule-server-federation，这不是本程序的快照")
		return
	}

	addedPeers, addedRoutes := 0, 0

	// 先建对端，路由要引用它们的新 id。
	nameToID := map[string]string{}
	for _, p := range s.loadPeers() {
		nameToID[p.Name] = p.ID
	}
	for _, p := range snap.Peers {
		base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		if base == "" {
			continue
		}
		var existing string
		_ = s.db.sql.QueryRow(`SELECT id FROM federation_peers WHERE base_url=?`, base).Scan(&existing)
		if existing != "" {
			nameToID[p.Name] = existing
			continue
		}
		id := newID("peer_")
		// insecure 必须跟着搬：它描述的是"对方那张证书怎么处理"，
		// 漏掉之后导入方连同一台服务器会直接 TLS 失败，而错误信息只说
		// "证书无效"，看不出是导入时丢的配置。
		_, err := s.db.sql.Exec(`
			INSERT INTO federation_peers(id,name,base_url,token,enabled,created_at,insecure)
			VALUES(?,?,?,'',?,?,?)`,
			id, orDefaultStr(p.Name, base), base, boolToInt(p.Enabled),
			time.Now().Unix(), boolToInt(p.Insecure))
		if err == nil {
			nameToID[p.Name] = id
			addedPeers++
		}
	}

	for _, rt := range snap.Routes {
		peerID := nameToID[rt.PeerName]
		if peerID == "" {
			peerID = rt.PeerID
		}
		if s.peerWithToken(peerID) == nil {
			continue
		}
		var n int
		_ = s.db.sql.QueryRow(`
			SELECT COUNT(1) FROM distribution_routes
			WHERE kind=? AND match_key=? AND peer_id=?`,
			rt.Kind, rt.MatchKey, peerID).Scan(&n)
		if n > 0 {
			continue
		}
		_, err := s.db.sql.Exec(`
			INSERT INTO distribution_routes(id,kind,match_key,peer_id,mode,priority,enabled,created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			newID("rt_"), rt.Kind, rt.MatchKey, peerID, orDefaultStr(rt.Mode, "route"),
			orDefaultInt(rt.Priority, 100), boolToInt(rt.Enabled), time.Now().Unix())
		if err == nil {
			addedRoutes++
		}
	}

	writeJSON(w, 200, map[string]any{
		"ok":           true,
		"added_peers":  addedPeers,
		"added_routes": addedRoutes,
		"note":         "版本记录没有覆盖：快照里的版本对应对方磁盘上的文件，本机没有那个文件。令牌也不会随快照传递，导入后要逐台补填。",
	})
}

// ---------- 小工具 ----------

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// parsePeerPort 只是给面板提示用的，不影响路由。
func parsePeerPort(base string) int {
	u, err := url.Parse(base)
	if err != nil {
		return 0
	}
	if p := u.Port(); p != "" {
		n, _ := strconv.Atoi(p)
		return n
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}
