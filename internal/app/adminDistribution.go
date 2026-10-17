package app

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 分发管理（2026-10-18）
//
// 这一层解决的是"服务器上那个包现在是什么状态"，而不是"怎么上传一个包"。
// 上传还是走 /api/updates/publish（发布脚本用），这里管的是发布之后的事：
//
//	停用      包还在，别人下不到。用于"这个版本有问题，先别发了"
//	改说明    版本号不动，只改发布备注和是否强制更新
//	删除      连文件一起删
//	克隆      从别的站点（官网）把安装包拉过来，变成自己的
//
// 克隆为什么要有：开源版是给别人自己部署的，用户在官网看到一个新版本，
// 想让自己的服务器也分发同一个包，如果只能"下载到本地再上传"，中间那次
// 下载会落到用户自己的电脑上，多一个环节就多一次出错的机会。
// ---------------------------------------------------------------------------

// artifactView 是面板看到的一条记录。比 updateArtifact 多三个"现场状态"字段。
type artifactView struct {
	updateArtifact
	FileExists  bool   `json:"file_exists"`
	FileSize    int64  `json:"file_size"`
	DownloadURL string `json:"download_url"`
}

func (s *Server) artifactViewFor(platform string, art *updateArtifact) *artifactView {
	if art == nil {
		return nil
	}
	v := &artifactView{updateArtifact: *art}
	if art.File != "" {
		full := filepath.Join(s.updatesDir(), filepath.Base(art.File))
		if st, err := os.Stat(full); err == nil {
			v.FileExists = true
			v.FileSize = st.Size()
		}
	}
	v.DownloadURL = s.downloadURLFor(platform, art)
	return v
}

// handleAdminUpdateList 返回两个平台的完整状态。
//
// 与 /api/updates/manifest 的区别：那个是给发布脚本看的，返回目录和杂项文件；
// 这个是给面板看的，只关心"这个平台有没有包、包在不在、发不发"。
func (s *Server) handleAdminUpdateList(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	m := s.loadManifest()
	writeJSON(w, 200, map[string]any{
		"android":     s.artifactViewFor("android", m.Android),
		"windows":     s.artifactViewFor("windows", m.Windows),
		"cos_base":    s.cfg.COSBase,
		"updates_dir": s.updatesDir(),
	})
}

// handleAdminUpdateToggle 启停某个平台的分发。
//
//	POST /api/admin/updates/{platform}/toggle  {"enabled": false}
func (s *Server) handleAdminUpdateToggle(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	platform := strings.ToLower(r.PathValue("platform"))
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSON(r, &body); err != nil || body.Enabled == nil {
		apiErr(w, 400, "需要 enabled 字段（true 或 false）")
		return
	}
	m := s.loadManifest()
	art := pickArtifact(&m, platform)
	if art == nil {
		apiErr(w, 404, "这个平台还没有发布过任何包")
		return
	}
	v := *body.Enabled
	art.Enabled = &v
	if err := s.saveManifest(m); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	log.Printf("update toggle: %s enabled=%v", platform, v)
	writeJSON(w, 200, map[string]any{"ok": true, "artifact": s.artifactViewFor(platform, art)})
}

// handleAdminUpdateEdit 改版本说明、强制更新开关、版本号。
//
//	PUT /api/admin/updates/{platform}
//	{"version":"1.2.4","version_code":124,"notes":"...","mandatory":true}
//
// 版本号可以改是因为"克隆来的包文件名里带版本，但版本可能解析错"，
// 让人能在面板里直接改掉，比重新发布一次省事。
func (s *Server) handleAdminUpdateEdit(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	platform := strings.ToLower(r.PathValue("platform"))
	var body struct {
		Version     *string `json:"version"`
		VersionCode *int64  `json:"version_code"`
		Notes       *string `json:"notes"`
		Mandatory   *bool   `json:"mandatory"`
		Series      *string `json:"series"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	m := s.loadManifest()
	art := pickArtifact(&m, platform)
	if art == nil {
		apiErr(w, 404, "这个平台还没有发布过任何包")
		return
	}
	if body.Version != nil {
		v := strings.TrimSpace(*body.Version)
		if v == "" {
			apiErr(w, 400, "版本号不能为空")
			return
		}
		art.Version = v
	}
	if body.VersionCode != nil {
		if platform == "android" && *body.VersionCode <= 0 {
			apiErr(w, 400, "android 的 version_code 必须是正整数")
			return
		}
		art.VersionCode = *body.VersionCode
	}
	if body.Notes != nil {
		art.Notes = *body.Notes
	}
	if body.Mandatory != nil {
		art.Mandatory = *body.Mandatory
	}
	if body.Series != nil {
		art.Series = orDefaultStr(*body.Series, "stable")
	}
	if err := s.saveManifest(m); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "artifact": s.artifactViewFor(platform, art)})
}

// handleAdminUpdateDelete 删掉某个平台的分发记录，附带删文件。
//
//	DELETE /api/admin/updates/{platform}?keep_file=1
//
// keep_file=1 只清记录不删文件 —— 用于"这条记录配错了，重发一遍"，
// 而文件还在磁盘上、可以直接复用。
func (s *Server) handleAdminUpdateDelete(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	platform := strings.ToLower(r.PathValue("platform"))
	m := s.loadManifest()
	art := pickArtifact(&m, platform)
	if art == nil {
		apiErr(w, 404, "这个平台还没有发布过任何包")
		return
	}
	if r.URL.Query().Get("keep_file") != "1" && art.File != "" {
		full := filepath.Join(s.updatesDir(), filepath.Base(art.File))
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			apiErr(w, 500, "删除文件失败："+err.Error())
			return
		}
	}
	setArtifact(&m, platform, nil)
	if err := s.saveManifest(m); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	log.Printf("update deleted: %s", platform)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func pickArtifact(m *updateManifest, platform string) *updateArtifact {
	switch platform {
	case "android":
		return m.Android
	case "windows":
		return m.Windows
	}
	return nil
}

func setArtifact(m *updateManifest, platform string, art *updateArtifact) {
	switch platform {
	case "android":
		m.Android = art
	case "windows":
		m.Windows = art
	}
}

// ---------- 从别的站点克隆安装包 ----------

// 官网 /api/downloads 返回的形状（只取这里要用的字段）。
type remoteDownload struct {
	Platform string `json:"platform"`
	Label    string `json:"label"`
	Web      bool   `json:"web"`
	Ready    bool   `json:"ready"`
	File     string `json:"file"`
	Href     string `json:"href"`
	Fallback string `json:"fallback"`
	Size     int64  `json:"size"`
}

type remoteDownloads struct {
	OK        bool             `json:"ok"`
	Downloads []remoteDownload `json:"downloads"`
}

var versionFromName = regexp.MustCompile(`\d+\.\d+(?:\.\d+)*`)

// clonePick 把两种源站格式收敛成一个形状。
type clonePick struct {
	File    string
	Href    string
	Version string
	SHA256  string
	Size    int64
}

// fetchCloneSource 依次尝试官网格式与服务端格式。
func fetchCloneSource(ctx context.Context, client *http.Client, source, platform, wantFile, token string) (*clonePick, error) {
	if p, err := fetchSiteDownloads(ctx, client, source, platform, wantFile, token); err == nil && p != nil {
		return p, nil
	}
	return fetchServerPublic(ctx, client, source, platform, token)
}

// setCloneAuth 同时带两种凭证。
//
// 对方是官网时它只用得上 Bearer（官网读主服务的 manifest，不校验）；
// 对方是本程序时，X-Relay-Token 走的是"只放行四个下载路径"的那条窄路，
// Bearer 走的是正常账号令牌。两个都带，调用方就不用先判断对方是谁。
func setCloneAuth(req *http.Request, token string) {
	if token == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Relay-Token", token)
}

func getJSON(ctx context.Context, client *http.Client, url string, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "schedule-server/clone")
	setCloneAuth(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("连不上 %s：%w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s 返回 %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// fetchSiteDownloads 读官网 /api/downloads。
func fetchSiteDownloads(ctx context.Context, client *http.Client, source, platform, wantFile, token string) (*clonePick, error) {
	var rl remoteDownloads
	if err := getJSON(ctx, client, strings.TrimRight(source, "/")+"/api/downloads", token, &rl); err != nil {
		return nil, err
	}
	if !rl.OK && len(rl.Downloads) == 0 {
		return nil, fmt.Errorf("源站没有返回下载清单")
	}
	for _, d := range rl.Downloads {
		if d.Platform != platform || d.Web {
			continue
		}
		if wantFile != "" && d.File != wantFile {
			continue
		}
		href := d.Href
		if strings.TrimSpace(href) == "" {
			href = d.Fallback
		}
		if !d.Ready && href == "" {
			continue
		}
		return &clonePick{File: d.File, Href: href, Size: d.Size}, nil
	}
	return nil, fmt.Errorf("源站没有 %s 可下载的安装包", platform)
}

// fetchServerPublic 读另一台本程序的 /api/updates/public。
func fetchServerPublic(ctx context.Context, client *http.Client, source, platform, token string) (*clonePick, error) {
	var pub struct {
		Android *struct {
			Version     string `json:"version"`
			File        string `json:"file"`
			SHA256      string `json:"sha256"`
			Size        int64  `json:"size"`
			DownloadURL string `json:"download_url"`
		} `json:"android"`
		Windows *struct {
			Version     string `json:"version"`
			File        string `json:"file"`
			SHA256      string `json:"sha256"`
			Size        int64  `json:"size"`
			DownloadURL string `json:"download_url"`
		} `json:"windows"`
	}
	if err := getJSON(ctx, client, strings.TrimRight(source, "/")+"/api/updates/public", token, &pub); err != nil {
		return nil, fmt.Errorf("源站既不认 /api/downloads 也不认 /api/updates/public：%w", err)
	}
	if platform == "android" && pub.Android != nil {
		return &clonePick{
			File: pub.Android.File, Href: pub.Android.DownloadURL,
			Version: pub.Android.Version, SHA256: pub.Android.SHA256, Size: pub.Android.Size,
		}, nil
	}
	if platform == "windows" && pub.Windows != nil {
		return &clonePick{
			File: pub.Windows.File, Href: pub.Windows.DownloadURL,
			Version: pub.Windows.Version, SHA256: pub.Windows.SHA256, Size: pub.Windows.Size,
		}, nil
	}
	return nil, fmt.Errorf("源站没有发布 %s 的安装包", platform)
}

// guessVersion 从文件名里抠一个版本号。抠不到就返回空串，交给调用方要求用户填。
func guessVersion(name string) string {
	if m := versionFromName.FindString(filepath.Base(name)); m != "" {
		return m
	}
	return ""
}

// resolveRemoteURL 把官网给的 href 变成绝对地址。
//
// 官网在没有 COS 的时候 href 是 "/dl/android" 这种相对路径，直接用会拼到
// 本机上（变成"自己下载自己"），必须相对源站解析。
func resolveRemoteURL(base, href string) (string, error) {
	b, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil {
		return "", err
	}
	h, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	abs := b.ResolveReference(h)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return "", fmt.Errorf("只支持 http/https 地址")
	}
	return abs.String(), nil
}

// handleAdminUpdateClone 从另一个站点（通常是官网）拉一个安装包过来。
//
//	POST /api/admin/updates/clone
//	{"source_url":"https://example.com","platform":"android",
//	 "version":"1.2.3","version_code":123,"file":"","notes":"从官网克隆"}
//
// file 留空表示用源站给的那个（推荐）。version 留空会从文件名里猜。
func (s *Server) handleAdminUpdateClone(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var body struct {
		SourceURL   string `json:"source_url"`
		Platform    string `json:"platform"`
		File        string `json:"file"`
		Version     string `json:"version"`
		VersionCode int64  `json:"version_code"`
		Notes       string `json:"notes"`
		// Token 是源站的部署令牌或账号令牌。
		//
		// 源站的下载接口要凭证（/api/updates/download 走 ext），而公网官网的
		// /dl/ 是匿名的。所以这个字段可以留空 —— 留空能克隆官网，克隆另一台
		// 本程序时就必须填。
		Token string `json:"token"`
		// Insecure 跳过源站的证书校验。
		//
		// 必须有这个开关：本程序默认发的是自签证书，两台自己人的服务器之间
		// 克隆时，对方的证书一定不在本机信任链里。默认关着，是因为它对
		// 公网站点意味着"任何人都能冒充"。
		Insecure bool `json:"insecure"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	source := strings.TrimSpace(body.SourceURL)
	if source == "" {
		apiErr(w, 400, "缺少 source_url")
		return
	}
	if u, err := url.Parse(source); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		apiErr(w, 400, "source_url 必须是 http:// 或 https:// 开头")
		return
	}
	platform := strings.ToLower(strings.TrimSpace(body.Platform))
	if platform != "android" && platform != "windows" {
		apiErr(w, 400, "platform 只能是 android 或 windows")
		return
	}

	client := &http.Client{Timeout: 30 * time.Minute}
	if body.Insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	// 1) 问源站要清单。两种站点都认：
	//    官网     /api/downloads（给人点的下载页，字段是 file/href/size）
	//    另一台本程序 /api/updates/public（字段是 version/file/sha256/download_url）
	//    先试官网格式，不成立再试服务端格式 —— 两者都是 200 + JSON，
	//    只能靠字段形状区分，所以顺序是先具体后一般。
	pick, err := fetchCloneSource(ctx, client, source, platform, strings.TrimSpace(body.File), strings.TrimSpace(body.Token))
	if err != nil {
		hint := ""
		if !body.Insecure && strings.Contains(err.Error(), "certificate") {
			hint = "（对方用的是自签证书。确认地址没问题的话，勾选「跳过证书校验」再试一次。）"
		}
		apiErr(w, 502, err.Error()+hint)
		return
	}

	// 2) 决定真正从哪儿拉
	href := pick.Href
	if strings.TrimSpace(href) == "" {
		apiErr(w, 404, "源站这一项没有给出下载地址")
		return
	}
	dlURL, err := resolveRemoteURL(source, href)
	if err != nil {
		apiErr(w, 400, "下载地址不合法："+err.Error())
		return
	}

	// 3) 文件名与版本
	name := filepath.Base(pick.File)
	if name == "" || name == "." || name == "/" {
		name = platform + "-" + time.Now().Format("20060102") + ".bin"
		if platform == "android" {
			name = strings.TrimSuffix(name, ".bin") + ".apk"
		}
	}
	// 加个前缀避免和本机已有文件同名互相覆盖（克隆来的包不该悄悄顶掉本地的）。
	if !strings.HasPrefix(name, "clone-") {
		name = "clone-" + name
	}
	vers := strings.TrimSpace(body.Version)
	if vers == "" {
		vers = strings.TrimSpace(pick.Version)
	}
	if vers == "" {
		vers = guessVersion(pick.File)
	}
	if vers == "" {
		vers = guessVersion(name)
	}
	if vers == "" {
		apiErr(w, 400, "源站没有给出版本号，文件名里也看不出来。请在 version 里填一个。")
		return
	}
	code := body.VersionCode
	if platform == "android" && code <= 0 {
		apiErr(w, 400, "android 包必须填 version_code（整数，只能比上一版大）")
		return
	}

	// 4) 下载
	if err := os.MkdirAll(s.updatesDir(), 0o755); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	dst := filepath.Join(s.updatesDir(), name)
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	dreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	dreq.Header.Set("User-Agent", "schedule-server/clone")
	setCloneAuth(dreq, strings.TrimSpace(body.Token))
	dresp, err := client.Do(dreq)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		apiErr(w, 502, "下载失败："+err.Error())
		return
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != 200 {
		f.Close()
		os.Remove(tmp)
		apiErr(w, 502, fmt.Sprintf("下载地址返回 %d", dresp.StatusCode))
		return
	}

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(dresp.Body, 2<<30))
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		apiErr(w, 500, "写入失败："+err.Error())
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	// 源站给了哈希就一定要核。下载中断、CDN 返回了错误页、中间人换包，
	// 这三种情况的共同表现都是"文件长度不对或内容不对"，而客户端拿到之后
	// 只会报一句校验失败，根本查不到这一层。
	if pick.SHA256 != "" && !strings.EqualFold(sum, pick.SHA256) {
		os.Remove(tmp)
		apiErr(w, 502, "下载到的文件和源站给的 sha256 不一致，可能被截断或篡改")
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		apiErr(w, 500, err.Error())
		return
	}

	// 5) 记进 manifest
	enabled := true
	art := updateArtifact{
		Platform:    platform,
		Version:     vers,
		VersionCode: code,
		File:        name,
		Series:      "stable",
		SHA256:      sum,
		Size:        written,
		Notes:       orDefaultStr(strings.TrimSpace(body.Notes), "从 "+source+" 克隆"),
		PublishedAt: time.Now().Unix(),
		Enabled:     &enabled,
		Source:      "clone",
	}
	m := s.loadManifest()
	setArtifact(&m, platform, &art)
	if err := s.saveManifest(m); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	log.Printf("update cloned: %s %s from %s (%d bytes)", platform, vers, dlURL, written)
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"artifact": s.artifactViewFor(platform, &art),
		"from":     dlURL,
	})
}
