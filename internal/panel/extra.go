package panel

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// handleBackups 列出数据目录下的备份文件。
func (p *Panel) handleBackups(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(p.cfg.DataDir, "backups")
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"backups": []any{}})
		return
	}
	type item struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		ModTime int64  `json:"mod_time"`
	}
	out := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, item{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	writeJSON(w, http.StatusOK, map[string]any{"backups": out})
}

// deployTokenFile 与服务核心共用同一个令牌文件。
func (p *Panel) deployTokenFile() string {
	return filepath.Join(p.opt.BaseDir, "deploy-token.txt")
}

// handleGetDeployToken 返回当前令牌。没有就返回空，由用户点“生成”。
func (p *Panel) handleGetDeployToken(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(p.deployTokenFile())
	token := ""
	if err == nil {
		token = strings.TrimSpace(string(b))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"path":  p.deployTokenFile(),
		"set":   token != "",
	})
}

// handleNewDeployToken 生成并覆盖部署令牌。
//
// 覆盖会让旧令牌立刻失效（服务端每次现读文件），这是有意的：
// 轮换令牌通常正是因为旧令牌泄漏了。
func (p *Panel) handleNewDeployToken(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		writeErr(w, http.StatusInternalServerError, "生成随机令牌失败")
		return
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(p.deployTokenFile(), []byte(token+"\n"), 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入令牌文件失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "path": p.deployTokenFile()})
}

// handleOpenPanel 把面板地址告诉管理器（用于“在浏览器中打开”）。
func (p *Panel) handleOpenPanel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"url": p.URL()})
}

// nowString 给日志用。
func nowString() string { return time.Now().Format("2006-01-02 15:04:05") }
