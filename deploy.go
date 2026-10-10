package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// 主服务端的远程部署通道（2026-09-21）
//
// **为什么加这个**：网页端早就有一条完整的远程更新通道（`web/selfupdate.go`
// + `/-/deploy`），而**主服务端一直什么都没有**。换一次 exe 就得有人上服务器
// 敲 taskkill → move → 起进程，"能上服务器"这件事不是每次都成立。
//
// 后果 2026-09-21 真的发生了：客户端发了 0.6.0，服务端还停在 0.4.0，
// 新接口全是 404。用户原话——
//   「昨晚好像就是没更新,服务端程序上是看到了,但是接口全都没连上」
//
// 这条通道补上之后，**换主服务端 exe 不再需要服务器本机执行任何命令**。
//
// 与网页端的关系：两个 exe 在同一目录，**共用同一个 deploy-token.txt**。
// 所以不需要第二套令牌，也不需要用户再记一个。
// ---------------------------------------------------------------------------

// maxDeployBytes 上传上限。
//
// 主服务端 exe 约 13MB。给到 64MB 是留够"以后加了 embed 资源"的余量，
// 同时挡住"拿这个接口往磁盘上灌垃圾"。
const maxDeployBytes = 64 << 20

// deployTokenPath 与网页端共用同一个令牌文件（同一个目录）。
func deployTokenPath() string {
	return filepath.Join(exeDir(), "deploy-token.txt")
}

// readDeployToken 读令牌。
//
// ⚠️ 每次现读、**不缓存**：令牌是可以轮换的，缓存住的话轮换之后
// 旧令牌还能用（而"轮换令牌"往往正是因为旧令牌泄漏了）。
// 这个接口调用频率极低，多一次文件读毫无成本。
//
// 读不到令牌时**直接拒绝所有请求**（fail closed）——
// 绝不能出现"没有令牌文件 = 不校验"这种反过来的默认值。
func readDeployToken() (string, error) {
	b, err := os.ReadFile(deployTokenPath())
	if err != nil {
		return "", errors.New("部署通道未启用（读不到 " + deployTokenPath() + "）")
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("部署令牌是空的")
	}
	return t, nil
}

// exeDir 当前进程 exe 所在目录。
//
// 部署接口写文件、自更新找暂存的 .new.exe 都靠它。
// 拿不到就返回空串 —— 调用方会发现路径拼出来不对，而不是悄悄写到别处。
func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err2 := filepath.EvalSymlinks(p); err2 == nil {
		p = resolved
	}
	return filepath.Dir(p)
}

// handleDeployServer 接收新的主服务端 exe，暂存，并安排一次替换+重启。
//
// 请求：`POST /api/deploy/server`，`X-Deploy-Token` 头带令牌，body 是 exe 原始字节。
//
// ⚠️ **这个接口不走 `s.auth`**（不能用登录令牌）——它是给部署脚本用的，
// 脚本手上没有管理员会话，而且我们也不希望"某个成员的管理员会话"能顺手换掉
// 整个服务端二进制。所以它是一条**独立的、只认部署令牌**的通道。
//
// 换 exe 的动作**不在本请求里做**（运行中的 exe 在 Windows 上不能被覆盖），
// 而是交给 selfupdate.go 的 scheduleSelfReplace：起一个脱离本进程的替换脚本，
// 本进程让出文件句柄后退出。**响应先回，替换随后发生** —— 所以响应里
// 明说"重启之后才生效"，不要让人以为发完就切过去了。
func (s *Server) handleDeployServer(w http.ResponseWriter, r *http.Request) {
	want, err := readDeployToken()
	if err != nil {
		apiErr(w, 503, err.Error())
		return
	}
	got := strings.TrimSpace(r.Header.Get("X-Deploy-Token"))
	// 定时比较：普通 == 会在第一个不同的字节上返回，理论上能被逐字节探出来
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		log.Printf("[deploy] 令牌不对，拒绝（来自 %s）", r.RemoteAddr)
		apiErr(w, 403, "部署令牌不对")
		return
	}

	dir := exeDir()
	if dir == "" {
		apiErr(w, 500, "拿不到 exe 所在目录，无法部署")
		return
	}

	// 先写临时文件，写完再改名 —— 中途断线不会在暂存位置留下半个 exe，
	// 而"半个 exe 停在暂存位"会被下一次替换直接换上去，必炸。
	tmp := filepath.Join(dir, stagedExeName+".part")
	dst := filepath.Join(dir, stagedExeName)

	// 磁盘要有地方：先清掉上次没换成的暂存（能到这一步说明它已经没用了）
	_ = os.Remove(dst)

	f, err := os.Create(tmp)
	if err != nil {
		apiErr(w, 500, "写不了暂存文件："+err.Error())
		return
	}
	// LimitReader 多读一个字节：刚好卡在上限时能区分"正好"和"超了"
	n, err := io.Copy(f, io.LimitReader(r.Body, maxDeployBytes+1))
	if err != nil {
		f.Close()
		_ = os.Remove(tmp)
		apiErr(w, 500, "收包出错："+err.Error())
		return
	}
	if n > maxDeployBytes {
		f.Close()
		_ = os.Remove(tmp)
		apiErr(w, 413, fmt.Sprintf("文件太大（上限 %d MB）", maxDeployBytes>>20))
		return
	}
	// ⚠️ 必须 Sync 再关闭：不 Sync 的话缓冲区里的内容可能还在，
	// 而紧接着的替换脚本会去 rename 这个文件 —— 换上去的就是个截断的 exe。
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		apiErr(w, 500, "落盘失败："+err.Error())
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		apiErr(w, 500, "关闭文件失败："+err.Error())
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		apiErr(w, 500, "改名失败："+err.Error())
		return
	}
	// 一个明显太小的 exe 几乎一定是传坏了（正常 13MB）。
	// 不直接拒（以后可能变小），但要吼一声 —— 换上去起不来才发现就太晚了。
	if n < 1<<20 {
		log.Printf("[deploy] 警告：收到的 exe 只有 %d 字节，可能是传坏了", n)
	}

	if err := scheduleSelfReplace("部署接口收到 " + fmt.Sprintf("%d 字节", n)); err != nil {
		apiErr(w, 500, "暂存好了，但安排替换失败："+err.Error())
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"staged":    dst,
		"bytes":     n,
		"from":      version,
		"note":      "已安排替换并重启；本响应发出后服务进程会退出，新版本起来才算生效",
		"health":    "/api/health 的 pending_update 字段能看到'有待重启的新版本'",
		"instances": s.counts(),
	})
}

// counts 给部署响应和 health 用的实体计数（方便换版本前后对比，确认数据没丢）。
func (s *Server) counts() map[string]any {
	var users, events, instances int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM events WHERE deleted_at IS NULL`).Scan(&events)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM instances WHERE deleted_at IS NULL`).Scan(&instances)
	return map[string]any{"users": users, "events": events, "instances": instances}
}
