package app

import (
	"encoding/json"
	"net/http"
)

// 主页 HTML 卡片的**样式清单**（2026-10-09）。
//
// ## 为什么住在服务端，而不是打进 APK
//
// 用户的原话：
//
//	"从网上找几个 CSS 文件之类的，预先提供给 AI，最好的方式是 AI 留一个
//	 Json 清单，然后可以进行单次读取，而不是打包到 APK 里面，这就需要改一下
//	 这个服务端了。"
//
// 这个决定是对的，而且理由比"省体积"更硬：
//
//   - **改样式不用发版主程序**。样式是"好不好看"的事，会一直调；
//     打进 APK 意味着每调一次都要走一遍出包 → 发版 → 用户 OTA。
//   - **AI 读一次就够**。清单是静态的，AI 写 HTML 卡片时拉一次、
//     把用得上的那套 CSS 内联进页面，之后不再需要网络。
//
// ## ⚠️ 不需要登录
//
// 它里面**没有任何用户数据** —— 就是几段写死的 CSS 和名字。
// 要求登录的话，「用户还没登录时 AI 想画个东西」这条路就断了，
// 而那条路和"有没有账号"没有关系。
//
// ## ⚠️ 这份清单里的 CSS 是**自己写的**，不是从网上抄的
//
// 用户说"从网上找几个"，但直接抄别人的 CSS 会带上许可证问题
// （而且很多"漂亮"的库体量远超这里能给的）。
// 所以这里是几套**极简的等价物**：每套只定义约定的几个类名，
// 加起来不到 3KB，没有许可证负担，也不会因为上游改版而失效。

type homeStyle struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Desc string `json:"desc"`
	// 亮色/暗色各一份：HTML 卡片自己决定跟哪套（`prefers-color-scheme` 在
	// WebView 里也可用，但那要求 AI 写媒体查询；直接给两份更省事也更可控）。
	CSSLight string `json:"css_light"`
	CSSDark  string `json:"css_dark"`
}

// 约定的类名（**必须在 note 里告诉 AI**，否则它不知道能用什么）：
//
//	.card     一块卡片（背景 + 圆角 + 内边距）
//	.row      一行（flex、左右两端对齐）
//	.title    标题
//	.big      大数字（温度、日期这种）
//	.muted    次要文字
//	.chip     小标签
//	.bar      进度/占比条
var homeStyles = []homeStyle{
	{
		ID:   "soft",
		Name: "柔和卡片",
		Desc: "浅底 + 大圆角 + 极淡阴影。最通用的那种，和系统浅色主题最搭。",
		CSSLight: `.card{background:#fff;border-radius:16px;padding:14px;box-shadow:0 1px 3px rgba(0,0,0,.06)}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#1b1b1f}
.big{font-size:28px;font-weight:700;color:#1b1b1f;line-height:1.2}
.muted{font-size:12px;color:#6b6c70}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:#eef0f3;font-size:11px;color:#4a4b50}
.bar{height:6px;border-radius:999px;background:#eef0f3;overflow:hidden}
.bar>i{display:block;height:100%;background:#2F6FB5}`,
		CSSDark: `.card{background:#1c1c1e;border-radius:16px;padding:14px;box-shadow:0 1px 3px rgba(0,0,0,.4)}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#f2f2f7}
.big{font-size:28px;font-weight:700;color:#f2f2f7;line-height:1.2}
.muted{font-size:12px;color:#9a9aa0}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:#2c2c2e;font-size:11px;color:#c7c7cc}
.bar{height:6px;border-radius:999px;background:#2c2c2e;overflow:hidden}
.bar>i{display:block;height:100%;background:#5B9BD5}`,
	},
	{
		ID:   "flat",
		Name: "扁平描边",
		Desc: "没有阴影，只有一圈细边。信息密度高的时候更清爽。",
		CSSLight: `.card{background:transparent;border:1px solid #e3e5e8;border-radius:10px;padding:12px}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#1b1b1f}
.big{font-size:26px;font-weight:700;color:#1b1b1f;line-height:1.2}
.muted{font-size:12px;color:#6b6c70}
.chip{display:inline-block;padding:2px 8px;border:1px solid #d7dade;border-radius:6px;font-size:11px;color:#4a4b50}
.bar{height:4px;background:#e3e5e8;border-radius:2px;overflow:hidden}
.bar>i{display:block;height:100%;background:#1b1b1f}`,
		CSSDark: `.card{background:transparent;border:1px solid #38383a;border-radius:10px;padding:12px}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#f2f2f7}
.big{font-size:26px;font-weight:700;color:#f2f2f7;line-height:1.2}
.muted{font-size:12px;color:#9a9aa0}
.chip{display:inline-block;padding:2px 8px;border:1px solid #48484a;border-radius:6px;font-size:11px;color:#c7c7cc}
.bar{height:4px;background:#2c2c2e;border-radius:2px;overflow:hidden}
.bar>i{display:block;height:100%;background:#f2f2f7}`,
	},
	{
		ID:   "glass",
		Name: "毛玻璃",
		Desc: "半透明 + 模糊。适合压在渐变或图片上面。⚠️ 模糊在低端机上有一点开销。",
		CSSLight: `.card{background:rgba(255,255,255,.55);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);border:1px solid rgba(255,255,255,.6);border-radius:18px;padding:14px}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#1b1b1f}
.big{font-size:28px;font-weight:700;color:#1b1b1f;line-height:1.2}
.muted{font-size:12px;color:#5a5b60}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:rgba(255,255,255,.7);font-size:11px;color:#3a3b40}
.bar{height:6px;border-radius:999px;background:rgba(255,255,255,.6);overflow:hidden}
.bar>i{display:block;height:100%;background:#2F6FB5}`,
		CSSDark: `.card{background:rgba(40,40,44,.5);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);border:1px solid rgba(255,255,255,.12);border-radius:18px;padding:14px}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#f2f2f7}
.big{font-size:28px;font-weight:700;color:#f2f2f7;line-height:1.2}
.muted{font-size:12px;color:#a8a8ae}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:rgba(255,255,255,.12);font-size:11px;color:#d7d7dc}
.bar{height:6px;border-radius:999px;background:rgba(255,255,255,.12);overflow:hidden}
.bar>i{display:block;height:100%;background:#5B9BD5}`,
	},
	{
		ID:   "solid-dark",
		Name: "深色实底",
		Desc: "深底 + 高对比文字。**深色模式下的稳妥选择** —— 不靠透明和模糊，老机器也顺。",
		CSSLight: `.card{background:#12141a;border-radius:14px;padding:14px;color:#fff}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#fff}
.big{font-size:28px;font-weight:700;color:#fff;line-height:1.2}
.muted{font-size:12px;color:#a9adba}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:rgba(255,255,255,.14);font-size:11px;color:#e3e6ee}
.bar{height:6px;border-radius:999px;background:rgba(255,255,255,.14);overflow:hidden}
.bar>i{display:block;height:100%;background:#7FB2F0}`,
		CSSDark: `.card{background:#12141a;border-radius:14px;padding:14px;color:#fff}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#fff}
.big{font-size:28px;font-weight:700;color:#fff;line-height:1.2}
.muted{font-size:12px;color:#a9adba}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:rgba(255,255,255,.14);font-size:11px;color:#e3e6ee}
.bar{height:6px;border-radius:999px;background:rgba(255,255,255,.14);overflow:hidden}
.bar>i{display:block;height:100%;background:#7FB2F0}`,
	},
	{
		ID:   "vivid",
		Name: "渐变撞色",
		Desc: "渐变底 + 白字。适合**只有一块**要抢眼时（比如日期头图），多了会吵。",
		CSSLight: `.card{background:linear-gradient(135deg,#2F6FB5,#7A4FD0);border-radius:20px;padding:16px;color:#fff}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#fff}
.big{font-size:32px;font-weight:700;color:#fff;line-height:1.15}
.muted{font-size:12px;color:rgba(255,255,255,.85)}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:rgba(255,255,255,.2);font-size:11px;color:#fff}
.bar{height:6px;border-radius:999px;background:rgba(255,255,255,.25);overflow:hidden}
.bar>i{display:block;height:100%;background:#fff}`,
		CSSDark: `.card{background:linear-gradient(135deg,#3D5A8C,#5B3E96);border-radius:20px;padding:16px;color:#fff}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.title{font-size:14px;font-weight:600;color:#fff}
.big{font-size:32px;font-weight:700;color:#fff;line-height:1.15}
.muted{font-size:12px;color:rgba(255,255,255,.85)}
.chip{display:inline-block;padding:2px 8px;border-radius:999px;background:rgba(255,255,255,.2);font-size:11px;color:#fff}
.bar{height:6px;border-radius:999px;background:rgba(255,255,255,.25);overflow:hidden}
.bar>i{display:block;height:100%;background:#fff}`,
	},
}

const homeStylesNote = `给 HTML 卡片用的样式清单。
每套只定义这几个**约定类名**，直接用就行（不用自己写 CSS）：
  .card   一块卡片（背景 + 圆角 + 内边距）
  .row    一行（flex，左右两端对齐）
  .title  标题      .big  大数字/大字      .muted  次要文字
  .chip   小标签    .bar  进度条（里面放一个 <i> 表示占比，如 <i style="width:60%"></i>）
css_light / css_dark 各一份，直接用对应主题的那份内联进 <style>。
⚠️ 这几套是**约定**，不是必须：想自己写 CSS 完全可以，那才是 HTML 卡片的自由度所在。`

func (s *Server) handleHomeStyles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 清单是**静态**的（除非改这个文件并重新部署），所以让客户端缓存一天。
	// ⚠️ 缓存的是"清单本身"，不是"AI 写的页面" —— 后者只在这台设备上。
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version": 1,
		"note":    homeStylesNote,
		"styles":  homeStyles,
	})
}
