package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

type ctxKey string

const ctxUserID ctxKey = "uid"

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if tok == "" {
			apiErr(w, http.StatusUnauthorized, "missing token")
			return
		}
		var uid string
		err := s.db.sql.QueryRow(`SELECT user_id FROM tokens WHERE token=?`, tok).Scan(&uid)
		if err != nil {
			apiErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUserID, uid)))
	}
}

func userIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxUserID).(string); ok {
		return v
	}
	return ""
}

// bearerToken 取请求头里的令牌原文。中间件里已经验过一次，这里是为了
// "改密后只留当前这台"这种需要知道自己拿的是哪个 token 的场景。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// handleBootstrap reports whether the server still needs its first admin, so
// the very first launch can create the admin without an invite code.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var n int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	writeJSON(w, 200, map[string]any{"need_admin": n == 0, "server_time": time.Now().Unix()})
}

type registerReq struct {
	InviteCode string `json:"invite_code"`
	Name       string `json:"name"`
	Pass       string `json:"pass"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Pass) < 4 {
		apiErr(w, 400, "名字不能为空，密码至少 4 位")
		return
	}

	var n int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	isAdmin := n == 0
	if !isAdmin {
		if s.cfg.InviteCode == "" || req.InviteCode != s.cfg.InviteCode {
			apiErr(w, 403, "邀请码不正确")
			return
		}
	}

	id := newID("u_")
	now := time.Now().Unix()
	_, err := s.db.sql.Exec(
		`INSERT INTO users(id,name,pass_hash,is_admin,created_at) VALUES(?,?,?,?,?)`,
		id, req.Name, hashPassword(req.Pass), boolToInt(isAdmin), now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			apiErr(w, 409, "这个名字已经被占用了")
			return
		}
		apiErr(w, 500, err.Error())
		return
	}

	tok := newToken()
	if _, err := s.db.sql.Exec(`INSERT INTO tokens(token,user_id,created_at) VALUES(?,?,?)`, tok, id, now); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"token": tok,
		"user":  User{ID: id, Name: req.Name, IsAdmin: isAdmin, CreatedAt: now},
	})
}

type loginReq struct {
	Name string `json:"name"`
	Pass string `json:"pass"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	var id, hash string
	var admin int64
	var created int64
	err := s.db.sql.QueryRow(
		`SELECT id,pass_hash,is_admin,created_at FROM users WHERE name=?`,
		strings.TrimSpace(req.Name)).Scan(&id, &hash, &admin, &created)
	if err != nil || !verifyPassword(req.Pass, hash) {
		apiErr(w, 401, "用户名或密码不对")
		return
	}
	tok := newToken()
	now := time.Now().Unix()
	if _, err := s.db.sql.Exec(`INSERT INTO tokens(token,user_id,created_at) VALUES(?,?,?)`, tok, id, now); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"token": tok,
		"user":  User{ID: id, Name: strings.TrimSpace(req.Name), IsAdmin: admin != 0, CreatedAt: created},
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	_, _ = s.db.sql.Exec(`DELETE FROM tokens WHERE token=?`, bearerToken(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

type passwordReq struct {
	OldPass string `json:"old_pass"`
	NewPass string `json:"new_pass"`
}

/**
 * handleChangePassword 改自己的密码。
 *
 * 两道门槛：①带着有效 token（身份），②**还得报出旧密码**。
 * 只认 token 的话，谁临时拿到你的手机就能把密码改掉、把你所有设备踢下线；
 * 加一道旧密码，改密这件事就必须是本人操作。
 *
 * 改完把**其它设备**的会话作废（当前这台留着，否则用户改完密码自己先被踢出来）。
 * 这正是改密该有的效果：密码是泄露后才来改的，别处的登录必须一起失效。
 */
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req passwordReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	req.NewPass = strings.TrimSpace(req.NewPass)
	if len(req.NewPass) < 4 {
		apiErr(w, 400, "新密码至少 4 位")
		return
	}
	if req.NewPass == req.OldPass {
		apiErr(w, 400, "新密码不能和旧密码一样")
		return
	}

	var oldHash string
	if err := s.db.sql.QueryRow(`SELECT pass_hash FROM users WHERE id=?`, uid).Scan(&oldHash); err != nil {
		apiErr(w, 404, "找不到这个账户")
		return
	}
	if !verifyPassword(req.OldPass, oldHash) {
		apiErr(w, 403, "旧密码不对")
		return
	}
	if _, err := s.db.sql.Exec(`UPDATE users SET pass_hash=? WHERE id=?`,
		hashPassword(req.NewPass), uid); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	cur := bearerToken(r)
	kicked := int64(0)
	if res, err := s.db.sql.Exec(`DELETE FROM tokens WHERE user_id=? AND token<>?`, uid, cur); err == nil {
		if n, e := res.RowsAffected(); e == nil {
			kicked = n
		}
	}
	log.Printf("[password] 账户 %s 改了密码，作废其它会话 %d 个", uid, kicked)
	writeJSON(w, 200, map[string]any{"ok": true, "signed_out": kicked})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var u User
	var admin int64
	err := s.db.sql.QueryRow(`SELECT id,name,is_admin,created_at FROM users WHERE id=?`, uid).
		Scan(&u.ID, &u.Name, &admin, &u.CreatedAt)
	if err != nil {
		apiErr(w, 404, "user not found")
		return
	}
	u.IsAdmin = admin != 0
	writeJSON(w, 200, map[string]any{"user": u})
}

// handleGetInvite lets an admin read the code they need to hand out. Without
// this the only way to learn it is reading the server log on the machine.
func (s *Server) handleGetInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var admin int64
	if err := s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&admin); err != nil || admin == 0 {
		apiErr(w, 403, "只有管理员可以查看邀请码")
		return
	}
	writeJSON(w, 200, map[string]any{"invite_code": s.cfg.InviteCode})
}

// handleUsers lists every account, used by the member picker. The group is
// deliberately smaller than ten people, so exposing the roster is fine.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.sql.Query(`SELECT id,name,is_admin,created_at FROM users ORDER BY created_at`)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var admin int64
		if err := rows.Scan(&u.ID, &u.Name, &admin, &u.CreatedAt); err == nil {
			u.IsAdmin = admin != 0
			out = append(out, u)
		}
	}
	writeJSON(w, 200, map[string]any{"users": out})
}
