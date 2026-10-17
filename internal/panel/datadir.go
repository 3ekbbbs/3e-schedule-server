package panel

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/3ekbbbs/schedule-server/internal/config"
)

// ---------------------------------------------------------------------------
// 数据目录与程序目录分离（2026-10-18）
//
// 起因：旧版本把 data/ 和 certs/ 放在 exe 旁边。更新程序时只要整个目录
// 被替换（删了重装、解压到新目录再覆盖），业务数据就跟着没了。
//
// 现在的规则：
//   · 新装：data / certs 落在程序目录之外（config.DataRoot）
//   · 老部署：原地继续用 <exe>/data，不背着他搬
//   · 想搬：面板首页给一个按钮，搬到 DataRoot，**原目录保留**
//
// 搬家不是"移动"而是"复制 + 改配置 + 起服务 + 对账"。任何一步不对就退回
// 原配置，原目录一直没动过，所以没有"搬坏了"这种状态。
// ---------------------------------------------------------------------------

type dataLayout struct {
	DataDir   string `json:"data_dir"`
	CertDir   string `json:"cert_dir"`
	Target    string `json:"target"`
	TargetCrt string `json:"target_cert"`
	InProgram bool   `json:"in_program"`
	CanMove   bool   `json:"can_move"`
	Reason    string `json:"reason,omitempty"`
}

type relocateResult struct {
	OK        bool     `json:"ok"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	Bytes     int64    `json:"bytes"`
	Files     int      `json:"files"`
	Before    *dbStats `json:"before"`
	After     *dbStats `json:"after"`
	Mismatch  []string `json:"mismatch,omitempty"`
	Config    string   `json:"config"`
	OldKept   string   `json:"old_kept"`
	CertMoved bool     `json:"cert_moved"`
}

// dataLayoutNow 描述当前数据目录的位置，以及能不能搬。
func (p *Panel) dataLayoutNow() dataLayout {
	dataDir := p.cfg.DataDir
	certDir := p.cfg.CertDir
	target := filepath.Join(config.DataRoot(p.opt.BaseDir), "data")
	targetCert := filepath.Join(config.DataRoot(p.opt.BaseDir), "certs")

	out := dataLayout{
		DataDir:   dataDir,
		CertDir:   certDir,
		Target:    target,
		TargetCrt: targetCert,
		InProgram: insideDir(p.opt.BaseDir, dataDir),
	}
	if !out.InProgram {
		out.Reason = "数据已经在程序目录之外"
		return out
	}
	if samePath(dataDir, target) {
		out.Reason = "数据就在默认位置，但它在程序目录里（程序目录恰好是数据根目录）"
		return out
	}
	if insideDir(p.opt.BaseDir, target) {
		// 拿不到 %ProgramData% 这类目录时会走到这里。搬了还是在程序目录里，
		// 没有意义，不如不搬。
		out.Reason = "这台机器上取不到程序目录之外的路径，设一下环境变量 SCHEDULE_SERVER_ROOT"
		return out
	}
	if _, err := os.Stat(filepath.Join(target, "schedule.db")); err == nil {
		out.Reason = "目标位置已经有一个库：" + target + "，先确认那份是不是你要的"
		return out
	}
	out.CanMove = true
	return out
}

func (p *Panel) handleDataLayout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.dataLayoutNow())
}

// handleDataRelocate 把 data / certs 复制到程序目录之外，然后改配置起服务。
func (p *Panel) handleDataRelocate(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	if p.relocating {
		p.mu.Unlock()
		writeErr(w, http.StatusConflict, "已经在搬了，等这次做完")
		return
	}
	p.relocating = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.relocating = false
		p.mu.Unlock()
	}()

	lay := p.dataLayoutNow()
	if !lay.CanMove {
		writeErr(w, http.StatusBadRequest, lay.Reason)
		return
	}

	wasRunning := false
	if running, _, _ := p.running(); running {
		wasRunning = true
		if err := p.StopServer(); err != nil {
			writeErr(w, http.StatusInternalServerError, "先停服务失败："+err.Error())
			return
		}
	}
	restart := func() {
		if wasRunning {
			if err := p.StartServer(); err != nil {
				p.setLastErr("搬完重启服务失败：" + err.Error())
			}
		}
	}

	// 记下搬之前的样子，搬完对账。读的时候服务已经停了。
	before, _ := p.currentStats()

	oldData, oldCert := p.cfg.DataDir, p.cfg.CertDir
	newData := lay.Target

	bytes, files, err := copyTree(oldData, newData, skipLogs)
	if err != nil {
		restart()
		writeErr(w, http.StatusInternalServerError, "复制数据失败："+err.Error()+"（原目录没有动）")
		return
	}

	// 证书一起搬：换了目录还要重新签，客户端又要重新信任一次。
	newCert := oldCert
	certMoved := false
	if !samePath(oldCert, lay.TargetCrt) {
		if _, _, err := copyTree(oldCert, lay.TargetCrt, nil); err == nil {
			newCert = lay.TargetCrt
			certMoved = true
		}
		// 证书复制失败不算致命：服务会按 auto_cert 重新生成。
	}

	cfg := p.snapshotConfig()
	cfg.DataDir = newData
	cfg.CertDir = newCert
	if err := config.Save(p.opt.ConfigPath, cfg); err != nil {
		restart()
		writeErr(w, http.StatusInternalServerError, "写配置失败，没搬："+err.Error())
		return
	}
	p.reloadConfig(cfg)
	p.refreshLogPaths()

	if err := p.StartServer(); err != nil {
		p.revertPaths(cfg, oldData, oldCert)
		writeErr(w, http.StatusInternalServerError, "新目录起服务失败，已退回原来的目录："+err.Error())
		return
	}

	// 等健康检查通过，再对数。
	var after *dbStats
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if p.health() != nil {
			after, _ = p.currentStats()
			if after != nil {
				break
			}
		}
		time.Sleep(400 * time.Millisecond)
	}

	mismatch := diffStats(before, after)
	if after == nil || len(mismatch) > 0 {
		_ = p.StopServer()
		p.revertPaths(cfg, oldData, oldCert)
		note := "新目录起不来或数据对不上，已退回原来的目录。"
		if len(mismatch) > 0 {
			note += "对不上的地方：" + strings.Join(mismatch, "；")
		}
		writeErr(w, http.StatusInternalServerError, note+"旧目录和复制出来的那份都还在。")
		return
	}

	// 原目录留一封说明，不删。用户后悔了还能从里面拿东西。
	_ = os.WriteFile(filepath.Join(oldData, "已搬到.txt"),
		[]byte("数据已复制到：\r\n"+newData+"\r\n\r\n这个目录没有删除，确认新位置正常后可以自行清理。\r\n"), 0o644)

	// 搬之前是停着的，搬完仍然停着 —— 除了搬家本身，别替他改状态。
	if !wasRunning {
		_ = p.StopServer()
	}

	writeJSON(w, http.StatusOK, relocateResult{
		OK:        true,
		From:      oldData,
		To:        newData,
		Bytes:     bytes,
		Files:     files,
		Before:    before,
		After:     after,
		Config:    p.opt.ConfigPath,
		OldKept:   oldData,
		CertMoved: certMoved,
	})
}

// revertPaths 把配置改回原来的目录并重启。
func (p *Panel) revertPaths(used config.Config, dataDir, certDir string) {
	old := used
	old.DataDir = dataDir
	old.CertDir = certDir
	_ = config.Save(p.opt.ConfigPath, old)
	p.reloadConfig(old)
	p.refreshLogPaths()
	if err := p.StartServer(); err != nil {
		p.setLastErr("退回原目录后仍起不来：" + err.Error())
	}
}

func (p *Panel) refreshLogPaths() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consoleLog = filepath.Join(p.cfg.DataDir, "logs", "console.log")
	p.serverLog = filepath.Join(p.cfg.DataDir, "logs", "server.log")
}

func (p *Panel) setLastErr(msg string) {
	p.mu.Lock()
	p.lastErr = msg
	p.mu.Unlock()
}

// skipLogs 复制数据目录时不带 logs：那是可再生的，而且可能很大。
func skipLogs(rel string) bool {
	return rel == "logs" || strings.HasPrefix(rel, "logs"+string(os.PathSeparator))
}

// copyTree 递归复制目录，返回字节数与文件数。
// skip 返回 true 的相对路径会被跳过（目录也一样，不再往下走）。
func copyTree(src, dst string, skip func(rel string) bool) (int64, int, error) {
	var total int64
	var count int
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skip != nil && skip(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := copyFileCounted(path, target, &total); err != nil {
			return err
		}
		count++
		return nil
	})
	return total, count, err
}

func copyFileCounted(src, dst string, total *int64) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	*total += n
	return err
}

// insideDir 判断 child 是否在 parent 里面（同级或更外层都算 false）。
func insideDir(parent, child string) bool {
	p, err1 := filepath.Abs(parent)
	c, err2 := filepath.Abs(child)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

func describeBytes(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", n/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}
