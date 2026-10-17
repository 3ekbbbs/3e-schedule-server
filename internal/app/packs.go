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
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 导航数据包分发（2026-10-12）。
//
// ## 它是什么，和 plugins.go 是什么关系
//
//	plugins.go  分发**代码**（apk）—— 装一次，之后一直在那儿
//	packs.go    分发**数据**（.navpack.json）—— 一份楼栋坐标表
//
// 两者形状相似（都是"一个 id 一条线 + 磁盘上一个文件"），但**刻意没有合并**：
//
//	· 插件包是 apk，元数据靠**开 zip 读清单**得到（那是它唯一的真相）；
//	  数据包是纯 JSON，它自己**就是**那份清单 —— 没有"包内元数据"这一层。
//	  硬合并会让 zip 那套逻辑套在一个本来不需要 zip 的东西上。
//	· 插件的 id 由**出包的人**写死在清单里；数据包的 packId 由**管理员在后台建**。
//	  前者是"包自己说我叫什么"，后者是"我们给它起个名字"。
//
// ## ★ 授权码就是 packId
//
// 用户拍板的口径：「一个包对应一个授权码」。所以**没有第二张表** ——
// 用户手里那个码，就是包标识本身（`kindergarten-main` 这样）。
//
// 为什么不另发一串随机码：
//
//	· 多一层映射 = 多一个"码还对不对得上包"的失效点；
//	· 码本身要**能读、能念、能写在纸上**（"输 kindergarten-main"），
//	  随机串做不到这一点；
//	· 数据包是给"一个地方的一群人"用的，它的标识天生就该是人能认的名字。
//
// ⚠️ 代价：packId 是**可猜的**。所以下载接口不留匿名口子（见 §安全）。
//
// ## ★ 唯一真相是这个文件本身
//
// 和 plugins.go 同一条纪律：上传时**不接受调用方传 name / version / 楼栋数**，
// 一律由服务端打开这份 JSON 读。理由也一样 —— 让出包脚本把元数据当参数一起传，
// "文件里写的"和"服务端记的"就成了两份数据，中间没有任何机制保证一致。
//
// ## 安全（Key 是明文，这一段必须看）
//
// 分发包里**可能带腾讯 Key**（用户拍板：「通过授权码下载的包是携带的」）。
// 所以：
//
//	· 下载接口走 `ext()`（要登录态或机器 Key），**不开匿名口子**；
//	· 上传/删除/列表是 `s.auth` + `requireAdmin`；
//	· 索引**不对外**提供任何匿名接口。
//
// ⚠️ 仍然挡不住"一个有登录态的人拿到包之后把 Key 抠出来" —— 那是这条路的
//    固有代价，缓解手段在腾讯控制台那一侧（来源限制 + 用量告警 + 随时吊销）。
//    详见 docs/格式-导航数据包-navpack-2026-10-12.md §五。
// ---------------------------------------------------------------------------

// 当前数据包格式版本。**必须和电脑端工具 / 插件里的版本口径相等。**
//
// ★ 2026-10-12 晚：标注台升到 **2**（导出 `type` / `floors` / `rooms`）。
//
// 版本口径（和格式文档 §9.5、插件 `NavPack.MAX_FORMAT_VERSION` 三处**必须一致**）：
//
//	· 1 和 2 **都接受** —— v2 是 v1 的超集，旧包不该因为服务端升级就传不上去；
//	· > 2 **拒收** —— 服务端不认识的新字段会被"绕过"，
//	  而数据包是**分发的**，安静地做错事等于所有人一起错。
//
// ⚠️ `navPackFormatVersion` 现在表示"**当前**导出的是 2"（用于回给管理台显示），
//
//	校验用下面的 `minNavPackFormatVersion` / `maxNavPackFormatVersion` 区间。
const navPackFormatVersion = 2

// 可接受的版本区间（闭区间）。
const (
	minNavPackFormatVersion = 1
	maxNavPackFormatVersion = 2
)

const (
	navPackFormatTag       = "schedule.navpack"
	navPackFormatTagLegacy = "threees.navpack" // 兼容历史包
)

// navPackDoc 是文件里的那一份。**只挑服务端要用的字段**。
//
// ⚠️ 刻意不复刻整份格式（比如 `origin` 里的 name）—— 服务端**不执行**这个包，
//
//	多知道一个字都是负担。它只需要：能校验、能列表、能把它原样存下来。
//	真正消费它的是插件（手机端），那时读的是**文件原文**，不是这个结构体。
//
// 所以下面 `Raw` 字段是关键：**存的是上传上来的字节，不是反序列化再序列化的结果**。
// 为什么：如果这里 round-trip 一遍，任何我们没建模的字段都会**被丢掉**
// （比如 v2 的 `floors`，服务端不认识就抹了），而且不报错。
// 那正是"数据包发出去，某个功能默默没了"。
//
// ★ 但索引里要能看出"这个包标了分层没有" —— 所以 `Buildings` 里**数一下**
//
//	`floors` / `rooms` 的条数（只数，不回写）。
type navPackDoc struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"formatVersion"`
	PackID        string `json:"packId"`
	Name          string `json:"name"`
	Note          string `json:"note"`
	Version       string `json:"version"`
	UpdatedAt     int64  `json:"updatedAt"`
	// Buildings 只用来数个数、看坐标齐不齐、数分层。**不要拿它重写文件**。
	Buildings []struct {
		ID   string  `json:"id"`
		Name string  `json:"name"`
		Lat  float64 `json:"lat"`
		Lng  float64 `json:"lng"`
		// ★ v2：只数条数。`Floors` 里的 `Rooms` 同理 ——
		//   这里**不需要**知道房间的坐标，那只有插件渲染时才关心。
		Floors []struct {
			Rooms []struct{} `json:"rooms"`
		} `json:"floors"`
	} `json:"buildings"`
	Origin *struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	} `json:"origin"`
}

// navPackArtifact 是索引里的一条。**不含 Key**。
//
// ⚠️ 这份索引是给**管理台**看的，而管理台要能回答"这个包带不带 Key"——
//
//	所以存一个 `KeyCarried bool`（是/否），**绝不存 Key 本身**。
//	索引文件（`data/navpacks/index.json`）躺在一个可能被备份/同步/误传的地方，
//	往里头写密钥是纯粹的自找麻烦 —— 取的时候开一下包就知道了。
type navPackArtifact struct {
	PackID     string `json:"pack_id"`
	Name       string `json:"name"`
	Note       string `json:"note"`
	Version    string `json:"version"`
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Buildings  int    `json:"buildings"`
	WithCoord  int    `json:"with_coord"`
	HasOrigin  bool   `json:"has_origin"`
	KeyCarried bool   `json:"key_carried"`
	// ★ v2：分层统计。管理台靠它回答"这个包标了分层没有 / 标了多少间"——
	//    没有这两个数就只能把包下下来解一遍，而管理台是**不该**碰包的。
	//    `Floors` 是**带分层的楼栋数**，不是楼层总数（后者没有单独的意义：
	//    "3 栋楼各 1 层"和"1 栋楼 3 层"对用户的含义完全不同，混在一起没信息）。
	Floors int `json:"floors"`
	Rooms  int `json:"rooms"`
	// FormatVersion 记下这个包是哪一个版本导出的 —— 不参与校验，
	// 纯粹是"事后排查"要用的（v1 包不可能有分层，看到 Floors>0 就是数据坏了）。
	FormatVersion int   `json:"format_version"`
	PublishedAt   int64 `json:"published_at"`
	UpdatedAt     int64 `json:"updated_at"`
	// Downloads 只是"有人拉过几回"的粗略计数，用来发现"这个包被疯传"的苗头。
	// 它是**尽力而为**的（写索引失败不该让下载失败），不是一个审计账本。
	Downloads int64 `json:"downloads"`
}

type navPackIndex struct {
	Packs map[string]*navPackArtifact `json:"packs"`
}

func (s *Server) navPacksDir() string { return filepath.Join(s.cfg.DataDir, "navpacks") }

func (s *Server) navPackIndexPath() string {
	return filepath.Join(s.navPacksDir(), "index.json")
}

func (s *Server) loadNavPackIndex() navPackIndex {
	idx := navPackIndex{Packs: map[string]*navPackArtifact{}}
	raw, err := os.ReadFile(s.navPackIndexPath())
	if err != nil {
		return idx
	}
	// ⚠️ 解析失败**当空索引**（同 `loadPluginIndex`）：索引坏了的结果应该是
	//    "管理台暂时是空的"，而不是"服务端起不来"。
	_ = json.Unmarshal(raw, &idx)
	if idx.Packs == nil {
		idx.Packs = map[string]*navPackArtifact{}
	}
	return idx
}

func (s *Server) saveNavPackIndex(idx navPackIndex) error {
	if err := os.MkdirAll(s.navPacksDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.navPackIndexPath(), raw, 0o644)
}

// validNavPackID 校验包标识（= 授权码）。
//
// ⚠️ 正则必须和这三处**逐字一致**：
//
//	· 电脑端工具   `nav-studio.js` 的 `/^[a-z][a-z0-9-]{1,47}$/`
//	· 格式文档     docs/格式-导航数据包-navpack-2026-10-12.md
//	· 插件（待做） 导入时要再判一次
//
// 为什么这么死板：packId 会变成**磁盘上的文件名**（`<packId>.json`）。
// 允许路径分隔符就等于允许 `../../etc/passwd` 这种"写文件到任意位置"——
// 而发布接口是管理员用的，让它变成一个远程任意写漏洞毫无必要。
// 手写逐字符判断（不用 regexp）是为了和服务端既有风格一致，也避免正则构造开销。
func validNavPackID(id string) bool {
	if len(id) < 2 || len(id) > 48 {
		return false
	}
	if id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

// parseNavPack 解析并校验一份数据包文件。
//
// 返回的第二个值是**文件原文**（原封不动存盘用）。
func parseNavPack(raw []byte) (*navPackDoc, error) {
	var d navPackDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("不是合法的 JSON：%v", err)
	}
	if d.Format != navPackFormatTag && d.Format != navPackFormatTagLegacy {
		return nil, fmt.Errorf("format 字段不是 %q（读到 %q）—— 这不是一个导航数据包",
			navPackFormatTag, d.Format)
	}
	// ⚠️ 版本：接受 [1, 2]，超出就拒。不要"尽力解析"——
	//    数据包是**分发的**，安静地做错事等于所有人一起错。
	if d.FormatVersion < minNavPackFormatVersion || d.FormatVersion > maxNavPackFormatVersion {
		return nil, fmt.Errorf(
			"数据包格式版本是 %d，服务端只认 %d~%d（请用配套版本的标注工具重新导出）",
			d.FormatVersion, minNavPackFormatVersion, maxNavPackFormatVersion)
	}
	if !validNavPackID(d.PackID) {
		return nil, fmt.Errorf("packId %q 不合法（要小写字母开头、a-z0-9- 组成、2~48 位）—— 它就是用户的授权码", d.PackID)
	}
	if strings.TrimSpace(d.Name) == "" {
		return nil, fmt.Errorf("缺 name —— 用户导入时看到的就是这个名字")
	}
	if strings.TrimSpace(d.Version) == "" {
		return nil, fmt.Errorf("缺 version —— 用户重复导入时靠它判断要不要覆盖")
	}
	return &d, nil
}

// navPackHasKey 探一下原文里有没有非空的 tencentKey。
//
// ⚠️ 走 jsonMap 而不是 navPackDoc.HasKey 字段：`HasKey` 那个字段在结构体上
//
//	是 `json:"-"`（我们不希望它进索引的 JSON），所以反序列化时它永远是空串 ——
//	拿它判断"带不带 Key"会**恒为 false**，症状是"管理台永远显示不带 Key"，
//	而包其实是带 Key 的。这正是"字段声明和实际用途对不上"的经典坑。
func navPackHasKey(raw []byte) bool {
	var m jsonMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return strings.TrimSpace(m.stringOf("tencentKey")) != ""
}

// navPackFileName 决定这个包在磁盘上叫什么。
//
// ⚠️ 只用 packId，**不带用户上传时的文件名**：那个名字可能是中文、
//
//	可能带空格、可能是另一个包的（用户改过名）。packId 已经唯一，
//	而且它已经过了 validNavPackID —— 物理上不可能带路径分隔符。
//
// 那"多版本"怎么办？**只保留最新一版**（新版覆盖旧版）。
// 理由：数据包是"一份当前的楼栋表"，不像代码需要回滚历史；
// 留一堆版本只会让"磁盘上到底哪个是当前生效的"变得说不清。
// 真要留档，管理员自己在本地留 —— 服务端的职责是"发当前那一份"。
func navPackFileName(packID string) string { return packID + ".json" }

// ---------------------------------------------------------------------------
// 用户侧：凭授权码取包
// ---------------------------------------------------------------------------

// navPackListItem 是给**用户端**（插件）的列表项：**不含 Key**。
type navPackListItem struct {
	PackID      string `json:"pack_id"`
	Name        string `json:"name"`
	Note        string `json:"note"`
	Version     string `json:"version"`
	Buildings   int    `json:"buildings"`
	WithCoord   int    `json:"with_coord"`
	KeyCarried  bool   `json:"key_carried"`
	UpdatedAt   int64  `json:"updated_at"`
	DownloadURL string `json:"download_url"`
}

// handleNavPacksList 列出服务器上**所有**可下载的数据包。
//
// ⚠️ 这里**不含地址或 Key** —— 只是"有哪些包"。用户在插件里输授权码，
//
//	或从这里挑一个。列表公开给**已登录用户**（ext）：数据包的名字
//	（"某某学校主校区"）不算秘密，而"知道有哪些包"是它可被发现的前提。
//
// ★ 但真正的**下载**仍然要 packId（那个才是授权码）。列表有名字 ≠ 能拿走文件。
//
//	这条分界是刻意的：让"我们做了这个功能"能被看见，而"拿走 Key"要凭码。
func (s *Server) handleNavPacksList(w http.ResponseWriter, r *http.Request) {
	idx := s.loadNavPackIndex()
	out := make([]navPackListItem, 0, len(idx.Packs))
	for _, a := range idx.Packs {
		if a == nil || a.PackID == "" {
			continue
		}
		out = append(out, navPackListItem{
			PackID:      a.PackID,
			Name:        a.Name,
			Note:        a.Note,
			Version:     a.Version,
			Buildings:   a.Buildings,
			WithCoord:   a.WithCoord,
			KeyCarried:  a.KeyCarried,
			UpdatedAt:   a.UpdatedAt,
			DownloadURL: s.navPackDownloadURLFor(a),
		})
	}
	// 排序放服务端：三个客户端各自排一遍必然漂。
	// 按更新时间倒序（新的在前）—— "最近我们更新过哪个包"是管理员最常问的，
	// 同时也是用户进"从云端导入"时最可能想找的那个。
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].PackID < out[j].PackID
	})
	writeJSON(w, 200, map[string]any{
		"packs":          out,
		"format":         navPackFormatTag,
		"format_version": navPackFormatVersion,
		"server_time":    time.Now().Unix(),
	})
}

// navPackDownloadURLFor 决定客户端从哪儿下这个包。
//
// 和 `pluginDownloadURLFor` 同一套规则（配了 COSBase 就走对象存储，否则本机）。
//
// ⚠️ 键的拼法必须和上传脚本**严格一致**（`<prefix>/navpacks/<文件名>`），
//
//	两边错一个字符就是 404，而客户端只会报一句笼统的"下载失败"。
func (s *Server) navPackDownloadURLFor(a *navPackArtifact) string {
	local := "/api/navpacks/download/" + a.PackID
	if a == nil || a.File == "" {
		return local
	}
	base := strings.TrimRight(strings.TrimSpace(s.cfg.COSBase), "/")
	if base == "" {
		return local
	}
	return base + "/navpacks/" + filepath.Base(a.File)
}

// handleNavPackDownload 凭授权码（= packId）取一份数据包。
//
//	GET /api/navpacks/download/{id}
//
// ⚠️ 走 `ext()`：要登录态或机器 Key，**不开匿名口子**。
//
//	理由写在文件头那段 —— 包里可能带明文 Key。
func (s *Server) handleNavPackDownload(w http.ResponseWriter, r *http.Request) {
	packID := strings.TrimSpace(r.PathValue("id"))
	if !validNavPackID(packID) {
		// 格式就不对：不要说"没有这个包"，那会让人以为输错了名字。
		apiErr(w, 400, "授权码格式不对（小写字母开头，只能有小写字母/数字/连字符）")
		return
	}
	idx := s.loadNavPackIndex()
	a := idx.Packs[packID]
	if a == nil {
		// ★ 这里一句话说清"该怎么办"，因为用户看到的只是一个 404。
		apiErr(w, 404, "没有这个授权码对应的数据包："+packID)
		return
	}
	full := filepath.Join(s.navPacksDir(), filepath.Base(a.File))
	f, err := os.Open(full)
	if err != nil {
		apiErr(w, 410, "数据包不在服务器上了："+err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, a.File))
	w.Header().Set("X-Pack-Id", a.PackID)
	w.Header().Set("X-Pack-Version", a.Version)
	w.Header().Set("X-SHA256", a.SHA256)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	http.ServeContent(w, r, a.File, st.ModTime(), f)

	// 计数**放在响应之后**：写索引失败不该让下载失败（用户已经拿到文件了）。
	// 它是"发现异常"的辅助，不是审计账本 —— 所以尽力而为就够了。
	s.bumpNavPackDownloads(packID)
}

// bumpNavPackDownloads 累加一次下载计数。失败只记日志。
func (s *Server) bumpNavPackDownloads(packID string) {
	idx := s.loadNavPackIndex()
	a := idx.Packs[packID]
	if a == nil {
		return
	}
	a.Downloads++
	if err := s.saveNavPackIndex(idx); err != nil {
		log.Printf("navpack: 下载计数写失败 %s: %v", packID, err)
	}
}

// ---------------------------------------------------------------------------
// 管理员侧：上传 / 列表 / 删除
// ---------------------------------------------------------------------------

// handleNavPackAdminList 给管理台的列表。**不含 Key**（索引里本来就没有）。
//
//	GET /api/admin/navpacks
func (s *Server) handleNavPackAdminList(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	idx := s.loadNavPackIndex()
	packs := make([]*navPackArtifact, 0, len(idx.Packs))
	for _, a := range idx.Packs {
		if a != nil && a.PackID != "" {
			packs = append(packs, a)
		}
	}
	sort.Slice(packs, func(i, j int) bool {
		if packs[i].UpdatedAt != packs[j].UpdatedAt {
			return packs[i].UpdatedAt > packs[j].UpdatedAt
		}
		return packs[i].PackID < packs[j].PackID
	})

	// 磁盘上有什么也给出来 —— 用来发现"文件在了但索引没记"这种情况
	// （那是"传上去了但用户下不到"，只有对着两边看才发现）。
	files := []map[string]any{}
	entries, _ := os.ReadDir(s.navPacksDir())
	for _, e := range entries {
		if e.IsDir() || e.Name() == "index.json" {
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
		"packs":          packs,
		"files":          files,
		"dir":            s.navPacksDir(),
		"format":         navPackFormatTag,
		"format_version": navPackFormatVersion,
		// ★ 把"一个包对应一个授权码"这句话**从服务端下发**，而不是在界面上写死 ——
		//   将来如果要改成"码和包分开"，只要改这一处。
		"auth_note": "授权码就是包标识（pack_id）本身。用户拿这个码在手机里一键导入。",
	})
}

// handleNavPackUpload 收一份数据包。管理员 only。
//
//	POST /api/admin/navpacks/upload
//	body = .navpack.json 原文（raw，不是 multipart）
//	query: replace=1 允许覆盖已存在的包（不带时，撞包名会拒）
//
// 为什么 raw 而不是 multipart：唯一的调用方是管理台或我们的出包脚本，
// 不值得引入一个 multipart 解析器（同 plugins.go 的理由）。
//
// ★ 元数据**一律从文件里读**，接口不收 name / version / 楼栋数。
func (s *Server) handleNavPackUpload(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	replace := strings.TrimSpace(r.URL.Query().Get("replace")) == "1"

	if err := os.MkdirAll(s.navPacksDir(), 0o755); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	// 先读进内存：数据包是纯文本、有 4MB 上限兜底（一个几千栋楼的表也就几百 KB）。
	// ⚠️ 用 LimitReader 而不是无上限 io.Copy：一个 200MB 的"包"能把这台机器
	//    的内存吃掉，而它本来就该被拒。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		apiErr(w, 500, "读取失败："+err.Error())
		return
	}
	if len(raw) == 0 {
		apiErr(w, 400, "body 是空的 —— 请把 .navpack.json 的内容原样 PUT 上来")
		return
	}
	if len(raw) >= 4<<20 {
		apiErr(w, 413, "文件超过 4MB —— 数据包不该这么大（是不是贴错了别的东西？）")
		return
	}

	doc, err := parseNavPack(raw)
	if err != nil {
		apiErr(w, 400, err.Error())
		return
	}

	// ---- 撞包检查 ----
	//
	// ⚠️ 默认**拒掉**同名包，要覆盖必须显式 `?replace=1`。
	//    理由：packId 同时是用户的授权码 —— 换掉同一个 packId 的内容，
	//    等于**所有已经拿到这个码的人下次导入就会换掉他们本地的楼栋表**。
	//    那可能正是我们要的（改错了要修正），但绝不能是"手滑传错了文件"的结果。
	idx := s.loadNavPackIndex()
	prev := idx.Packs[doc.PackID]
	if prev != nil && !replace {
		apiErrDetail(w, http.StatusConflict, "pack_exists",
			fmt.Sprintf("已经有一个叫 %s 的包了（%s / v%s）。换掉它 = 所有拿这个码的人下次导入都会跟着换，所以请确认。",
				doc.PackID, prev.Name, prev.Version),
			map[string]any{"prev": prev})
		return
	}

	// ---- 落盘 ----
	//
	// ⚠️ 顺序和 plugins.go 一样：**先写临时文件、校验通过才改名**。
	//    直接按 packId 命名再校验的话，一个坏包会**原地顶掉**一个好包 ——
	//    索引里还指着旧版本，磁盘上已经坏了。
	fileName := navPackFileName(doc.PackID)
	dst := filepath.Join(s.navPacksDir(), fileName)
	tmp, err := os.CreateTemp(s.navPacksDir(), "upload-*.json.part")
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		apiErr(w, 500, "写入失败："+err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		apiErr(w, 500, "写入失败："+err.Error())
		return
	}
	// Windows 上 `os.Rename` 到已存在的文件会失败 → 先删目标。
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmpName)
		apiErr(w, 500, "替换旧包失败："+err.Error())
		return
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		apiErr(w, 500, "落地失败："+err.Error())
		return
	}

	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])

	layered, rooms := navPackCountLayering(doc)
	art := &navPackArtifact{
		PackID:        doc.PackID,
		Name:          doc.Name,
		Note:          doc.Note,
		Version:       doc.Version,
		File:          fileName,
		SHA256:        sha,
		Size:          int64(len(raw)),
		Buildings:     len(doc.Buildings),
		WithCoord:     navPackCountWithCoord(doc),
		HasOrigin:     doc.Origin != nil && (doc.Origin.Lat != 0 || doc.Origin.Lng != 0),
		KeyCarried:    navPackHasKey(raw),
		Floors:        layered,
		Rooms:         rooms,
		FormatVersion: doc.FormatVersion,
		PublishedAt:   time.Now().Unix(),
		UpdatedAt:     doc.UpdatedAt,
		Downloads:     prevDownloads(prev),
	}
	if art.UpdatedAt == 0 {
		// 文件里没写就补一个当前时间 —— 列表按它排序，0 会让它沉到底下。
		art.UpdatedAt = art.PublishedAt
	}

	idx.Packs[art.PackID] = art
	if err := s.saveNavPackIndex(idx); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	replaced := ""
	if prev != nil {
		replaced = fmt.Sprintf("（替换 %s / v%s）", prev.Name, prev.Version)
	}
	log.Printf("navpack published: %s v%s (fv%d, %d bytes, %d buildings, %d layered, %d rooms, key=%v) %s",
		art.PackID, art.Version, art.FormatVersion, art.Size, art.Buildings,
		art.Floors, art.Rooms, art.KeyCarried, replaced)
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"pack":     art,
		"replaced": prev,
		"file":     fileName,
		// ★ 把授权码再回一句：管理台要把它显示给管理员（那就是分发时给用户的东西）。
		"auth_code": art.PackID,
		"download":  s.navPackDownloadURLFor(art),
	})
}

// handleNavPackDelete 删掉一个包。
//
//	DELETE /api/admin/navpacks/{id}
//
// ⚠️ 删了之后**所有拿这个码的人都下不到了** —— 但**已经下到手机里的那份不受影响**
//
//	（那是人家本地的数据）。这个语义要在界面上说清楚，否则管理员会以为
//	"删了就收回了"。真要收回，正确做法是吊销那个腾讯 Key。
func (s *Server) handleNavPackDelete(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	packID := strings.TrimSpace(r.PathValue("id"))
	if !validNavPackID(packID) {
		apiErr(w, 400, "授权码格式不对")
		return
	}
	idx := s.loadNavPackIndex()
	a := idx.Packs[packID]
	if a == nil {
		apiErr(w, 404, "没有这个包："+packID)
		return
	}
	if err := os.Remove(filepath.Join(s.navPacksDir(), filepath.Base(a.File))); err != nil && !os.IsNotExist(err) {
		apiErr(w, 500, "删文件失败："+err.Error())
		return
	}
	delete(idx.Packs, packID)
	if err := s.saveNavPackIndex(idx); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	log.Printf("navpack deleted: %s (%s)", packID, a.Name)
	writeJSON(w, 200, map[string]any{"ok": true, "pack_id": packID})
}

// handleNavPackAdminView 看**某一个包的内容**（管理台预览用）。
//
//	GET /api/admin/navpacks/{id}/view
//
// ★ 这里**会把原文里的 Key 掩码后给出来**（不是原文）—— 管理员需要确认
//
//	"这个包到底带没带 Key、带的是不是我以为的那个"，但没有任何理由
//	把密钥原文再吐回浏览器一次（那会进浏览器历史、devtools、剪贴板…）。
func (s *Server) handleNavPackAdminView(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	packID := strings.TrimSpace(r.PathValue("id"))
	if !validNavPackID(packID) {
		apiErr(w, 400, "授权码格式不对")
		return
	}
	idx := s.loadNavPackIndex()
	a := idx.Packs[packID]
	if a == nil {
		apiErr(w, 404, "没有这个包："+packID)
		return
	}
	raw, err := os.ReadFile(filepath.Join(s.navPacksDir(), filepath.Base(a.File)))
	if err != nil {
		apiErr(w, 410, "文件不在了："+err.Error())
		return
	}
	var doc navPackDoc
	_ = json.Unmarshal(raw, &doc)

	var m jsonMap
	_ = json.Unmarshal(raw, &m)

	// 楼栋按名字列出来（管理台要能一眼看出"这个包是不是我传的那份"）。
	type bBrief struct {
		ID    string   `json:"id"`
		Name  string   `json:"name"`
		Lat   float64  `json:"lat"`
		Lng   float64  `json:"lng"`
		Alias []string `json:"alias"`
		// ★ v2：分类 + 分层概况。
		// ⚠️ 这里只回**数量**，不回房间坐标 —— 管理台要回答的是
		//    "这个包标到什么程度"，不是"101 在哪"。多回一份坐标只是
		//    把响应体撑大，而且还得在界面上画一遍（那是标注台的活）。
		Type   string `json:"type"`
		Floors int    `json:"floors"`
		Rooms  int    `json:"rooms"`
	}
	buildings := []bBrief{}
	if arr, ok := m["buildings"]; ok {
		if list, ok := arr.([]any); ok {
			for _, it := range list {
				bm, ok := it.(map[string]any)
				if !ok {
					continue
				}
				var alias []string
				if av, ok := bm["alias"].([]any); ok {
					for _, x := range av {
						if s, ok := x.(string); ok {
							alias = append(alias, s)
						}
					}
				}
				n := navPackNum(bm["lat"])
				l := navPackNum(bm["lng"])
				// 楼层数 / 房间数：结构不认识的（比如 `floors` 不是数组）
				// 一律当 0 —— 详情页是**只读展示**，不值得为畸形数据报错。
				nf, nr := 0, 0
				if fa, ok := bm["floors"].([]any); ok {
					nf = len(fa)
					for _, fv := range fa {
						fm, ok := fv.(map[string]any)
						if !ok {
							continue
						}
						if ra, ok := fm["rooms"].([]any); ok {
							nr += len(ra)
						}
					}
				}
				buildings = append(buildings, bBrief{
					ID: navPackStr(bm["id"]), Name: navPackStr(bm["name"]),
					Lat: n, Lng: l, Alias: alias,
					Type: navPackStr(bm["type"]), Floors: nf, Rooms: nr,
				})
			}
		}
	}

	resp := map[string]any{
		"pack":      a,
		"buildings": buildings,
		"origin":    m["origin"],
		"note":      doc.Note,
		"auth_code": a.PackID,
	}
	// ★ 只回掩码。`tencentKey` 原文**永不**离开服务端。
	if k := strings.TrimSpace(m.stringOf("tencentKey")); k != "" {
		resp["tencent_key_masked"] = maskSecret(k)
		resp["referer"] = m.stringOf("referer")
	}
	writeJSON(w, 200, resp)
}

// maskSecret 把一个密钥掩成"够认出来、不够用"的样子。
//
// 长这样：`TW5BZ-****-****-ITBQH`。前 5 后 5 —— 足够管理员认出"是不是我那个"，
// 中间全抹掉。**不要**只留后四位：腾讯的 Key 是分组连字符格式，
// 只留尾四位时两个不同的 Key 长得几乎一样。
func maskSecret(s string) string {
	if len(s) <= 12 {
		return "****"
	}
	return s[:5] + "****" + s[len(s)-5:]
}

// navPackCountWithCoord 数一下有多少栋楼是有坐标的。
// "有坐标" = 不是 (0,0)（和插件侧 `nsHasCoord` 同一条判据）。
func navPackCountWithCoord(d *navPackDoc) int {
	n := 0
	for _, b := range d.Buildings {
		if b.Lat != 0 || b.Lng != 0 {
			n++
		}
	}
	return n
}

func prevDownloads(p *navPackArtifact) int64 {
	if p == nil {
		return 0
	}
	return p.Downloads
}

// navPackCountLayering 数"带分层的楼栋数"和"房间总数"。
//
// ⚠️ 第一个返回的是**楼栋数**，不是楼层总数。
//
//	原因："3 栋楼各 1 层"和"1 栋楼 3 层"在管理台上应该长得不一样 ——
//	前者是全校都标了、后者是只标了一栋。合成一个"楼层数 = 3"就没信息量了。
//
// v1 包里没有 `floors`，解出来是 nil 切片 → 两个数都是 0，正是"没标分层"。
func navPackCountLayering(d *navPackDoc) (layeredBuildings int, rooms int) {
	for _, b := range d.Buildings {
		if len(b.Floors) == 0 {
			continue
		}
		layeredBuildings++
		for _, f := range b.Floors {
			rooms += len(f.Rooms)
		}
	}
	return layeredBuildings, rooms
}

// navPackStr / navPackNum 从 any 里安全取值（JSON 解出来是 any）。
func navPackStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func navPackNum(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}
