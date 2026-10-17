package app

import (
	"fmt"
	"html"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// 团队 / 日历邀请的**落地页**（2026-10-15）
//
// ## 为什么主服务端要自己出一页
//
// 以前客户端把邀请链接拼成 `https://<作者域名>:8444/i/<code>` —— 那个地址里
// 同时写死了"作者的域名"和"网页端的端口"。两件事都错：
//
//	① 合规：个人备案的安装包里不该出现指向作者服务器的地址（见 ApiClient.kt
//	   里 `PRIMARY_SERVER` 那段说明）；
//	② 自建服务器的用户：邀请链接会指向**作者的**网站，同学点开看到的是
//	   一个跟他毫无关系的页面。
//
// 现在邀请链接就是 `<用户自己配的服务器>/i/<code>` —— 谁部署的服务器，
// 链接就落在谁那儿。主服务端本来就在监听、本来就有这个码（`team_invites`
// / `invites` 两张表），出这一页不需要任何新依赖。
//
// ## ⚠️ 这一页是**匿名**的，所以它只说"有这么一个邀请"，不泄露任何内容
//
// 邀请码本身是 256 位随机（`fst_` / `fsi_` 前缀），能猜到等于不可能。
// 页面**不显示**团队里有谁、有哪些日程 —— 那些要登录之后才看得到。
// ---------------------------------------------------------------------------

// inviteLandingHandler 出那一页。刻意是一个**纯 HTML 字符串**，
// 不引模板引擎：一页、没有变量循环，模板带来的只有一层间接。
func (s *Server) inviteLandingHandler(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	if code == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	kind := "邀请"
	switch {
	case strings.HasPrefix(code, "fst_"):
		kind = "团队邀请"
	case strings.HasPrefix(code, "fsi_"):
		kind = "日历邀请"
	}

	// ⚠️ 所有拼进 HTML 的东西都要转义。`code` 是**路径参数**（外部输入），
	//    直接拼进去就是一个反射型 XSS —— 而这一页是匿名的，等于谁都能利用。
	safeCode := html.EscapeString(code)
	page := fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>%s · 3e学习助手</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         font: 16px/1.65 -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif;
         background:#f6f7f9; color:#1c1c1e; padding:24px; box-sizing:border-box; }
  .card { background:#fff; border:1px solid #e3e5e8; border-radius:16px; padding:28px 24px;
          max-width:520px; width:100%%; box-shadow:0 1px 3px rgba(0,0,0,.05); }
  h1 { margin:0 0 6px; font-size:20px; }
  .sub { margin:0 0 18px; color:#6b7280; font-size:14px; }
  .code { display:block; margin:14px 0; padding:14px; border-radius:10px;
          background:#f2f4f7; font-family:ui-monospace, Consolas, monospace;
          font-size:15px; word-break:break-all; user-select:all; }
  ol { margin:14px 0 0; padding-left:20px; color:#374151; font-size:14px; }
  li { margin:6px 0; }
  .foot { margin-top:20px; color:#9ca3af; font-size:12.5px; }
  @media (prefers-color-scheme: dark) {
    body { background:#111214; color:#e7e8ea; }
    .card { background:#1b1c1f; border-color:#2c2e33; }
    .sub { color:#9aa0a6; } .code { background:#24262b; } ol { color:#c6c9cd; }
  }
</style>
</head>
<body>
  <div class="card">
    <h1>%s</h1>
    <p class="sub">有人把 3e学习助手 的%s发给了你。</p>
    <span class="code">%s</span>
    <ol>
      <li>打开 3e学习助手（还没装就先装上）。</li>
      <li>在登录页点「注册新账号」。</li>
      <li>把上面这串码粘进「邀请码」那一栏，填好名字和密码即可。</li>
    </ol>
    <p class="foot">这串码有时效，过期了找发给你的人再要一个。</p>
  </div>
</body>
</html>`, kind, kind, kind, safeCode)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 邀请码会被吊销/过期，页面本身不该被缓存 —— 缓存住的话"已经失效的邀请"
	// 在浏览器里还会显示成有效的样子。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// ⚠️ 这一页**没有脚本**，但把 CSP 写死在这里是一道廉价的保险：
	//    将来谁往这里加了脚本，浏览器会直接拒绝执行，而不是悄悄生效。
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write([]byte(page))
}
