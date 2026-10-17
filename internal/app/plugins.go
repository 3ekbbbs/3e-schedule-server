package app

import (
	"archive/zip"
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
// 插件分发（2026-10-08，批次 5）。
//
// ## 和热更新（`updates.go`）是两件事，没有合并
//
//	热更新   平台（android / windows）→ 一个产物，"最新的那个"
//	插件分发 插件 id                    → 一个产物，"我们自己出的一个功能包"
//
// 为什么不能复用同一张表：热更新是"**最新即唯一**"（发布新版本就顶掉旧的），
// 插件是"**每个 id 各自一条线**"（日历和随心记各升各的，互不影响）。
// 硬塞进一张表会出现"发了个插件包，结果客户端的更新检查说该升级了"。
//
// ## ★ 唯一的真相是插件包自己
//
// 发布时**不接受调用方传 id / name / version / 能力清单** —— 一律由服务端
// 用标准库 `archive/zip` 打开 apk、读里头那份 `assets/threees-plugin.json`
// 得到（apk 本来就是 zip，不需要任何第三方库）。
//
// 为什么这么较真：如果让出包脚本把清单当参数一起传，那么"包里写的"和
// "服务端记的"就成了**两份数据**，而它们中间**没有任何机制保证一致**。
// 症状会是"商店里写着 v1.2.0、装完一看是 v1.1.0"或者"商店说这插件有月历、
// 装上却没有"这类没人能一眼看穿的问题，而且只会在某个包上偶发。
// 让服务端自己开包读，两边在物理上就不可能不一致。
//
// ⚠️ 于是发布接口**只收文件本体** + 备注。要改能力清单 = 重新出包，没有第二条路。
//
// ## 下线（delist）刻意不做
//
// 索引里没有的插件，客户端商店就不显示；**已经装了的照旧用**。
// 这正是我们要的语义：撤掉一个包不该让用户手里的功能突然消失。
// 真要让某个插件消失，正确做法是发一个"什么也不做"的新版本，而不是从索引里抹掉。
// ---------------------------------------------------------------------------

// pluginPageBrief 是"这一页叫什么、里头有几个视图" —— 商店里要显示给用户看的东西。
//
// ⚠️ 只挑**发布页要显示的**字段，刻意不复刻整份清单。
//
//	复刻整份 = 服务端也维护一份插件契约 = 契约加字段时服务端跟着漂，
//	而服务端**根本不执行插件**，它多知道一个字都是负担。
type pluginPageBrief struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
	Views int    `json:"views"`
}

type pluginNamedBrief struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// pluginManifestProbe 是从包内清单里挑出来的那几个字段。
type pluginManifestProbe struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Icon       string `json:"icon"`
	Desc       string `json:"desc"`
	Version    string `json:"version"`
	APIVersion int    `json:"apiVersion"`
	Pages      []struct {
		ID    string   `json:"id"`
		Label string   `json:"label"`
		Icon  string   `json:"icon"`
		Views []string `json:"views"`
	} `json:"pages"`
	Widgets []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"widgets"`
	AI struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	} `json:"ai"`
	Services []struct {
		Name string `json:"name"`
	} `json:"services"`
	Permissions []string `json:"permissions"`
}

// pluginArtifact 是索引里的一条。字段名与客户端 `RemotePlugin` 一一对应。
type pluginArtifact struct {
	ID string `json:"id"`
	// Platform 是这条属于哪个端（android / windows）。
	//
	// ⚠️ 2026-10-15 加：**同一个 id 在两个平台上各有一份**（`notes`、`smartnotes`
	//    就是），所以"id → 那一条"不再唯一，索引的键改成了 `<platform>:<id>`。
	//    老索引里的键没有冒号，读的时候按 android 迁进来（幂等）。
	//
	// 客户端读到这个字段是无害的（它按 id 找自己的插件），所以没有理由不给它。
	Platform   string `json:"platform,omitempty"`
	Name       string `json:"name"`
	Icon       string `json:"icon"`
	Desc       string `json:"desc"`
	Version    string `json:"version"`
	APIVersion int    `json:"api_version"`
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Notes      string `json:"notes"`
	// Order 是商店里的排序权重，**越小越靠前**，默认 100。
	// 存在的理由：按 id 字母序排的话，将来加一个插件就会被塞到某个说不清的位置上；
	// 给一个显式的权重，"我们想让人先看到哪个"才是一句能表达出来的话。
	Order       int                `json:"order"`
	Pages       []pluginPageBrief  `json:"pages"`
	Widgets     []pluginNamedBrief `json:"widgets"`
	AiTools     []string           `json:"ai_tools"`
	Services    []string           `json:"services"`
	PublishedAt int64              `json:"published_at"`
}

// pluginIndex 是索引文件（`data/plugins/manifest.json`）。
// 用 map 而不是数组：id → 那一条，天然就是"一个 id 只能有一条"。
type pluginIndex struct {
	Plugins map[string]*pluginArtifact `json:"plugins"`
}

// 插件清单在包内的位置。
//
// 首选中性名 `schedule-plugin.json`；同时接受历史包用的 `threees-plugin.json`，
// 否则已经发出去的插件包会在升级服务端后全部变成“不是插件包”。
const pluginManifestAsset = "assets/schedule-plugin.json"

// pluginManifestAssetDesktop 是**电脑端**插件 zip 里清单的位置（包根）。
//
// ⚠️ 两个平台的位置**不同**，这不是笔误：
//
//	android apk：清单要进 `assets/`，因为它跟着 apk 的资产目录走；
//	windows zip：解包后就是一个目录，清单放根上（`plugin-host.js` 从这里读）。
//
// 整合到同一个分发口之后，服务端必须**两个都认** —— 只认一个的话，
// 另一个平台上传的包会被判成"这不是一个插件包"，而错误信息看着像包坏了。
const pluginManifestAssetDesktop = "schedule-plugin.json"

// 历史兼容名（客户端旧版本打出来的包）。
const (
	pluginManifestAssetLegacy        = "assets/threees-plugin.json"
	pluginManifestAssetDesktopLegacy = "threees-plugin.json"
)

func (s *Server) pluginsDir() string { return filepath.Join(s.cfg.DataDir, "plugins") }

func (s *Server) pluginIndexPath() string {
	return filepath.Join(s.pluginsDir(), "manifest.json")
}

func (s *Server) loadPluginIndex() pluginIndex {
	idx := pluginIndex{Plugins: map[string]*pluginArtifact{}}
	raw, err := os.ReadFile(s.pluginIndexPath())
	if err != nil {
		return idx
	}
	// ⚠️ 解析失败**当空索引**，不抛：索引文件坏了的结果应该是"商店暂时是空的"，
	//    而不是"服务端起不来"。同 `Pages.loadHidden` 那条外部输入的铁律。
	_ = json.Unmarshal(raw, &idx)
	if idx.Plugins == nil {
		idx.Plugins = map[string]*pluginArtifact{}
	}
	// ★ 2026-10-15：键从 `<id>` 改成 `<platform>:<id>`。老文件里没有冒号，
	//   在这里一次性按 android 迁进来（幂等：已经是新格式的原样保留）。
	//
	// ⚠️ 重建一个新 map，**不要在 range 里增删同一个 map** ——
	//    那个语义是"可能改到、可能没改到"，比 panic 还难查。
	fixed := make(map[string]*pluginArtifact, len(idx.Plugins))
	for k, a := range idx.Plugins {
		if a == nil {
			continue
		}
		if strings.Contains(k, ":") {
			fixed[k] = a
			continue
		}
		// 老键 = 只有 id，一律是 android（那时候还没有电脑端插件分发）
		a.Platform = pluginPlatformAndroid
		fixed[pluginKey(pluginPlatformAndroid, a.ID)] = a
	}
	idx.Plugins = fixed
	return idx
}

func (s *Server) savePluginIndex(idx pluginIndex) error {
	if err := os.MkdirAll(s.pluginsDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.pluginIndexPath(), raw, 0o644)
}

// readPluginManifest 打开插件包，读里头那份清单。
//
// apk = zip，所以标准库就够。走的是"直接开 zip 读文件"，**不碰
// `PackageManager` / `AssetManager`** —— 后者要先把一个没装过的 apk 包装成
// `ApplicationInfo` 骗过资源系统，任何一步在将来变了行为，症状都是
// "读不到清单"，而且看不出是框架变了还是包坏了。客户端那边
// （`PluginRepo.readManifest`）用的是同一个思路、同一个路径。
func readPluginManifest(apkPath string) (*pluginManifestProbe, error) {
	zr, err := zip.OpenReader(apkPath)
	if err != nil {
		return nil, fmt.Errorf("这个文件不是 zip/apk：%v", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		// ⚠️ 用 filepath.ToSlash 归一：zip 里理论上都是 `/`，但万一是自己用
		//    工具造出来的包，`\` 会让这个比较永远不成立 —— 而症状同样是
		//    "读不到清单"，排查成本极高。
		//
		// ⚠️ 同时认两个位置（见 pluginManifestAssetDesktop 的说明）。
		//    一个包里两个都有时**先命中哪个用哪个** —— 那种包本身就是坏的，
		//    不值得为它加一层"该以哪个为准"的判断。
		name := filepath.ToSlash(f.Name)
		if name != pluginManifestAsset && name != pluginManifestAssetDesktop &&
			name != pluginManifestAssetLegacy && name != pluginManifestAssetDesktopLegacy {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("打不开包里的清单：%v", err)
		}
		defer rc.Close()
		raw, err := io.ReadAll(io.LimitReader(rc, 4<<20)) // 4MB 足够，防炸
		if err != nil {
			return nil, fmt.Errorf("读清单失败：%v", err)
		}
		var m pluginManifestProbe
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("包里的清单不是合法 JSON：%v", err)
		}
		return &m, nil
	}
	return nil, fmt.Errorf("这个包里没有 %s（电脑端是包根的 %s）—— 它不是插件包（普通 apk/zip 没有这个文件）",
		pluginManifestAsset, pluginManifestAssetDesktop)
}

// validatePluginProbe 把客户端那套"必需字段"的规矩在服务端也过一遍。
//
// ⚠️ 这不是重复劳动，而是**把失败挪到发布这一刻**：这些判据客户端也有一份
//
//	（`PluginManifest.parse`），但那是"装的时候才拒"。发布时拒掉，
//	我们能立刻知道，而不是等用户点了安装才发现这个包做坏了。
//	两边判据故意保持**同一句话**，将来改的时候一眼能看出是一对。
// pluginAllowedPageIDs 是**允许出现在插件清单里的页面 id**（手机端 ∪ 电脑端）。
//
// ⚠️ 2026-10-16 加（第三轮审计 M7）：`validatePluginProbe` 原来只验
//    id / name / version / apiVersion，**不验 `pages[].id`** —— 于是经
//    `POST /api/plugins/submit` 上传的插件可以自造任意页面 id 进市场目录。
//    真正的拦截只剩客户端 `PluginLoader` 的白名单（那个是有效的），
//    表现成"市场里出现装得上、点进去白屏的包"。
//
// ## 为什么手机端和电脑端的 id 合成一份
//
// 两边的清单**不重合**，差异是实打实的：
//   · 局域网同步在手机端叫 `lanlink`、在电脑端叫 `lansync`（两个插件、两个页面）；
//   · `teams`（团队面板）只有电脑端有那一页。
//
// 理论上可以按 platform 分两份，但这里**故意不分**：服务端这道闸门的目的是
// "拦住自造的页面 id"，合并之后的精度完全够用 —— 代价只是"手机端插件声明
// `teams` 时服务端会放行（而客户端仍会拒载）"，而那种包不会出自我们自己的构建。
// 少一份镜像常量 = 少一处会漂的地方。
//
// ★ 实测教训：第一次只列了手机端那份，结果发 `lansync` 时被**自己的新校验**拦下
//   （报"页面 id 是跨端冻结的契约，不能自造"，而它明明是合法的电脑端页面）。
//
// 为什么不退而求其次只做"形状校验"：形状拦得住 `../../` 那类，
// 拦不住"自造一个形状合法的 id" —— 而后者才是这个缺口的真正内容。
var pluginAllowedPageIDs = map[string]bool{
	// 跨端冻结（两端都有这一页）
	"schedule": true, "calendars": true, "inbox": true, "ai": true, "settings": true,
	// 手机端独有（数据只在这台手机上，不进跨端清单）
	"notes": true, "mdnote": true, "plot": true, "mdnotelive": true,
	"wordbook": true, "nav": true, "calculator": true, "lanlink": true,
	// 电脑端独有（手机端没有这两页）
	"teams": true, "lansync": true,
}

func validatePluginProbe(m *pluginManifestProbe) error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("包里的清单缺 id")
	}
	// 和客户端 `PluginManifest.parse` 的正则**逐字一致**：
	// id 要拿来拼目录名和默认服务名前缀，不许带路径分隔符和空格。
	ok := false
	if len(m.ID) >= 2 && len(m.ID) <= 32 && m.ID[0] >= 'a' && m.ID[0] <= 'z' {
		ok = true
		for i := 0; i < len(m.ID); i++ {
			c := m.ID[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
				continue
			}
			ok = false
			break
		}
	}
	if !ok {
		return fmt.Errorf("插件 id %q 不合法（只允许小写字母开头、a-z0-9_- 组成、2~32 位）", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("包里的清单缺 name")
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("包里的清单缺 version")
	}
	if m.APIVersion <= 0 {
		return fmt.Errorf("包里的清单缺 apiVersion（当前契约是 %d）", pluginAPIVersion)
	}

	// pages[].id 必须在允许的清单里（见 pluginAllowedPageIDs 的注释）。
	// 挡的是"市场里出现装得上、点进去白屏的包"：客户端也会拦，但拦在那里
	// 意味着**用户点安装之后**才发现；拦在这里，出包的人当场就知道。
	for i, p := range m.Pages {
		id := strings.TrimSpace(p.ID)
		if id == "" {
			return fmt.Errorf("包的 pages[%d] 缺 id", i)
		}
		if !pluginAllowedPageIDs[id] {
			return fmt.Errorf(
				"包的 pages[%d] 声明了未知的页面 id %q —— 页面 id 是跨端冻结的契约，不能自造",
				i, p.ID)
		}
	}
	return nil
}

// 当前插件契约版本。**必须和客户端 `PluginManifest.API_VERSION` 相等。**
//
// ⚠️ 这个数字只在**发布时**用来提示"你出的包和现在的契约对不上"，
//
//	真正拦不拦由客户端说了算（它才是加载插件的那一方）。
//	服务端**不做**版本裁决 —— 那会变成"服务端说能装、客户端说不能装"。
const pluginAPIVersion = 1

// pluginListItem 是商店列表里的一条：索引里那一条 + 一个下载地址。
//
// ⚠️ 用**内嵌**，不要手写一个 `map[string]any` 把每个字段再抄一遍。
// 抄一遍的代价是字段名有了**两份来源**（结构体的 json tag 和 map 的键），
// 而它们中间没有任何机制保证一致 —— 改一处忘一处的表现是
// "线上格式悄悄变回旧名字"：编译过、日志正常，只有一个客户端某栏永远是空的。
// 这和本文件开头那段"唯一真相是插件包自己"是同一个道理，
// 只是对象从"包的元数据"换成了"响应体的键名"。
//
// （`updates.go` 的 `handleUpdateCheck` 用的是手写 map，那边的理由是有几个键
// **刻意不同名** —— 比如 `latest.version` 对应的其实是 `art.Version`。这里没有那个理由。）
type pluginListItem struct {
	*pluginArtifact
	DownloadURL string `json:"download_url"`
}

// handlePluginsList 给客户端的**可安装插件清单**。
//
// 已登录（或带机器 Key）就能看 —— 商店本身就是登录之后才到得了的地方。
//
// ⚠️ 这里**只列我们发布的**，不列"服务器磁盘上恰好有哪些文件"：
//
//	索引才是"对外提供什么"的唯一说法（同 `handleUpdateManifest` 那条分界）。
func (s *Server) handlePluginsList(w http.ResponseWriter, r *http.Request) {
	// ★ 平台过滤（2026-10-15）：索引里现在同时有 android 和 windows 的条目，
	//   不过滤的话手机端商店里会出现一堆装不上的电脑端插件。
	//   默认 android —— 这条路径的历史调用方全是手机端（它们不带参数）。
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
		out = append(out, pluginListItem{pluginArtifact: a, DownloadURL: s.pluginDownloadURLFor(a)})
	}
	// 排序放在服务端做：三个客户端（手机/电脑/网页）将来各自排一遍，
	// 排法几乎一定会漂。**顺序是"我们想让人先看到哪个"，那是发布方的决定。**
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		// 同权重时按 id —— 一定要有一个能兜底的确定性判据，
		// 否则两次请求可能给出不同顺序（map 遍历本来就是随机的）。
		return out[i].ID < out[j].ID
	})
	writeJSON(w, 200, map[string]any{
		"plugins":     out,
		"platform":    platform,
		"server_time": time.Now().Unix(),
		"api_version": pluginAPIVersion,
		// ★ 2026-10-15：客户端"打开网页版市场"那个按钮的地址**由服务端给**。
		//
		// 以前它写死在客户端里（`https://example.com/plugins.html`）——
		// 那等于把"我们这个域名"编进了每一个安装包，和"个人备案应用不依赖
		// 自己的云服务"这条线冲突。留空 = 客户端不显示那个按钮（用内置商店）。
		"market_url": strings.TrimSpace(s.cfg.MarketURL),
	})
}

// ---------------------------------------------------------------------------
// 「官网插件市场页」为什么不走服务端接口（2026-10-08 定）
//
// 官网（`site/`，443）自己有两条**同域**接口 `/api/site` 与 `/api/downloads`，
// 而插件索引在主服务端（8443）。让官网页面直接 fetch 8443 会有两个现实问题：
//
//	① 跨域（不同端口 = 不同 origin）→ 得给一条匿名接口开 CORS；
//	② 有些网络只放 443，8443 直接被拦 → 市场页空白，而用户会以为我们没有插件。
//
// 所以市场页改用**静态 JSON**（`site/public/plugins.json`），由
// `scripts/gen-plugin-market.mjs` 从各插件源码里的 `threees-plugin.json` 生成：
//
//	· 数据来源仍然是**插件包本身**（和本文件开头那句"唯一真相是插件包自己"同一个道理）；
//	· 页面**零运行时依赖**（与 `site.js` 那句"官网的可用性不依赖脚本"是同一条纪律）；
//	· 代价是"改了插件忘了重生成" —— 那由闸门 `plugin-market-check.cjs` 盯着，
//	  它会比对两边并报红（红线 103/104/145：新形状要立成防线）。
//
// ⚠️ 所以这里**故意没有** `handlePluginsPublic` —— 不是漏了，是这个决定的结果。
//    客户端要的那份清单走 [handlePluginsList]（要凭证），那条一直是实时的。
// ---------------------------------------------------------------------------

// pluginDownloadURLFor 决定客户端从哪儿下这个插件包。
//
// 和 `downloadURLFor` 是同一套规则（配了 `-cos-base` 就走对象存储，否则回落本机），
// 只是平台那一段固定是 `plugins`。
//
// ⚠️ 键的拼法必须和发布脚本 `scripts/lib/cos.mjs` 的 `cosKeyFor(cfg, "plugins", 文件名)`
//
//	严格一致 —— 两边错一个字符就是 404，而客户端只会报一句笼统的"下载失败"。
func (s *Server) pluginDownloadURLFor(a *pluginArtifact) string {
	local := "/api/plugins/download/" + a.ID
	if a == nil || a.File == "" {
		return local
	}
	base := strings.TrimRight(strings.TrimSpace(s.cfg.COSBase), "/")
	if base == "" {
		return local
	}
	return base + "/plugins/" + filepath.Base(a.File)
}

// handlePluginDownload 把某个插件包流出去。
//
// ★ 2026-10-15：路径参数是 **id**（不是 `<platform>:<id>`），因为那是已发布
//   客户端的契约，不能改。解析顺序：显式 `?platform=` → android → 任意平台。
//
//	`?platform=` 优先，是为了"两个平台同名"时能指名道姓（`notes` 就是这样）。
//	不传时先找 android —— 历史上所有调用方都是手机端。
func (s *Server) handlePluginDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apiErr(w, 400, "缺少插件 id")
		return
	}
	idx := s.loadPluginIndex()
	var a *pluginArtifact
	if p := strings.TrimSpace(r.URL.Query().Get("platform")); p != "" {
		a = idx.Plugins[pluginKey(normalizePlatform(p), id)]
	}
	if a == nil {
		a = idx.Plugins[pluginKey(pluginPlatformAndroid, id)]
	}
	if a == nil {
		// 兜底：id 唯一时（另一个平台没有同名插件）也应该能下到。
		for _, cand := range idx.Plugins {
			if cand != nil && cand.ID == id {
				a = cand
				break
			}
		}
	}
	if a == nil {
		apiErr(w, 404, "服务器上没有这个插件："+id)
		return
	}
	s.servePluginFile(w, r, a, "attachment")
}

// handlePluginPublish 收一个插件包。管理员only。
//
//	POST /api/plugins/publish?notes=...&order=10&platform=android
//
// body 就是包本体（raw，不是 multipart）—— 和热更新同一个理由：
// 唯一的调用方是我们自己的出包脚本，不值得引入一个 multipart 解析器。
//
// ★ **其余信息一律从包里读**（见文件头那段）。这里能传的只有三样：
//
//	给人看的备注、商店排序权重、以及**平台**（android / windows）。
//	前两样都不在包契约里；平台**在**包外 —— 同一个 apk 结构与 zip 结构不同，
//	服务端没法从内容上可靠地判断，所以必须由发布方说明（默认 android）。
//
// ⚠️ 这条路径**跳过审核队列**（见 market.go 顶部那段）：它是给我们自己的
//    出包脚本用的。用户上传走 `POST /api/plugins/submit`。
func (s *Server) handlePluginPublish(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(userIDOf(r)) {
		apiErr(w, 403, "只有管理员能发布插件")
		return
	}
	q := r.URL.Query()
	platform := normalizePlatform(q.Get("platform"))

	if err := os.MkdirAll(s.pluginsDir(), 0o755); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	// 先落到一个**临时文件名**，读得出清单、校验通过才改名成正式名字。
	//
	// ⚠️ 顺序不能反（先按 id 命名再校验）：那样一个坏包会**原地顶掉**
	//    一个好好的旧包 —— 索引里还指着旧版本，磁盘上却已经是新的坏包，
	//    表现是"商店显示 v1.0.0，下下来的是个装不上的东西"。
	tmp, err := os.CreateTemp(s.pluginsDir(), "upload-*.apk.part")
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	tmpName := tmp.Name()
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, h), r.Body)
	closeErr := tmp.Close()
	if err != nil {
		_ = os.Remove(tmpName)
		apiErr(w, 500, "写入失败："+err.Error())
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmpName)
		apiErr(w, 500, "写入失败："+closeErr.Error())
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))

	probe, err := readPluginManifest(tmpName)
	if err != nil {
		_ = os.Remove(tmpName)
		apiErr(w, 400, err.Error())
		return
	}
	if err := validatePluginProbe(probe); err != nil {
		_ = os.Remove(tmpName)
		apiErr(w, 400, err.Error())
		return
	}
	if probe.APIVersion != pluginAPIVersion {
		// 只警告不拒绝：真正拦不拦是客户端的事（它才是加载插件的那一方）。
		// 但一定要**说出来** —— 契约对不上是"装上去也用不了"，静默接受最难查。
		log.Printf("plugin publish: %s 的 apiVersion=%d，服务端当前是 %d —— 客户端可能拒载",
			probe.ID, probe.APIVersion, pluginAPIVersion)
	}

	// 正式文件名：`<平台>-<id>-<版本><后缀>`。
	//
	// ⚠️ 故意**不带中文**、也**不用上传时的本地文件名**：这个名字会进
	//    `download_url`（可能是对象存储的键），也是用户机器上存下来的文件名。
	//    用本地名会出现"服务端记的名字和对象存储上的键对不上 → 404"，
	//    而两边各自的日志都显示成功（热更新那边 2026-09-30 就踩过这一下）。
	//
	// ⚠️ 平台那一段是 2026-10-15 加的：两个平台可以**同名**（`notes` 就是），
	//    不加的话第二个平台的包会原地顶掉第一个 —— 而索引里两条都指着它。
	fileName := fmt.Sprintf("%s-%s-%s%s", platform, probe.ID,
		sanitizeVersionForFile(probe.Version), pluginExtFor(platform))
	fileName = filepath.Base(fileName)
	dst := filepath.Join(s.pluginsDir(), fileName)

	// 先删目标再改名：Windows 上 `os.Rename` 到已存在的文件会失败。
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

	order := 100
	if v := strings.TrimSpace(q.Get("order")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			order = n
		}
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
		Notes:       q.Get("notes"),
		Order:       order,
		Pages:       pluginPagesOf(probe),
		Widgets:     pluginWidgetsOf(probe),
		AiTools:     pluginAiToolsOf(probe),
		Services:    pluginServicesOf(probe),
		PublishedAt: time.Now().Unix(),
	}

	idx := s.loadPluginIndex()
	key := pluginKey(platform, art.ID)
	prev := idx.Plugins[key]
	idx.Plugins[key] = art
	if err := s.savePluginIndex(idx); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	replaced := ""
	if prev != nil {
		replaced = "（替换 " + prev.Version + "）"
	}
	log.Printf("plugin published: %s v%s %s (%d bytes, sha256=%s)",
		key, art.Version, replaced, written, sum[:12])
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"artifact": art,
		"replaced": prev,
		"file":     fileName,
		"download": s.pluginDownloadURLFor(art),
	})
}

// handlePluginIndex 给管理员看"现在发布了什么、磁盘上还有什么"。不对外。
func (s *Server) handlePluginIndexView(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(userIDOf(r)) {
		apiErr(w, 403, "只有管理员能看插件索引")
		return
	}
	idx := s.loadPluginIndex()
	dir := s.pluginsDir()
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
		"plugins":     idx.Plugins,
		"files":       files,
		"dir":         dir,
		"api_version": pluginAPIVersion,
	})
}

// ---------- 小工具（从 probe 摊平成索引要的形状） ----------

func pluginPagesOf(m *pluginManifestProbe) []pluginPageBrief {
	out := make([]pluginPageBrief, 0, len(m.Pages))
	for _, p := range m.Pages {
		if strings.TrimSpace(p.ID) == "" {
			continue
		}
		out = append(out, pluginPageBrief{ID: p.ID, Label: p.Label, Icon: p.Icon, Views: len(p.Views)})
	}
	return out
}

func pluginWidgetsOf(m *pluginManifestProbe) []pluginNamedBrief {
	out := make([]pluginNamedBrief, 0, len(m.Widgets))
	for _, w := range m.Widgets {
		if strings.TrimSpace(w.ID) == "" {
			continue
		}
		out = append(out, pluginNamedBrief{ID: w.ID, Label: w.Label})
	}
	return out
}

func pluginAiToolsOf(m *pluginManifestProbe) []string {
	out := make([]string, 0, len(m.AI.Tools))
	for _, t := range m.AI.Tools {
		if s := strings.TrimSpace(t.Name); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func pluginServicesOf(m *pluginManifestProbe) []string {
	out := make([]string, 0, len(m.Services))
	for _, s := range m.Services {
		if n := strings.TrimSpace(s.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}
