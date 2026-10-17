package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 插件市场：提交 → 审核 → 公示（2026-10-15）
//
// ## 为什么要这一层
//
// `plugins.go` 的 `handlePluginPublish` 是"**我们自己的出包脚本**直发" —— 管理员、
// 立即生效、没有中间态。那对"我们自己出功能"是对的（发布脚本是可信的），
// 但**用户上传**是另一回事：一个来路不明的 apk 直接进商店，等于我们把
// 别人的代码分发给所有用户。所以必须有一条**待审队列**。
//
// ## 三个状态，两条流水线
//
//	                 POST /api/plugins/submit
//	   用户 ──────────────────────────────────► pending（待审，只对提交者和管理员可见）
//	                                                 │
//	                      管理员 approve ───────────┼───────────► published（对外）
//	                      管理员 reject  ────────────┘
//	                                                 └───────────► rejected（留理由，提交者可见）
//
//	                 POST /api/plugins/publish
//	   我们自己的出包脚本 ────────────────────────► published（跳过队列）
//
// ## ★ 数据放在哪：**不并进 manifest.json**
//
// 已发布的那份索引是**客户端契约**（`pluginArtifact` 的 json tag 就是客户端读的键）。
// 把"待审/被驳回"的行塞进去，会让客户端收到一堆它不该看见的东西 ——
// 而且**将来某个客户端版本一旦开始过滤，另一批就会当成 bug**。
//
// 所以：已发布 = `data/plugins/manifest.json`（原样），
//       待审   = `data/plugins/submissions.json` + `data/plugins/pending/*`。
// 两份文件、两套语义，互不污染。
//
// ## ★ 平台维度
//
// 手机端是 apk（清单在 `assets/threees-plugin.json`），电脑端是 zip
// （清单在包根 `threees-plugin.json`）。**同一个 id 可能在两个平台上各有一份**
// （`notes` / `smartnotes` 就是），所以索引的键从 `<id>` 改成了 `<platform>:<id>`。
// 老索引里没有冒号的键在读的时候按 `android:` 迁进来（幂等）。
// ---------------------------------------------------------------------------

const (
	pluginPlatformAndroid = "android"
	pluginPlatformWindows = "windows"

	pluginStatusPending   = "pending"
	pluginStatusRejected  = "rejected"
	pluginStatusPublished = "published"

	// 上传上限。apk 插件目前最大的（日历）约 400 KB，zip 更小；
	// 32 MB 是"远远够用、但不至于让一次误操作把磁盘塞满"的位置。
	pluginUploadMaxBytes = 32 << 20
)

// pluginSubmission 是待审队列里的一条。
//
// ⚠️ 内嵌 `pluginArtifact` 而不是复制一遍字段：清单内容只有一个来源
//    （包内 `threees-plugin.json`），复制就是让它有第二份。
//
// ⚠️⚠️ 必须是**值内嵌**，不能写成 `*pluginArtifact`（指针内嵌）。
//
//    Go 的 `encoding/json` **不能把 JSON 反序列化进"指向未导出结构体的内嵌指针"**：
//
//        json: cannot set embedded pointer to unexported struct: main.pluginArtifact
//
//    而 `loadSubmissions()` 是把错误 `_ =` 掉的（"文件坏了当空队列"），
//    于是**整个内嵌结构静默丢失** —— 落盘的文件是完整的（Marshal 没问题），
//    但读回来只剩外层那几个字段。现象是"待审列表里每条都没有名字和版本"，
//    而文件、日志、HTTP 状态码**全部正常**。
//    （2026-10-15 冒烟时踩到：落盘的 submissions.json 里 `id`/`name`/`version` 都在，
//     接口却返回 `{"status":"pending","submitter_name":"smoke"}`。）
//
//    值内嵌两个方向都正常，所以这里用值。
type pluginSubmission struct {
	pluginArtifact
	// ⚠️ 也**不要**在这里再写一个 `Platform string`（JSON 名会撞 `pluginArtifact.Platform`）。
	//    平台本来就在内嵌结构里，一份就够。
	Status        string `json:"status"`
	SubmitterID   string `json:"submitter_id"`
	SubmitterName string `json:"submitter_name"`
	SubmittedAt   int64  `json:"submitted_at"`
	ReviewNote    string `json:"review_note"`
	ReviewerName  string `json:"reviewer_name"`
	ReviewedAt    int64  `json:"reviewed_at"`
}

type pluginSubmissionIndex struct {
	Items map[string]*pluginSubmission `json:"items"`
}

// pluginKey 是索引的键。**平台在前**，因为 id 在两个平台之间可以重复。
func pluginKey(platform, id string) string { return platform + ":" + id }

// pluginPlatformOf 给出某条记录的平台，老记录（没有 Platform 字段）算 android。
func pluginPlatformOf(a *pluginArtifact) string {
	if a == nil || strings.TrimSpace(a.Platform) == "" {
		return pluginPlatformAndroid
	}
	return a.Platform
}

// normalizePlatform 把外部传进来的平台名收口到两个合法值。
// 认不出来一律当 android —— 那是最早的、也是唯一有历史数据的那个。
func normalizePlatform(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case pluginPlatformWindows, "win", "windows-x64", "desktop", "pc":
		return pluginPlatformWindows
	default:
		return pluginPlatformAndroid
	}
}

// pluginExtFor 是包文件的后缀。**用它来决定清单路径和落盘文件名**。
func pluginExtFor(platform string) string {
	if platform == pluginPlatformWindows {
		return ".zip"
	}
	return ".apk"
}

func (s *Server) pendingPluginsDir() string {
	return filepath.Join(s.pluginsDir(), "pending")
}

func (s *Server) submissionsPath() string {
	return filepath.Join(s.pluginsDir(), "submissions.json")
}

func (s *Server) loadSubmissions() pluginSubmissionIndex {
	idx := pluginSubmissionIndex{Items: map[string]*pluginSubmission{}}
	raw, err := os.ReadFile(s.submissionsPath())
	if err != nil {
		return idx
	}
	// 同 loadPluginIndex：解析失败**当空**，不抛 —— 队列文件坏了的结果应该是
	// "暂时没有待审的"，而不是"服务端起不来"。
	//
	// ⚠️ 但**必须把原因打出来**。2026-10-15 这里吞掉过一个
	//    `json: cannot set embedded pointer to unexported struct`：
	//    表现是"文件明明是完整的，接口却只回外层几个字段"，
	//    而**没有任何一行日志**指向 JSON 解析 —— 查了很久才回到这里。
	if err := json.Unmarshal(raw, &idx); err != nil {
		log.Printf("plugin market: ⚠️ 待审队列文件解析失败（%v）—— 本次按空队列处理；文件 %s",
			err, s.submissionsPath())
	}
	if idx.Items == nil {
		idx.Items = map[string]*pluginSubmission{}
	}
	return idx
}

func (s *Server) saveSubmissions(idx pluginSubmissionIndex) error {
	if err := os.MkdirAll(s.pluginsDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.submissionsPath(), raw, 0o644)
}

// ---------------------------------------------------------------- 上传

// saveUploadedPackage 把一个上传的包落到目标目录，返回 (临时文件名, 字节数, sha256)。
//
// ⚠️ 一律先落**临时文件**：清单读得出来、校验过了才改名成正式名字。
//    顺序反过来的话，一个坏包会原地顶掉一个好包 —— 表现是
//    "列表里显示 v1.0.0，下下来的是个装不上的东西"。
func saveUploadedPackage(dir string, r io.Reader, maxBytes int64) (string, int64, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, "", err
	}
	tmp, err := os.CreateTemp(dir, "upload-*.part")
	if err != nil {
		return "", 0, "", err
	}
	name := tmp.Name()
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxBytes+1))
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(name)
		if err == nil {
			err = closeErr
		}
		return "", 0, "", err
	}
	if written > maxBytes {
		_ = os.Remove(name)
		return "", 0, "", fmt.Errorf("包太大了（上限 %d MB）", maxBytes>>20)
	}
	if written == 0 {
		_ = os.Remove(name)
		return "", 0, "", fmt.Errorf("没有收到任何内容（body 是空的）")
	}
	return name, written, hex.EncodeToString(h.Sum(nil)), nil
}

// readUploadBody 把请求体取出来。**两种格式都收**：
//
//	· raw body（我们自己脚本用的，零依赖）
//	· multipart/form-data（网页端 <input type=file> 用的，浏览器只会发这个）
//
// ⚠️ 必须两种都收：网页端上传插件的表单**不可能**发 raw body，
//    而只做 multipart 会让出包脚本要引一个 multipart 组装器。
//    这里按 Content-Type 分流，两条路最后汇成同一个 io.Reader。
func readUploadBody(r *http.Request) (io.Reader, func(), error) {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if !strings.Contains(ct, "multipart/form-data") {
		return r.Body, func() {}, nil
	}
	// 用 ParseMultipartForm 而不是 MultipartReader：文件在内存/临时目录里
	// 由标准库管，而 MultipartReader 要求调用方自己按顺序读、自己管清理，
	// 出错路径多一倍。上限已经在 saveUploadedPackage 里再卡一次。
	if err := r.ParseMultipartForm(pluginUploadMaxBytes); err != nil {
		return nil, func() {}, fmt.Errorf("表单解析失败：%v", err)
	}
	cleanup := func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}
	files := map[string][]*multipart.FileHeader{}
	if r.MultipartForm != nil {
		files = r.MultipartForm.File
	}
	// 字段名认 `file` / `package` / `apk` 三种 —— 表单是**我们自己写的**，
	// 但闸门/脚本/将来的人各写各的，多认两个名字比让人猜"为什么没收到文件"便宜。
	for _, key := range []string{"file", "package", "apk", "zip", "plugin"} {
		if hs := files[key]; len(hs) > 0 {
			f, err := hs[0].Open()
			if err != nil {
				cleanup()
				return nil, func() {}, fmt.Errorf("打不开上传的文件：%v", err)
			}
			return f, func() { _ = f.Close(); cleanup() }, nil
		}
	}
	cleanup()
	return nil, func() {}, fmt.Errorf("表单里没有文件字段（用 file / package / apk 任一个名字）")
}

// ---------------------------------------------------------------- 用户侧接口

// handlePluginSubmit 收一个**用户提交**的插件包，进待审队列。
//
//	POST /api/plugins/submit?platform=android&notes=...
//
// 登录即可（不限管理员）—— 这正是"以后用户也可以上传插件"那条需求。
//
// ★ 和 `handlePluginPublish` 一样，**包内清单是唯一真相**：
//
//	id / name / version / 能力清单全部由服务端开包读，不接受调用方传。
//	上传者只能额外给一段**给人看的备注**。
func (s *Server) handlePluginSubmit(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	if uid == "" {
		apiErr(w, 401, "请先登录再上传插件")
		return
	}
	q := r.URL.Query()
	platform := normalizePlatform(q.Get("platform"))

	// ⚠️⚠️ 顺序：**必须先解析 body，再读表单字段里的 notes**。
	//
	//    `r.MultipartForm` 在 `ParseMultipartForm` 跑之前**永远是 nil**，
	//    而解析就发生在 `readUploadBody` 里。先读 notes 的话表单里那段备注
	//    会被静默丢掉（query 里没有、表单还没解析）—— 现象是"提交成功了，
	//    但审核的人看不到任何说明"，而两边都不报错。
	//    （2026-10-15 冒烟时踩到的：`"notes":""`。）
	body, cleanup, err := readUploadBody(r)
	if err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	defer cleanup()

	notes := strings.TrimSpace(q.Get("notes"))
	if notes == "" && r.MultipartForm != nil {
		if v := r.MultipartForm.Value["notes"]; len(v) > 0 {
			notes = strings.TrimSpace(v[0])
		}
	}
	if len(notes) > 500 {
		notes = notes[:500]
	}

	tmp, written, sum, err := saveUploadedPackage(s.pendingPluginsDir(), body, pluginUploadMaxBytes)
	if err != nil {
		apiErr(w, 400, err.Error())
		return
	}

	probe, err := readPluginManifest(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		apiErr(w, 400, err.Error())
		return
	}
	if err := validatePluginProbe(probe); err != nil {
		_ = os.Remove(tmp)
		apiErr(w, 400, err.Error())
		return
	}
	if probe.APIVersion != pluginAPIVersion {
		log.Printf("plugin submit: %s 的 apiVersion=%d，服务端当前是 %d —— 客户端可能拒载",
			probe.ID, probe.APIVersion, pluginAPIVersion)
	}

	// 落盘名字：`<平台>-<id>-<版本><后缀>`。
	//
	// ⚠️ 不带中文、不用上传者本地的文件名：这个名字会进 download_url
	//    （可能是对象存储的键），也是用户机器上存下来的文件名。
	fileName := fmt.Sprintf("%s-%s-%s%s", platform, probe.ID,
		sanitizeVersionForFile(probe.Version), pluginExtFor(platform))
	fileName = filepath.Base(fileName)
	dst := filepath.Join(s.pendingPluginsDir(), fileName)
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		apiErr(w, 500, "替换旧提交失败："+err.Error())
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		apiErr(w, 500, "落地失败："+err.Error())
		return
	}

	art := &pluginArtifact{
		ID:          probe.ID,
		Platform:    platform,
		Name:        probe.Name,
		Icon:        orDefaultStr(probe.Icon, "🧩"),
		Desc:        probe.Desc,
		Version:     probe.Version,
		APIVersion:  probe.APIVersion,
		File:        fileName,
		SHA256:      sum,
		Size:        written,
		Notes:       notes,
		Order:       100,
		Pages:       pluginPagesOf(probe),
		Widgets:     pluginWidgetsOf(probe),
		AiTools:     pluginAiToolsOf(probe),
		Services:    pluginServicesOf(probe),
		PublishedAt: 0,
	}

	idx := s.loadSubmissions()
	key := pluginKey(platform, probe.ID)
	prev := idx.Items[key]
	idx.Items[key] = &pluginSubmission{
		pluginArtifact: *art, // 值内嵌（见结构体上那段说明）；art.Platform 已经是 platform
		Status:         pluginStatusPending,
		SubmitterID:    uid,
		SubmitterName:  userNameOf(s, uid),
		SubmittedAt:    time.Now().Unix(),
	}
	if err := s.saveSubmissions(idx); err != nil {
		_ = os.Remove(dst)
		apiErr(w, 500, "记录提交失败："+err.Error())
		return
	}

	replaced := ""
	if prev != nil {
		replaced = fmt.Sprintf("（覆盖了上一次提交的 %s）", prev.Version)
	}
	log.Printf("plugin submitted: %s/%s v%s by %s (%d bytes, sha256=%s) %s",
		platform, art.ID, art.Version, uid, written, sum[:12], replaced)
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"status": pluginStatusPending,
		"artifact": art,
		"replaced": replaced,
		"hint": "已提交，**等管理员审核通过后**才会出现在市场里。" +
			"在「我的提交」里可以看到状态和审核意见。",
	})
}

// handlePluginMySubmissions 列出**我自己**提交过的插件（含状态与审核意见）。
//
// ⚠️ 只返回提交者是自己的那些 —— 别人的提交内容和理由跟调用方无关。
func (s *Server) handlePluginMySubmissions(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	idx := s.loadSubmissions()
	out := []*pluginSubmission{}
	for _, it := range idx.Items {
		if it == nil || it.SubmitterID != uid {
			continue
		}
		out = append(out, it)
	}
	sortSubmissions(out)
	writeJSON(w, 200, map[string]any{"ok": true, "items": out, "me": uid})
}

// ---------------------------------------------------------------- 公开市场

// handlePluginMarketList 是**公开**的市场清单（网页端市场页用）。
//
// ## 为什么开匿名口子
//
// 市场页是"给人逛的"：还没注册的人也应该能看见有哪些插件、各多大、
// 能干什么 —— 那是这个页面的全部意义。而它的内容**本来就是公开的**
// （已发布插件的名称、描述、体积、下载地址）。
//
// ⚠️ 和 `handlePluginsList` 的区别只有一条：**过滤平台**。
//    客户端那一条不带平台参数（历史原因），所以默认 android。
func (s *Server) handlePluginMarketList(w http.ResponseWriter, r *http.Request) {
	platform := normalizePlatform(r.URL.Query().Get("platform"))
	idx := s.loadPluginIndex()
	out := make([]pluginListItem, 0, len(idx.Plugins))
	for _, a := range idx.Plugins {
		if a == nil || a.ID == "" {
			continue
		}
		if pluginPlatformOf(a) != platform {
			continue
		}
		item := pluginListItem{pluginArtifact: a}
		item.DownloadURL = "/api/market/download/" + platform + "/" + a.ID
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	writeJSON(w, 200, map[string]any{
		"ok":          true,
		"platform":    platform,
		"plugins":     out,
		"api_version": pluginAPIVersion,
		"server_time": time.Now().Unix(),
	})
}

// handlePluginMarketDownload 是市场页的下载口（公开）。
//
// ⚠️ 和 `/api/plugins/download/{id}` 分开，而不是把后者改成匿名：
//    后者是**客户端**在用的（带凭证），改它的鉴权语义会影响到已发布的 App。
//    这里是"网页上点一下"的路，两条互不干扰。
func (s *Server) handlePluginMarketDownload(w http.ResponseWriter, r *http.Request) {
	platform := normalizePlatform(r.PathValue("platform"))
	id := strings.TrimSpace(r.PathValue("id"))
	idx := s.loadPluginIndex()
	a := idx.Plugins[pluginKey(platform, id)]
	if a == nil {
		apiErr(w, 404, "市场里没有这个插件："+id)
		return
	}
	s.servePluginFile(w, r, a, "attachment")
}

// servePluginFile 把包流出去。两处下载口共用（`inline` 给市场页内联预览用）。
func (s *Server) servePluginFile(w http.ResponseWriter, r *http.Request, a *pluginArtifact, disposition string) {
	full := filepath.Join(s.pluginsDir(), filepath.Base(a.File))
	f, err := os.Open(full)
	if err != nil {
		apiErr(w, 410, "插件包不在服务器上了："+err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", pluginContentType(a.File))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, a.File))
	w.Header().Set("X-Plugin-Id", a.ID)
	w.Header().Set("X-Plugin-Version", a.Version)
	w.Header().Set("X-SHA256", a.SHA256)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	http.ServeContent(w, r, a.File, st.ModTime(), f)
}

func pluginContentType(file string) string {
	if strings.HasSuffix(strings.ToLower(file), ".zip") {
		return "application/zip"
	}
	return "application/vnd.android.package-archive"
}

// ---------------------------------------------------------------- 管理侧接口

// handleAdminPluginPending 列出待审队列（管理员）。
//
// `?status=` 空 = 只看待审；传 all = 连被驳回的一起看。
func (s *Server) handleAdminPluginPending(w http.ResponseWriter, r *http.Request) {
	// ⚠️⚠️ 极性**不能搞反**：`requireAdmin` 返回的是"**已经被拦下并写好响应了**"，
	//    不是"你有权限"。它的实现是"拦下 → 写 401/403 → return true"。
	//    写成 `if !s.requireAdmin(...) { return }` 的话，管理员会被**静默**跳过整个
	//    handler —— 现象是 **HTTP 200 且响应体是 0 字节**，而日志里那条
	//    `GET /api/admin/plugins/pending 0s` 看着完全正常。
	//    （2026-10-15 冒烟时真踩了：四个管理接口全是 200/0B，只有非管理员才"正常"地报 403。）
	if s.requireAdmin(w, r) {
		return
	}
	wantAll := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("status")), "all")
	idx := s.loadSubmissions()
	out := []*pluginSubmission{}
	for _, it := range idx.Items {
		if it == nil {
			continue
		}
		if !wantAll && it.Status != pluginStatusPending {
			continue
		}
		out = append(out, it)
	}
	sortSubmissions(out)
	writeJSON(w, 200, map[string]any{
		"ok":     true,
		"items":  out,
		"count":  len(out),
		"dir":    s.pendingPluginsDir(),
		"status": map[bool]string{true: "all", false: pluginStatusPending}[wantAll],
	})
}

// handleAdminPluginReview 审核一条提交。
//
//	POST /api/admin/plugins/review
//	{ "platform":"android", "id":"notes", "action":"approve"|"reject",
//	  "note":"给提交者看的理由", "order":100 }
//
// ★ 通过时做三件事，**顺序不能反**：
//
//	① 把包从 pending/ 移到 plugins/（先移，失败就整体不生效）
//	② 写进已发布索引（manifest.json）
//	③ 才把提交记录标成 published
//
//	反过来（先标状态再移文件）会出现"审核显示已通过，但市场里下不下来"——
//	而那是**没有任何提示**的一种坏。
func (s *Server) handleAdminPluginReview(w http.ResponseWriter, r *http.Request) {
	// ⚠️⚠️ 极性**不能搞反**：`requireAdmin` 返回的是"**已经被拦下并写好响应了**"，
	//    不是"你有权限"。它的实现是"拦下 → 写 401/403 → return true"。
	//    写成 `if !s.requireAdmin(...) { return }` 的话，管理员会被**静默**跳过整个
	//    handler —— 现象是 **HTTP 200 且响应体是 0 字节**，而日志里那条
	//    `GET /api/admin/plugins/pending 0s` 看着完全正常。
	//    （2026-10-15 冒烟时真踩了：四个管理接口全是 200/0B，只有非管理员才"正常"地报 403。）
	if s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
		Action   string `json:"action"`
		Note     string `json:"note"`
		Order    int    `json:"order"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	platform := normalizePlatform(body.Platform)
	id := strings.TrimSpace(body.ID)
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if id == "" {
		apiErr(w, 400, "缺少 id")
		return
	}
	if action != "approve" && action != "reject" {
		apiErr(w, 400, `action 只能是 "approve" 或 "reject"`)
		return
	}

	subs := s.loadSubmissions()
	key := pluginKey(platform, id)
	sub := subs.Items[key]
	if sub == nil {
		apiErr(w, 404, "待审队列里没有这条："+key)
		return
	}
	reviewer := userNameOf(s, userIDOf(r))
	note := strings.TrimSpace(body.Note)

	if action == "reject" {
		// 驳回：**删掉包文件**，只留记录（理由要能回给提交者）。
		// 留着包没意义 —— 它没有被任何人使用，而磁盘上的每一份多余副本
		// 都是一个将来会有人问"这是什么"的东西。
		_ = os.Remove(filepath.Join(s.pendingPluginsDir(), filepath.Base(sub.File)))
		sub.Status = pluginStatusRejected
		sub.ReviewNote = orDefaultStr(note, "未说明理由")
		sub.ReviewerName = reviewer
		sub.ReviewedAt = time.Now().Unix()
		if err := s.saveSubmissions(subs); err != nil {
			apiErr(w, 500, "保存失败："+err.Error())
			return
		}
		log.Printf("plugin rejected: %s by %s（%s）", key, reviewer, sub.ReviewNote)
		writeJSON(w, 200, map[string]any{"ok": true, "status": pluginStatusRejected, "item": sub})
		return
	}

	// ---- 通过 ----
	src := filepath.Join(s.pendingPluginsDir(), filepath.Base(sub.File))
	if _, err := os.Stat(src); err != nil {
		apiErr(w, 410, "包文件不在了（可能已被驳回）："+err.Error())
		return
	}
	dst := filepath.Join(s.pluginsDir(), filepath.Base(sub.File))
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		apiErr(w, 500, "替换旧包失败："+err.Error())
		return
	}
	if err := os.Rename(src, dst); err != nil {
		apiErr(w, 500, "移动包失败："+err.Error())
		return
	}

	order := sub.Order
	if body.Order > 0 {
		order = body.Order
	}
	art := sub.pluginArtifact
	art.Platform = platform
	art.Order = order
	art.PublishedAt = time.Now().Unix()
	if strings.TrimSpace(note) != "" {
		// 审核意见对已发布的插件就是"版本说明"，两个字段合并成一个入口
		art.Notes = note
	}

	pub := s.loadPluginIndex()
	prev := pub.Plugins[key]
	pub.Plugins[key] = &art
	if err := s.savePluginIndex(pub); err != nil {
		_ = os.Rename(dst, src) // 回滚：包放回待审目录，索引没动
		apiErr(w, 500, "写索引失败（已回滚）："+err.Error())
		return
	}

	sub.Status = pluginStatusPublished
	sub.ReviewNote = note
	sub.ReviewerName = reviewer
	sub.ReviewedAt = time.Now().Unix()
	if err := s.saveSubmissions(subs); err != nil {
		log.Printf("plugin review: 索引已更新但提交记录写失败：%v", err)
	}

	log.Printf("plugin approved: %s v%s by %s（替换 %v）", key, art.Version, reviewer, prev != nil)
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"status":   pluginStatusPublished,
		"artifact": &art,
		"replaced": prev,
		"download": s.pluginDownloadURLFor(&art),
		"hint":     "已上架，市场里立刻可见（客户端**不用重启**）。",
	})
}

// handleAdminPluginPublished 列出已发布的插件（管理员），附带磁盘对账信息。
func (s *Server) handleAdminPluginPublished(w http.ResponseWriter, r *http.Request) {
	// ⚠️⚠️ 极性**不能搞反**：`requireAdmin` 返回的是"**已经被拦下并写好响应了**"，
	//    不是"你有权限"。它的实现是"拦下 → 写 401/403 → return true"。
	//    写成 `if !s.requireAdmin(...) { return }` 的话，管理员会被**静默**跳过整个
	//    handler —— 现象是 **HTTP 200 且响应体是 0 字节**，而日志里那条
	//    `GET /api/admin/plugins/pending 0s` 看着完全正常。
	//    （2026-10-15 冒烟时真踩了：四个管理接口全是 200/0B，只有非管理员才"正常"地报 403。）
	if s.requireAdmin(w, r) {
		return
	}
	idx := s.loadPluginIndex()
	out := make([]pluginListItem, 0, len(idx.Plugins))
	for k, a := range idx.Plugins {
		if a == nil || a.ID == "" {
			continue
		}
		item := pluginListItem{pluginArtifact: a}
		item.DownloadURL = s.pluginDownloadURLFor(a)
		out = append(out, item)
		// 磁盘对账：索引指着文件，文件却不在 —— 这是"市场里能看见、点下载 404"。
		_ = k
	}
	sort.Slice(out, func(i, j int) bool {
		if pluginPlatformOf(out[i].pluginArtifact) != pluginPlatformOf(out[j].pluginArtifact) {
			return pluginPlatformOf(out[i].pluginArtifact) < pluginPlatformOf(out[j].pluginArtifact)
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})

	// 磁盘上有、索引里没有的包 = 上次发布留下的孤儿。报出来，但**不自动删**
	// （自动删 = 一次误判就毁掉一个能重新上架的包）。
	missing := []string{}
	orphans := []string{}
	known := map[string]bool{}
	for _, a := range idx.Plugins {
		if a == nil || a.File == "" {
			continue
		}
		known[filepath.Base(a.File)] = true
		if _, err := os.Stat(filepath.Join(s.pluginsDir(), filepath.Base(a.File))); err != nil {
			missing = append(missing, a.File)
		}
	}
	entries, _ := os.ReadDir(s.pluginsDir())
	for _, e := range entries {
		if e.IsDir() || e.Name() == "manifest.json" || e.Name() == "submissions.json" {
			continue
		}
		if !known[e.Name()] {
			orphans = append(orphans, e.Name())
		}
	}
	sort.Strings(missing)
	sort.Strings(orphans)

	writeJSON(w, 200, map[string]any{
		"ok":          true,
		"plugins":     out,
		"missing":     missing,
		"orphans":     orphans,
		"dir":         s.pluginsDir(),
		"api_version": pluginAPIVersion,
	})
}

// handleAdminPluginDelist 下架一个已发布的插件。
//
// ## ★ 语义：从索引里摘掉，**不删包**
//
// 客户端商店读的是索引 —— 摘掉之后新装的用户看不到它。
// 而**已经装了的人照旧用**：这正是我们要的（撤掉一个包不该让用户手里的功能消失）。
// 包文件留在磁盘上，所以"下架错了"只要重新上架即可，不需要重新上传。
//
// ⚠️ 这也意味着：下架**不是**"阻止分发"的手段。真要阻止，
//    正确做法是发一个"什么也不做"的新版本（见 plugins.go 顶部那段）。
func (s *Server) handleAdminPluginDelist(w http.ResponseWriter, r *http.Request) {
	// ⚠️⚠️ 极性**不能搞反**：`requireAdmin` 返回的是"**已经被拦下并写好响应了**"，
	//    不是"你有权限"。它的实现是"拦下 → 写 401/403 → return true"。
	//    写成 `if !s.requireAdmin(...) { return }` 的话，管理员会被**静默**跳过整个
	//    handler —— 现象是 **HTTP 200 且响应体是 0 字节**，而日志里那条
	//    `GET /api/admin/plugins/pending 0s` 看着完全正常。
	//    （2026-10-15 冒烟时真踩了：四个管理接口全是 200/0B，只有非管理员才"正常"地报 403。）
	if s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Platform string `json:"platform"`
		ID       string `json:"id"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	platform := normalizePlatform(body.Platform)
	id := strings.TrimSpace(body.ID)
	key := pluginKey(platform, id)

	idx := s.loadPluginIndex()
	prev := idx.Plugins[key]
	if prev == nil {
		apiErr(w, 404, "已发布列表里没有这条："+key)
		return
	}
	delete(idx.Plugins, key)
	if err := s.savePluginIndex(idx); err != nil {
		apiErr(w, 500, "保存失败："+err.Error())
		return
	}
	log.Printf("plugin delisted: %s v%s", key, prev.Version)
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"delisted": prev,
		"hint": "已下架。**已经装了的用户不受影响**；包文件仍留在服务器上，" +
			"需要重新上架时不用再传一遍。",
	})
}

// ---------------------------------------------------------------- 小工具

// sanitizeVersionForFile 把版本号里不能进文件名的字符换掉。
// 版本号来自包内清单（**外部输入**），直接拼进路径是危险的。
func sanitizeVersionForFile(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "0"
	}
	var b strings.Builder
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_', c == '+':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// sortSubmissions 的排序是**确定性**的：待审在前、按提交时间倒序。
// map 遍历本来就是随机的，没有这一层的话两次刷新顺序都不一样。
func sortSubmissions(list []*pluginSubmission) {
	sort.Slice(list, func(i, j int) bool {
		pi := list[i].Status == pluginStatusPending
		pj := list[j].Status == pluginStatusPending
		if pi != pj {
			return pi
		}
		if list[i].SubmittedAt != list[j].SubmittedAt {
			return list[i].SubmittedAt > list[j].SubmittedAt
		}
		return list[i].ID < list[j].ID
	})
}

// userNameOf 查一个用户的名字（查不到就回 id）。
//
// 为什么要有它：`userIDOf` 只给 id，而管理台上"谁提交的"要显示名字 ——
// 显示一串 id 等于没显示。
func userNameOf(s *Server, uid string) string {
	if strings.TrimSpace(uid) == "" {
		return ""
	}
	var name string
	if err := s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, uid).Scan(&name); err != nil || strings.TrimSpace(name) == "" {
		return uid
	}
	return name
}
