package panel

import (
	"embed"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed web/*
var webFS embed.FS

// handleStatic 提供面板前端。全部资源都在 exe 内，没有 CDN 依赖。
func (p *Panel) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/" + name)
	if err != nil {
		// 单页应用：未知路径回落到首页，方便直接刷新 /accounts 这种地址。
		if !strings.Contains(name, ".") {
			data, err = webFS.ReadFile("web/index.html")
			name = "index.html"
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
