package app

import (
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ===========================================================================
// 违禁词匹配
// ===========================================================================
//
// 设计要点，每条都是踩过的坑：
//
// 1. **归一化后匹配，而不是直接 Contains。**
//    真实绕过手法是插字符："法 轮 功"、"法*轮*功"、"法-轮-功"、"法。轮。功"，
//    以及全角（"法轮功"用全角字母）、大小写（"FaLunGong"）、零宽字符。
//    所以待检文本与词条**都**走一遍 normalizeForMatch，再找子串。
//    只归一化一边是不够的 —— 词表里存的也得是归一化形式（入库时就转）。
//
// 2. **词表带进程内缓存，写路径不能每个请求查一次库。**
//    写入拦截发生在"建日程"这种高频路径上，每次都 SELECT 全表会让它变成热点。
//    缓存用 RWMutex 保护，管理员增删词时**立刻失效重建**（不是等 TTL），
//    否则"我明明加了词怎么不生效"。
//
// 3. **匹配结果要能说清"中了哪个词、什么分类"**，不能只回一句"含违禁词"。
//    前端要高亮，管理员要看依据，用户要知道改哪里。所以 FindHit 返回结构化结果。

// BannedWord 是词表里的一行（给后台展示用）。
type BannedWord struct {
	ID        string `json:"id"`
	Word      string `json:"word"`     // 归一化形式
	Raw       string `json:"raw"`      // 管理员输入的原样
	Category  string `json:"category"` // porn/politics/violence/gang/other
	Severity  int    `json:"severity"` // 1=提醒 2=拦截 3=风险（转 AI 复核）
	Enabled   bool   `json:"enabled"`
	Note      string `json:"note"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// WordHit 是一次命中的结果。
type WordHit struct {
	Word     string `json:"word"`     // 归一化词
	Raw      string `json:"raw"`      // 展示用原词
	Category string `json:"category"` // 分类
	Severity int    `json:"severity"` // 1=提醒 2=拦截 3=风险（转 AI 复核）
}

// severity 三档。用常量而不是裸数字 —— 这三个数字的语义差别很大
// （放行 / 拦死 / 转 AI），散落的字面量迟早会有人把 2 和 3 写反。
const (
	// sevNotice 只提醒用户，不拦。
	sevNotice = 1
	// sevBlock 直接拦，不让保存。**真正的边界**。
	sevBlock = 2
	// sevRisk 不拦，转交 AI 复核（详见 moderation_ai.go）。
	//
	// 这一档存在的理由见 db.go 里 severity 那段注释：生僻字/异体字/谐音
	// 抓不到、纯语义违规抓不到、词本身有正常用法会误伤 —— 三种都归这里。
	sevRisk = 3
)

// severityLabels 是给后台用的中文名（前端不自己维护一份，免得两边对不上）。
var severityLabels = map[int]string{
	sevNotice: "仅提醒",
	sevBlock:  "直接拦截",
	sevRisk:   "风险词（转 AI 判断）",
}

// 分类的中文名（前端与提示文案共用；写在服务端是为了只有一份）。
var wordCategoryLabels = map[string]string{
	"porn":     "涉黄",
	"politics": "涉政",
	"violence": "涉暴",
	"gang":     "涉黑违法",
	"other":    "其他违规",
}

func wordCategoryLabel(c string) string {
	if s, ok := wordCategoryLabels[c]; ok {
		return s
	}
	return "其他违规"
}

// ---------------------------------------------------------------------------
// 归一化
// ---------------------------------------------------------------------------

// matchSeparators 是**在匹配时会被忽略**的字符。刻意只放"明显的分隔/修饰"
// 类字符，不放正常标点（句号逗号这类出现在正文里太正常，删了会把不相关的
// 文本粘到一起造成误报）。
//
// ⚠️ 这里宁可少删也不要多删：多删一个字符就可能把"伤害"和"害怕"拼成敏感词。
// 这是"宁可漏报不可误报"的取值 —— 漏举报还有举报兜底，误报会直接打断用户。
const matchSeparators = " \t\r\n\u00a0\u3000·•*-_—–~～、。，,．.·*＊/\\|｜+＋=＝#＃@＠!！?？'\"“”‘’()（）[]【】{}《》<>《》^&%$"

// zeroWidth 是零宽/不可见字符，必须删掉（否则"法\u200b轮功"能绕过）。
var zeroWidth = map[rune]bool{
	'\u200b': true, '\u200c': true, '\u200d': true, '\u200e': true, '\u200f': true,
	'\u202a': true, '\u202b': true, '\u202c': true, '\u202d': true, '\u202e': true,
	'\u2060': true, '\ufeff': true, '\u00ad': true,
}

// normalizeForMatch 把文本变成"用于匹配"的形式。
//
// 步骤：① 全角转半角；② 统一小写；③ 删零宽字符；④ 删分隔/修饰字符。
// **不删中文字符本身** —— 中文没有大小写、也没有词形变化，保留原字即可。
func normalizeForMatch(s string) string {
	if s == "" {
		return ""
	}
	sep := make(map[rune]bool, len(matchSeparators))
	for _, r := range matchSeparators {
		sep[r] = true
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if zeroWidth[r] {
			continue
		}
		// 全角 ASCII（！ 到 ～）转半角；全角空格单独处理
		if r >= '\uff01' && r <= '\uff5e' {
			r = r - 0xfee0
		}
		if r == '\u3000' {
			continue
		}
		if sep[r] {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 引擎（带缓存的词表）
// ---------------------------------------------------------------------------

// wordEngine 持有词表的内存副本。零值可用（list 为 nil、加载过之后 ready=true）。
type wordEngine struct {
	mu    sync.RWMutex
	list  []BannedWord
	ready bool
}

// load 从库里重建缓存。**在写路径上不调用**（那是热点），只在：
//   - 服务启动时；
//   - 管理员增删改词之后；
//   - 词库为空、需要灌内置词之后。
func (e *wordEngine) load(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT id, word, raw, category, severity, enabled, note,
		       created_by, created_at, updated_at
		FROM banned_words`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var out []BannedWord
	for rows.Next() {
		var w BannedWord
		var enabled int
		if err := rows.Scan(&w.ID, &w.Word, &w.Raw, &w.Category, &w.Severity,
			&enabled, &w.Note, &w.CreatedBy, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return err
		}
		w.Enabled = enabled != 0
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	e.list = out
	e.ready = true
	e.mu.Unlock()
	return nil
}

// invalidate 让缓存下次使用时重建。增删词之后调用。
//
// 为什么是"标脏 + 立即重建"而不是只标脏：标脏会让**下一次写入请求**去查库，
// 而那个请求可能是并发来的、还没拿到管理员的操作结果。立即重建最简单可靠。
func (e *wordEngine) reload(db *sql.DB) {
	_ = e.load(db)
}

// FindHit 在文本里找第一个命中的词。
//
// 返回**严重度最高**的那个（拦截优先于提醒）：如果一句话里既有"提醒词"又有
// "拦截词"，正确的结论是"拦截"，而不是"先看到哪个算哪个"。
// 同级之间按词长降序 —— 长词更具体，提示"命中了『儿童色情』"比"命中了『色情』"
// 对用户更有用。
func (e *wordEngine) FindHit(text string) *WordHit {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	norm := normalizeForMatch(text)
	if norm == "" {
		return nil
	}
	e.mu.RLock()
	list := e.list
	e.mu.RUnlock()
	if len(list) == 0 {
		return nil
	}

	var best *WordHit
	for i := range list {
		w := &list[i]
		if !w.Enabled || w.Word == "" {
			continue
		}
		if !strings.Contains(norm, w.Word) {
			continue
		}
		// 同词去重由唯一索引保证；这里只挑最优的一条
		if best == nil ||
			w.Severity > best.Severity ||
			(w.Severity == best.Severity && len(w.Word) > len(best.Word)) {
			best = &WordHit{
				Word:     w.Word,
				Raw:      firstNonEmpty(w.Raw, w.Word),
				Category: w.Category,
				Severity: w.Severity,
			}
		}
	}
	return best
}

// FindAllHits 找出**所有**命中的词（去重）。给管理员/后台排查用，
// 写路径用 FindHit 就够（只需要知道"该不该拦"）。
func (e *wordEngine) FindAllHits(text string) []WordHit {
	norm := normalizeForMatch(text)
	if norm == "" {
		return nil
	}
	e.mu.RLock()
	list := e.list
	e.mu.RUnlock()
	seen := map[string]bool{}
	var out []WordHit
	for i := range list {
		w := &list[i]
		if !w.Enabled || w.Word == "" || seen[w.Word] {
			continue
		}
		if strings.Contains(norm, w.Word) {
			seen[w.Word] = true
			out = append(out, WordHit{
				Word: w.Word, Raw: firstNonEmpty(w.Raw, w.Word),
				Category: w.Category, Severity: w.Severity,
			})
		}
	}
	return out
}

// Count 返回启用的词数（后台显示用）。
func (e *wordEngine) Count() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	n := 0
	for i := range e.list {
		if e.list[i].Enabled {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 内置词灌入
// ---------------------------------------------------------------------------

// ensureSeedWords 在表**完全为空**时灌内置词。
//
// 判据是"表里一行都没有"，而不是"没有内置的这些词" —— 后者会在管理员
// 删掉某个内置词之后，下次重启又把它塞回来。管理员的删除必须是永久的。
func (s *Server) ensureSeedWords() error {
	var n int
	if err := s.db.sql.QueryRow(`SELECT COUNT(*) FROM banned_words`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	now := time.Now().Unix()
	for _, sw := range seedBannedWords {
		norm := normalizeForMatch(sw.Word)
		if norm == "" {
			continue
		}
		// 词表内自己也可能归一化后重复（如"法轮功"与"法 轮 功"），
		// INSERT OR IGNORE + 唯一索引兜住。
		if _, err := s.db.sql.Exec(`
			INSERT OR IGNORE INTO banned_words
			  (id, word, raw, category, severity, enabled, note, created_by, created_at, updated_at)
			VALUES(?,?,?,?,?,1,?,'内置',?,?)`,
			newID("bw_"), norm, sw.Word, sw.Category, sw.Severity, sw.Note, now, now); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 写入拦截
// ---------------------------------------------------------------------------

// moderateText 检查若干段文本，返回**最严重**的命中。全都没命中返回 nil。
//
// fields 的键是"给用户看的字段名"（如"标题"），值是内容 —— 这样报错能说清
// "是哪一栏有问题"，而不是让用户对着一条长日程自己找。
func (s *Server) moderateText(fields map[string]string) (*WordHit, string) {
	if s.words == nil {
		return nil, ""
	}
	var best *WordHit
	bestField := ""
	for field, text := range fields {
		h := s.words.FindHit(text)
		if h == nil {
			continue
		}
		if best == nil || h.Severity > best.Severity {
			best = h
			bestField = field
		}
	}
	return best, bestField
}

// blockedByWords 是**写接口的统一闸门**。
//
// 用法：各个写 handler 在校验通过、落库之前调一次；返回 true 表示
// "已拦下并已写响应"，调用方应立即 return。
//
// 为什么放在 handler 里逐个调，而不是做成中间件：
// 中间件只能拿到原始 body，得把 JSON 解出来才知道该查哪些字段 —— 而每个接口
// 的字段名不同（标题/备注/地点/名字），解错解漏都会静默放过。
// 在 handler 里用**已经解析好的结构体**做检查，最不容易出错。
//
// ⚠️ 这个函数是**同步**的，命中风险词时会等 AI 返回（见下面）。
func (s *Server) blockedByWords(w http.ResponseWriter, r *http.Request, fields map[string]string) bool {
	hit, field := s.moderateText(fields)
	if hit == nil {
		return false
	}

	// ---- 第 2 档：直接拦，不需要问 AI ----
	// 这类词已经足够明确（"法轮功""摇头丸""代考"），问一次 AI 只是
	// 白白让用户多等几秒，而且模型有概率给出与词表相反的结论 ——
	// 那会制造"同一个词有时拦有时放"的不可解释行为。
	if hit.Severity >= sevBlock {
		apiErrDetail(w, http.StatusBadRequest, "banned_word",
			"「"+field+"」含违规词「"+hit.Raw+"」（"+wordCategoryLabel(hit.Category)+"），请修改后再保存",
			map[string]any{
				"field":    field,
				"word":     hit.Raw,
				"category": hit.Category,
				"label":    wordCategoryLabel(hit.Category),
				"severity": hit.Severity,
				"by":       "wordlist",
			})
		return true
	}

	// ---- 第 3 档：风险词 → 转 AI 复核 ----
	if hit.Severity >= sevRisk {
		return s.gateByAI(w, r, fields, field, hit)
	}

	// 第 1 档是"提醒"，不拦 —— 前端会拿同一份词表做即时提示
	return false
}

// gateByAI 把命中风险词的文本交给 AI 复核，AI 判违规则拦下。
//
// ---------------------------------------------------------------------------
// 这一段的每一个分支都是刻意的，改动前请先读懂理由
// ---------------------------------------------------------------------------
//
// **① 为什么要合并"所有字段"一起送审，而不是只送命中的那一栏？**
// 违规判断常常依赖上下文：标题写"周末活动"，备注里才是真正的问题。
// 只送命中的字段，AI 看到的是一句孤立的话，很容易判"这没问题"。
// 所以送审文本 = 全部字段拼接，但**报错时只回命中的那一栏**（用户要改的是那栏）。
//
// **② 为什么调用方要先写 "gathering" 这一步的边界？**
// 没有这一步。AI 调用是同步的、就地等结果。理由见边界 2（fail-open）：
// 失败被判为"不拦"，所以不存在"等不到结论却已经把内容拦掉"的状态。
//
// **③ 为什么 AI 判定为"违规"时要带上它给的理由和命中的字？**
// 用户有权知道为什么不让保存。只说"AI 认为违规"是最糟的形态 ——
// 用户对着自己写的东西反复看也找不出问题，只会认为是系统坏了。
// 理由 + 命中的字让他知道该改哪里，也让这个判定**可被质疑**。
//
// **④ 为什么 AI 说"合规"还要记审计？**
// 那正是漏放的高发区。只记"判违规"的话，事后复盘永远看不到真正的盲区。
func (s *Server) gateByAI(w http.ResponseWriter, r *http.Request,
	fields map[string]string, field string, hit *WordHit) bool {

	uid := userIDOf(r)
	uname := s.userNameOf(uid)

	// 送审文本：把所有非空字段拼起来（保留字段名，让模型知道哪句是标题）。
	// 排序一下让同样的输入产生同样的 prompt —— 否则 map 的随机遍历顺序
	// 会让 prompt 每次不同，同一段文本的判定就不稳定了。
	var sb strings.Builder
	for _, k := range sortedFieldKeys(fields) {
		v := strings.TrimSpace(fields[k])
		if v == "" {
			continue
		}
		sb.WriteString(k + "：" + v + "\n")
	}
	sent := sb.String()

	verdict := s.aiReviewText(r.Context(), sent, []WordHit{*hit})

	s.logModerationAudit(uid, uname, field, sent, []WordHit{*hit}, verdict)

	if verdict.violated() {
		detail := "「" + field + "」被判定为违规内容（" +
			wordCategoryLabel(verdict.Category) + "），请修改后再保存"
		if verdict.Reason != "" {
			detail += "。理由：" + verdict.Reason
		}
		apiErrDetail(w, http.StatusBadRequest, "banned_word",
			detail,
			map[string]any{
				"field":      field,
				"word":       hit.Raw, // 触发复核的那个风险词
				"category":   verdict.Category,
				"label":      wordCategoryLabel(verdict.Category),
				"severity":   hit.Severity,
				"by":         "ai",
				"ai_reason":  verdict.Reason,
				"ai_terms":   verdict.Terms,
				"confidence": verdict.Confidence,
			})
		return true
	}
	// AI 判合规 / 不可用 / 没启用 —— 一律放行（fail-open，边界 2）
	return false
}

// sortedFieldKeys 让字段顺序稳定（map 遍历顺序是随机的）。
// 顺序稳定 = 同样的输入产生同样的 prompt = 同样的判定。
func sortedFieldKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 字段数量很少（最多三四个），插入排序足够，不值得引入 sort 的复杂度
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// classifyText 只做分类，不拦截。给"提交举报时附带自动识别结果"用。
func (s *Server) classifyText(text string) []WordHit {
	if s.words == nil {
		return nil
	}
	return s.words.FindAllHits(text)
}
