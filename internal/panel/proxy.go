package panel

import (
	"io"
	"net/http"
	"strings"
)

// handleProxy 把面板收到的请求转发给本机服务端。
//
// 只允许 /api/ 前缀，避免面板变成任意地址的代理。鉴权由调用方（p.auth）完成，
// 转发时换成该会话持有的服务端 token。
func (p *Panel) handleProxy(w http.ResponseWriter, r *http.Request) {
	sess := p.sessionOf(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "请先登录")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/panel/proxy/")
	if !strings.HasPrefix(rest, "api/") {
		writeErr(w, http.StatusBadRequest, "代理路径不合法")
		return
	}
	base := p.base()
	if base == "" {
		writeErr(w, http.StatusServiceUnavailable, "服务端没有可用的监听地址")
		return
	}
	target := base + "/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	// 实时协作的短票据接口需要这个头；其余接口不受影响。
	if lt := r.Header.Get("X-Live-Ticket"); lt != "" {
		req.Header.Set("X-Live-Ticket", lt)
	}
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	if r.ContentLength > 0 {
		req.ContentLength = r.ContentLength
	}

	resp, err := proxyHTTP().Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "连接服务端失败："+err.Error())
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{"Content-Type", "Content-Disposition", "Cache-Control", "X-Osm-Cache"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
