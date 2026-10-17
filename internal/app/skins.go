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
	"regexp"
	"sort"
	"strings"
	"time"
)

// 外观包（skinpack）的分发 —— 2026-10-14。
//
// ## 它是 `packs.go`（导航数据包）的**同构物**，不是它的抽象
//
// 文件头先说清楚为什么不把两者合并成一套"通用包"：
//
//	导航包和外观包**除了形状像，没有一个共同的生命周期**——校验字段不同、
//	客户端的解析器不同（`NavPack.kt` / `SkinPack.kt`）、失败原因要给人看的话也不同
//	（"楼栋缺坐标" vs "配色角色写错名"）。合并的结果一定是"多一张字段表 + 一堆 if"，
//	而**校验是最不能抽象的地方**（抽象错了 = 坏包进库，而坏包的问题是"装上去一半生效"）。
//
// 所以这里刻意**照着 packs.go 抄一遍**。抄的代价是"改一条纪律要改两处"，
// 用一条注释纪律兜住：**两边的差异只允许出现在"包里有什么"上，流程必须一致**
// （授权码口径 / 限流 / 落盘方式 / 索引 / 响应头 / 计数）。
//
// ## 授权：和 navpack **逐字相同**
//
//   - **授权码就是包标识（packId）本身** —— 没有第二张表。用户手里那个码就是包标识。
//   - 列表与下载走 `ext()`（要登录态或机器 Key），**不开匿名口子**。
//   - 上传 / 删除 / 管理列表走 `s.auth + requireAdmin`。
//
// ## 外观包比导航包小得多
//
// 一套外观就是几十个色值 + 几个字段：**几 KB**。所以：
//   - 上限收到 **1MB**（导航包是 4MB）—— 超过就说明贴错了东西；
//   - **不上传 COS**：几 KB 的 JSON 走本机文件就够了，多一条外部依赖没有收益。

const (
	skinPackFormatTag       = "schedule.skinpack"
	skinPackFormatTagLegacy = "threees.skinpack" // 兼容历史包
)

// 服务端认到的格式版本上限。⚠️ 和设计台、手机端 `SkinPack.kt` 三处必须一致：
// 三处不一致的表现是"本地导出成功、上传被拒"（或者反过来，更糟）。
//
// v2（2026-10-14）：skin 里新增 `home_layout`（主页块布局，形状 = 手机端
// `HomeLayout`）+ `page_blocks` 升级为 6 种 kind（heading/text/actions/links/
// card/divider，形状 = 手机端 `PageBlock`）。v1 包仍然全兼容 —— 服务端对
// skin 正文本来就是透传，这里只是把"认到几"放宽。
const skinPackFormatVersion = 2

// 单个外观包的字节上限。
const skinPackMaxBytes = 1 << 20 // 1MB

// skinPackDoc 是**上传上来的原文**解出来的那一层（我们只挑要用的字段，其余原样存）。
type skinPackDoc struct {
	Format        string          `json:"format"`
	FormatVersion int             `json:"formatVersion"`
	PackID        string          `json:"packId"`
	Name          string          `json:"name"`
	Note          string          `json:"note"`
	Author        string          `json:"author"`
	Version       string          `json:"version"`
	UpdatedAt     int64           `json:"updatedAt"`
	Seed          string          `json:"seed"`
	Dark          bool            `json:"dark"`
	Skin          json.RawMessage `json:"skin"`
}

// skinPackArtifact 是索引里的一条。
//
// ⚠️ **不含 skin 正文**：列表是"给选包的人看的"，把整套皮肤塞进列表等于
// 每次开管理台都传几十 KB。要看内容，用 `.../{id}/view`（那也是掩码过的）。
type skinPackArtifact struct {
	PackID     string `json:"pack_id"`
	Name       string `json:"name"`
	Note       string `json:"note"`
	Author     string `json:"author"`
	Version    string `json:"version"`
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Seed       string `json:"seed"`
	Dark       bool   `json:"dark"`
	// RoleCount / PageCount 让管理台**不用开包**就能回答两个最常问的问题：
	// "这套改了哪些配色角色"、"它有没有分页设计"。和 navpack 记 Floors/Rooms 同一个理由。
	// HomeBlocks（v2）：主页自定义布局带了几块 —— "这套有没有动布局"。
	RoleCount     int   `json:"role_count"`
	PageCount     int   `json:"page_count"`
	HomeBlocks    int   `json:"home_blocks"`
	FormatVersion int   `json:"format_version"`
	PublishedAt   int64 `json:"published_at"`
	UpdatedAt     int64 `json:"updated_at"`
	Downloads     int64 `json:"downloads"`
}

type skinPackIndex struct {
	Packs map[string]*skinPackArtifact `json:"packs"`
}

func (s *Server) skinsDir() string { return filepath.Join(s.cfg.DataDir, "skinpacks") }

func (s *Server) skinPackIndexPath() string {
	return filepath.Join(s.skinsDir(), "index.json")
}

func (s *Server) loadSkinPackIndex() skinPackIndex {
	idx := skinPackIndex{Packs: map[string]*skinPackArtifact{}}
	raw, err := os.ReadFile(s.skinPackIndexPath())
	if err != nil {
		return idx
	}
	// ⚠️ 解析失败**当空索引**（同 `loadNavPackIndex`）：索引坏了的结果应该是
	// "管理台暂时是空的"，而不是"服务端起不来"。
	_ = json.Unmarshal(raw, &idx)
	if idx.Packs == nil {
		idx.Packs = map[string]*skinPackArtifact{}
	}
	return idx
}

func (s *Server) saveSkinPackIndex(idx skinPackIndex) error {
	if err := os.MkdirAll(s.skinsDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	// 和插件索引同一套"先写 tmp 再改名"：要的是**原子**，不是快。
	tmp := s.skinPackIndexPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.skinPackIndexPath())
}

// 包标识（= 授权码）的合法形状。
// ⚠️ 和设计台（`skin-studio.js` 的 preflight）、手机端（`SkinPack.kt`）三处一字不差。
var skinPackIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,31}$`)

func validSkinPackID(id string) bool { return skinPackIDRe.MatchString(id) }

// parseSkinPack 校验一份外观包。**这是权威判据**（客户端那两份只是为了少走一个来回）。
//
// 只做**结构**校验，不做"好不好看"的判断：配色是否协调是设计台的事，
// 而服务端拒一个包的代价是"作者拿不到明确的理由"。
func parseSkinPack(raw []byte) (*skinPackDoc, error) {
	var doc skinPackDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("不是合法的 JSON：%v", err)
	}
	if doc.Format != skinPackFormatTag && doc.Format != skinPackFormatTagLegacy {
		return nil, fmt.Errorf("format 应该是 %s（实际是 %q）", skinPackFormatTag, doc.Format)
	}
	if doc.FormatVersion <= 0 {
		return nil, fmt.Errorf("缺 formatVersion")
	}
	if doc.FormatVersion > skinPackFormatVersion {
		return nil, fmt.Errorf("formatVersion=%d，这台服务端只认到 %d", doc.FormatVersion, skinPackFormatVersion)
	}
	if !validSkinPackID(doc.PackID) {
		return nil, fmt.Errorf("packId（= 授权码）不合法：%q（要 3~32 位小写字母/数字/中划线，字母数字开头）", doc.PackID)
	}
	if strings.TrimSpace(doc.Name) == "" {
		return nil, fmt.Errorf("name 不能空 —— 用户在列表里就是靠它认包的")
	}
	if len(doc.Skin) == 0 || string(doc.Skin) == "null" {
		return nil, fmt.Errorf("缺 skin 那一段（拿不到要应用的外观）")
	}
	if doc.Version == "" {
		doc.Version = "1.0.0"
	}
	return &doc, nil
}

// 统计"这套外观改了多少东西" —— 给管理台用的两个数，**不进校验**。
func skinPackStats(doc *skinPackDoc) (roles int, pages int, homeBlocks int) {
	var skin map[string]json.RawMessage
	if err := json.Unmarshal(doc.Skin, &skin); err != nil {
		return 0, 0, 0
	}
	if raw, ok := skin["colors"]; ok {
		var colors map[string]string
		if json.Unmarshal(raw, &colors) == nil {
			roles = len(colors)
		}
	}
	seen := map[string]bool{}
	// ⚠️ 双口径：v1 设计台发的是 camelCase（pageBg…），v2 发的是手机端口径的
	// snake_case（page_bg…）—— 两边都要数，否则某一版的"分页设计"计数恒为 0。
	for _, key := range []string{"pageBg", "pageTitles", "pageBlocks", "hiddenTabs",
		"page_bg", "page_titles", "page_blocks", "hidden_tabs"} {
		raw, ok := skin[key]
		if !ok {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		for id := range m {
			seen[id] = true
		}
	}
	// v2：主页块布局改了几块（列表里回答"这套有没有自定义布局"用的，不开包）。
	// ⚠️ 双形态都吃：设计台写对象，手改的可能是整段转义成字符串。
	if raw, ok := skin["home_layout"]; ok {
		var asStr string
		if json.Unmarshal(raw, &asStr) == nil {
			raw = []byte(asStr)
		}
		var hl struct {
			Blocks []json.RawMessage `json:"blocks"`
		}
		if json.Unmarshal(raw, &hl) == nil {
			homeBlocks = len(hl.Blocks)
		}
	}
	return roles, len(seen), homeBlocks
}

func skinPackFileName(packID string) string { return packID + ".json" }

// ---------------------------------------------------------------- 用户侧

type skinPackListItem struct {
	PackID      string `json:"pack_id"`
	Name        string `json:"name"`
	Note        string `json:"note"`
	Author      string `json:"author"`
	Version     string `json:"version"`
	Dark        bool   `json:"dark"`
	RoleCount   int    `json:"role_count"`
	PageCount   int    `json:"page_count"`
	HomeBlocks  int    `json:"home_blocks"`
	UpdatedAt   int64  `json:"updated_at"`
	DownloadURL string `json:"download_url"`
}

// 用户侧列表：**给所有已登录用户看**（和 navpack 一样）。
// 授权码是"知道就能下"的口子，列表本身不额外泄露什么（包里没有秘密）。
func (s *Server) handleSkinPacksList(w http.ResponseWriter, r *http.Request) {
	idx := s.loadSkinPackIndex()
	out := make([]skinPackListItem, 0, len(idx.Packs))
	for _, a := range idx.Packs {
		if a == nil || a.PackID == "" {
			continue
		}
		out = append(out, skinPackListItem{
			PackID:      a.PackID,
			Name:        a.Name,
			Note:        a.Note,
			Author:      a.Author,
			Version:     a.Version,
			Dark:        a.Dark,
			RoleCount:   a.RoleCount,
			PageCount:   a.PageCount,
			HomeBlocks:  a.HomeBlocks,
			UpdatedAt:   a.UpdatedAt,
			DownloadURL: s.skinPackDownloadURLFor(a),
		})
	}
	// 排序放服务端：三个客户端各自排一遍必然漂（同 navpack）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].PackID < out[j].PackID
	})
	writeJSON(w, 200, map[string]any{"packs": out, "count": len(out)})
}

// 下载地址。⚠️ 外观包**不走 COS**（几 KB 没必要），所以这里恒返回本机 API 路径 ——
// 但方法留着，将来真要放 CDN 时改这一处就够（同 `navPackDownloadURLFor` 的形状）。
func (s *Server) skinPackDownloadURLFor(a *skinPackArtifact) string {
	return "/api/skinpacks/download/" + a.PackID
}

// 按授权码下载：**原样吐文件**（不重新序列化 —— 存进去是什么，拿出来就是什么）。
func (s *Server) handleSkinPackDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !validSkinPackID(id) {
		apiErr(w, 400, "授权码形状不对")
		return
	}
	idx := s.loadSkinPackIndex()
	a := idx.Packs[id]
	if a == nil {
		// ⚠️ 404 的人话由客户端翻译（"没有这个授权码对应的外观包"）——
		//    这里给英文级别的短语，别在服务端拼中文句子（两边拼两遍必然不一致）。
		apiErr(w, 404, "no such pack")
		return
	}
	path := filepath.Join(s.skinsDir(), skinPackFileName(id))
	raw, err := os.ReadFile(path)
	if err != nil {
		apiErr(w, 410, "index says it exists but the file is gone")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.skinpack.json"`)
	w.Header().Set("X-Pack-Id", a.PackID)
	w.Header().Set("X-Pack-Version", a.Version)
	if a.SHA256 != "" {
		w.Header().Set("X-SHA256", a.SHA256)
	}
	// 计数在**响应之后**写，而且失败只记日志（同 navpack）：下载是用户的正当行为，
	// 一次写索引失败不该让下载变成 500。
	defer s.bumpSkinPackDownloads(id)
	_, _ = w.Write(raw)
}

func (s *Server) bumpSkinPackDownloads(packID string) {
	defer func() { _ = recover() }()
	idx := s.loadSkinPackIndex()
	if a := idx.Packs[packID]; a != nil {
		a.Downloads++
		if err := s.saveSkinPackIndex(idx); err != nil {
			log.Printf("skinpack: 下载计数写失败 %s: %v", packID, err)
		}
	}
}

// ---------------------------------------------------------------- 管理侧

func (s *Server) handleSkinPackAdminList(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	idx := s.loadSkinPackIndex()
	out := make([]*skinPackArtifact, 0, len(idx.Packs))
	for _, a := range idx.Packs {
		if a != nil {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	writeJSON(w, 200, map[string]any{
		"packs":     out,
		"count":     len(out),
		"dir":       s.skinsDir(),
		"auth_note": "授权码就是包标识（pack_id）本身",
	})
}

// 上传：raw body，`?replace=1` 覆盖同 id 的旧包。
func (s *Server) handleSkinPackUpload(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	replace := strings.TrimSpace(r.URL.Query().Get("replace")) == "1"
	if err := os.MkdirAll(s.skinsDir(), 0o755); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	// ⚠️ LimitReader 而不是无上限 io.Copy：一个 100MB 的"包"能吃掉这台机器的内存，
	// 而它本来就该被拒。
	raw, err := io.ReadAll(io.LimitReader(r.Body, skinPackMaxBytes+1))
	if err != nil {
		apiErr(w, 500, "读取失败："+err.Error())
		return
	}
	if len(raw) == 0 {
		apiErr(w, 400, "body 是空的 —— 请把 .skinpack.json 的内容原样传上来")
		return
	}
	if len(raw) > skinPackMaxBytes {
		apiErr(w, 413, fmt.Sprintf("文件超过 %dKB —— 外观包不该这么大（是不是贴错了别的东西？）", skinPackMaxBytes>>10))
		return
	}

	doc, err := parseSkinPack(raw)
	if err != nil {
		apiErr(w, 400, err.Error())
		return
	}

	idx := s.loadSkinPackIndex()
	if old := idx.Packs[doc.PackID]; old != nil && !replace {
		apiErr(w, 409, fmt.Sprintf("已经有一个叫 %s 的包（v%s）—— 要覆盖它请带 ?replace=1", doc.PackID, old.Version))
		return
	}

	// sha256 由**服务端**算（客户端自报的校验和不算数）：它写进索引和响应头，
	// 让"用户下到的和作者上传的是不是同一份"以后有据可查。
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	roles, pages, homeBlocks := skinPackStats(doc)

	final := filepath.Join(s.skinsDir(), skinPackFileName(doc.PackID))
	// 先写临时文件、校验通过才改名 —— 半截文件**永远不该**成为"某个授权码的内容"。
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		apiErr(w, 500, "落盘失败："+err.Error())
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		apiErr(w, 500, "改名失败："+err.Error())
		return
	}

	now := time.Now().Unix()
	prev := idx.Packs[doc.PackID]
	a := &skinPackArtifact{
		PackID:        doc.PackID,
		Name:          doc.Name,
		Note:          doc.Note,
		Author:        doc.Author,
		Version:       doc.Version,
		File:          skinPackFileName(doc.PackID),
		SHA256:        sha,
		Size:          int64(len(raw)),
		Seed:          doc.Seed,
		Dark:          doc.Dark,
		RoleCount:     roles,
		PageCount:     pages,
		HomeBlocks:    homeBlocks,
		FormatVersion: doc.FormatVersion,
		PublishedAt:   now,
		UpdatedAt:     now,
	}
	if prev != nil {
		// 覆盖时保留两件事：首发时间和下载计数（它们是**这个包的历史**，
		// 不该因为改了一个色值就归零）。
		a.PublishedAt = prev.PublishedAt
		a.Downloads = prev.Downloads
	}
	idx.Packs[doc.PackID] = a
	if err := s.saveSkinPackIndex(idx); err != nil {
		apiErr(w, 500, "索引保存失败："+err.Error())
		return
	}

	log.Printf("skinpack published: %s v%s (fv%d, %d bytes, %d roles, %d pages, %d home blocks) %s",
		doc.PackID, doc.Version, doc.FormatVersion, len(raw), roles, pages, homeBlocks, doc.Name)
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"pack_id":   a.PackID,
		"name":      a.Name,
		"version":   a.Version,
		"sha256":    a.SHA256,
		"size":      a.Size,
		"replaced":  prev != nil,
		"auth_code": a.PackID,
	})
}

func (s *Server) handleSkinPackDelete(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !validSkinPackID(id) {
		apiErr(w, 400, "授权码形状不对")
		return
	}
	idx := s.loadSkinPackIndex()
	if idx.Packs[id] == nil {
		apiErr(w, 404, "no such pack")
		return
	}
	delete(idx.Packs, id)
	if err := s.saveSkinPackIndex(idx); err != nil {
		apiErr(w, 500, "索引保存失败："+err.Error())
		return
	}
	// ⚠️ 文件删失败**不算这次失败**：索引里已经没有它了，用户查不到也下不到 ——
	// 剩下一个孤儿文件只占几 KB，比"删除报错但其实已经删了"清楚得多。
	_ = os.Remove(filepath.Join(s.skinsDir(), skinPackFileName(id)))
	writeJSON(w, 200, map[string]any{"ok": true, "pack_id": id})
}

// 管理台预览：把包**原样**给回去（外观包几 KB，没必要做摘要版）。
//
// ⚠️ 和 navpack 不同：那边要掩码腾讯 Key，这边包里没有秘密（就是配色），
// 但**仍然要管理员**——"能不能看到别人的包"是权限问题，不是保密问题。
func (s *Server) handleSkinPackAdminView(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !validSkinPackID(id) {
		apiErr(w, 400, "授权码形状不对")
		return
	}
	raw, err := os.ReadFile(filepath.Join(s.skinsDir(), skinPackFileName(id)))
	if err != nil {
		apiErr(w, 404, "no such pack file")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}
