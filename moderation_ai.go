package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// ===========================================================================
// AI 内容复核（服务端调用，2026-09-23）
// ===========================================================================
//
// 为什么需要它：词表匹配只能抓**写死的字面量**，对下面这四类完全无能为力 ——
//
//   1. **生僻字 / 异体字**：同一句话用「㐅」「屮」「兲」这类字写出来，
//      归一化照旧，词表里没有就是抓不到。词表永远追不上。
//   2. **谐音 / 拆字 / 拼音**：「法轮功」→「珐輪功」「fa lun gong」「氵去车仑工力」。
//   3. **语义违规**：字面上一个敏感词都没有，但整句话是在拉人进群赌钱、
//      卖答案、发裸聊广告。这类只能理解语义才判得出来。
//   4. **真实场景的误伤**：「笑气」「开心水」「接码」这些词本身有正常用法
//      （化学课、饮料、快递取件码），词表一刀切会打断用户。
//
// 所以设计成**两级**：词表负责"高风险、写得出来"的硬拦截（快、零成本、
// 不依赖外部服务），AI 负责"模糊、看不懂、需理解语义"的风险判断（慢、花钱、
// 依赖外部服务）。词表里新增 severity=3 的**风险档**就是"交给 AI 看"的意思。
//
// ---------------------------------------------------------------------------
// 四条硬边界（都不是可选的）
// ---------------------------------------------------------------------------
//
// 1. **AI 只做"定性"，不做"执行"。** 它返回一个判定与理由，写不写库、
//    拦不拦由调用方按策略决定。绝不把 AI 的输出直接当 SQL 或当动作执行。
//
// 2. **AI 不可用时必须 fail-open（放行）**，绝不能 fail-closed。
//    否则 DeepSeek 一抖动，整个部署**谁都存不了日程** —— 拿可用性换
//    一个本来就有举报兜底的次要目标，是明显的错配。
//    代价（违规内容可能漏进去）由**举报 + 记录**兜底。
//
// 3. **密钥只在服务端**，永不出现在：日志、接口响应、错误信息、审计记录。
//    错误信息里连长度都不带（长度本身也是信息）。对外一律 aiMaskKey()。
//
// 4. **判断必须留痕**，无论结论是"违规"还是"合规"。理由：判"合规"的那些
//    恰恰是最需要复核的 —— 漏放的违规内容只能靠这条记录事后发现。

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// AI 配置存在 app_settings 里，键名见下。**只有管理员能改**（走 admin 接口）。
const (
	settingAIKey     = "ai_moderation_key"
	settingAIBase    = "ai_moderation_base"
	settingAIModel   = "ai_moderation_model"
	settingAIEnabled = "ai_moderation_enabled"
	settingAITimeout = "ai_moderation_timeout_ms"
)

// 默认值。模型用 flash 系（用户明确要求"默认接 DeepSeek flash API"）：
// 内容复核是**每写一次就调一次**的高频路径，用推理模型（v4-pro）
// 会让每次保存都多等好几秒，而且复核这件事不需要长链推理。
const (
	aiDefaultBase    = "https://api.deepseek.com"
	aiDefaultModel   = "deepseek-v4-flash"
	aiDefaultTimeout = 8000 // ms
	// 超时上限：再长就把用户的保存操作拖到不可接受。宁可放行 + 留记录。
	aiMaxTimeout = 30000
)

// aiConfig 是复核用的配置快照。
type aiConfig struct {
	BaseURL string
	Model   string
	APIKey  string
	Enabled bool
	Timeout time.Duration
}

// aiConfigFor 读配置。**Key 为空时 Enabled 一律为 false** ——
// 没配密钥就不可能调通，让它"看起来开着"只会让人困惑为什么没生效。
func (s *Server) aiConfigFor() aiConfig {
	key := strings.TrimSpace(s.setting(settingAIKey, ""))
	enabled := s.setting(settingAIEnabled, "0") == "1" && key != ""
	model := strings.TrimSpace(s.setting(settingAIModel, ""))
	if model == "" {
		model = aiDefaultModel
	}
	base := strings.TrimRight(strings.TrimSpace(s.setting(settingAIBase, "")), "/")
	if base == "" {
		base = aiDefaultBase
	}
	ms := atoiDefault(s.setting(settingAITimeout, ""), aiDefaultTimeout)
	if ms <= 0 {
		ms = aiDefaultTimeout
	}
	if ms > aiMaxTimeout {
		ms = aiMaxTimeout
	}
	return aiConfig{
		BaseURL: base,
		Model:   model,
		APIKey:  key,
		Enabled: enabled,
		Timeout: time.Duration(ms) * time.Millisecond,
	}
}

// aiMaskKey 把密钥变成可展示的形式。**这是唯一允许把密钥"露出来"的函数**，
// 且只露头 6 位与尾 4 位 —— 足够管理员确认"我填的是哪个"，不足以复原。
func aiMaskKey(key string) string {
	if key == "" {
		return ""
	}
	// 太短的密钥全打码，露头露尾等于全露
	if len(key) <= 12 {
		return strings.Repeat("•", len(key))
	}
	return key[:6] + "…" + key[len(key)-4:]
}

// ---------------------------------------------------------------------------
// 判定结果
// ---------------------------------------------------------------------------

// aiVerdict 是 AI 给的一次判定。
type aiVerdict struct {
	// Violation 是核心结论：true = 判为违规。
	Violation bool `json:"violation"`
	// Category 复用词表的分类口径（porn/politics/violence/gang/other），
	// 这样"AI 判的"和"词表判的"在后台能放在同一列里比较。
	Category string `json:"category"`
	// Confidence 是 0-1 的置信度。**只做展示与审计**，不作为自动拦截的判据 ——
	// 模型的自评分数往往偏乐观，按它设阈值等于把拦截权交给一个不可靠的数字。
	Confidence float64 `json:"confidence"`
	// Reason 是一句话理由（给管理员看的）。
	Reason string `json:"reason"`
	// Terms 是它认为有问题的字/词（可以多个）。管理员能据此判断
	// "是抓对了，还是把这个正常词当成了敏感词"。
	Terms []string `json:"terms"`

	// --- 下面两个是调用元信息，不属于模型输出 ---

	// Source 标记这条判定的来源：model / disabled / error。
	// error 时 Violation 一定是 false（fail-open），且 ErrorText 有值。
	Source string `json:"source"`
	// ErrorText 是失败原因（已脱敏）。只有 Source=error 时有意义。
	ErrorText string `json:"error_text"`
	// Model 记下当时用的是哪个模型（配置会变，审计要能回到当时的条件）。
	Model string `json:"model"`
	// Elapsed 是耗时（毫秒），排查"是不是每次都在超时"用。
	Elapsed int64 `json:"elapsed_ms"`
}

// violated 是"要不要拦"的唯一判据。
//
// ⚠️ 只有 Source == "model" 的判定才有拦截力。这是一个**刻意的收紧**：
// 解析失败、超时、HTTP 错误、开关关着 —— 任何"没真正拿到模型结论"的情况，
// 都退化成"不拦"。理由同上面边界 2。
func (v *aiVerdict) violated() bool {
	return v != nil && v.Source == "model" && v.Violation
}

// ---------------------------------------------------------------------------
// 提示词
// ---------------------------------------------------------------------------

// aiModerationSystem 是系统提示词。
//
// 写这段提示词时的三个取舍：
//
//  1. **必须明说"宁可放过，不可误伤"**。模型的默认倾向是"看着有点像就报违规"，
//     而这里的代价是不对称的：漏放一条违规内容，还有举报和事后审计兜底；
//     误拦一条正常内容，用户当场就存不了东西、而且无处申诉。
//
//  2. **必须让它只回 JSON**。任何寒暄、解释、markdown 围栏都会让解析变脆。
//     json_object 响应格式是另一道保险，但提示词里也要说。
//
//  3. **让它给出"命中的字"**。只回一个 true/false 的话，管理员没法判断
//     它是在认真判还是在瞎猜 —— 要求它指出具体的字词，是一次有效的自我约束。
const aiModerationSystem = `你是中文内容安全审核助手。用户会给你一句短语（来自日程的标题/备注/地点），你要判断它是否包含违法违规内容。

违规范围（只判这五类）：
- porn：色情、招嫖、裸聊、色情资源
- politics：宣扬邪教、分裂国家、暴力颠覆等违法有害信息
- violence：暴力、凶杀、自杀自残的具体实施信息、血腥内容
- gang：涉黑、毒品、赌博、诈骗、买卖违禁品、作弊器材、黑产
- other：其他明确违法违规（如买卖个人信息、非法集资）

判断时必须遵守：
1. **宁可放过，不可误伤。** 只有你能说清"它具体哪里违规"时才判违规。
   拿不准、看不懂、可能是正常用法（如化学课讲到"笑气"、快递取件码里的"接码"）→ 判不违规。
2. **注意变体写法。** 生僻字、异体字、谐音、拼音、拆字、插字符都算同一句话。
   例如"珐輪功"和"法轮功"是同一个东西。
3. **看整体语义，不只看字面。** 一句话可能一个敏感词都没有，
   但整体是在拉人进群赌钱、卖答案、卖原始股 —— 这也算违规。
4. **普通政务、历史、社会新闻的正常讨论不算违规。** 批评意见、政治观点不属于违规内容。

只输出 JSON，不要 markdown 围栏，不要任何解释文字。格式：
{"violation":true或false,"category":"porn|politics|violence|gang|other|none","confidence":0到1的小数,"reason":"一句话中文理由","terms":["命中的字或词"]}`

// ---------------------------------------------------------------------------
// 调用
// ---------------------------------------------------------------------------

// aiHTTPClient 是复核专用的 HTTP 客户端。
//
// ⚠️ **不复用默认 client**：默认 client 没有超时上限，而这里最怕的就是
// "某个请求永远挂着" —— 它会把用户的保存操作一起挂死。
// 每次调用自己带 context 超时，所以这里的 Timeout 只是一道额外的兜底。
var aiHTTPClient = &http.Client{
	Timeout: 35 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
	},
}

// aiReviewText 把一段文本交给 AI 复核。
//
// **这个函数永远不返回 nil**，也永远不返回"调用失败"作为错误 ——
// 失败会被折叠成 Source="error" 的判定（= 不拦）。调用方拿到结果就能直接判，
// 不需要额外处理错误分支。这是刻意的：把"失败"做成普通返回值，
// 才能保证调用方**不可能忘记**处理 fail-open。
func (s *Server) aiReviewText(ctx context.Context, text string, hint []WordHit) *aiVerdict {
	cfg := s.aiConfigFor()
	v := &aiVerdict{Category: "none", Model: cfg.Model}

	if !cfg.Enabled {
		v.Source = "disabled"
		v.ErrorText = "AI 复核未启用（管理员还没配密钥，或被关掉了）"
		return v
	}
	text = strings.TrimSpace(text)
	if text == "" {
		v.Source = "model" // 空文本没什么可判的，直接判合规
		return v
	}
	// 控制单次成本：复核只需要判断"有没有问题"，不需要读一整篇文章。
	// 截到 800 字，也顺带限制了 prompt 注入的篇幅。
	if r := []rune(text); len(r) > 800 {
		text = string(r[:800])
	}

	userMsg := "待审文本：\n" + text
	if len(hint) > 0 {
		// 把词表命中的词作为**线索**给它，但不许它据此直接定罪 ——
		// 这正是"风险词"档的意义：那些词本身不足以定性（如"笑气""接单"）。
		var parts []string
		for _, h := range hint {
			parts = append(parts, fmt.Sprintf("「%s」(%s)", h.Raw, wordCategoryLabel(h.Category)))
		}
		userMsg += "\n\n本地词表命中了这些词，仅供参考，**不要仅凭命中就判违规**：" +
			strings.Join(parts, "、")
	}

	body := map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": aiModerationSystem},
			{"role": "user", "content": userMsg},
		},
		"temperature":     0,   // 审核要可复现，不要创造力
		"max_tokens":      400, // 够输出那个 JSON 了，多给只是浪费
		"response_format": map[string]string{"type": "json_object"},
		"stream":          false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		v.Source, v.ErrorText = "error", "构造请求失败"
		return v
	}

	cctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		v.Source, v.ErrorText = "error", "构造请求失败"
		return v
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := aiHTTPClient.Do(req)
	v.Elapsed = time.Since(start).Milliseconds()
	if err != nil {
		// ⚠️ 这里绝不能把 err 原文塞进 ErrorText：Go 的网络错误经常把
		// 完整 URL 带出来，而 URL 里可能含查询参数形式的凭据。
		// 只按错误类型给一句人能看懂的话。
		v.Source = "error"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			v.ErrorText = fmt.Sprintf("AI 复核超时（%dms）", cfg.Timeout.Milliseconds())
		case errors.Is(cctx.Err(), context.Canceled):
			v.ErrorText = "请求被取消"
		default:
			v.ErrorText = "连不上 AI 服务（网络或地址不对）"
		}
		return v
	}
	defer resp.Body.Close()

	// 只读前 64KB：正常的 JSON 响应远小于这个数，
	// 设上限是为了防止一个异常响应把内存吃掉。
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode != http.StatusOK {
		v.Source = "error"
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			v.ErrorText = "AI 服务拒绝了这次请求（密钥不对或没有权限）"
		case http.StatusTooManyRequests:
			v.ErrorText = "AI 服务限流了（稍后自动重试就好）"
		case http.StatusNotFound:
			v.ErrorText = "接口地址不对（应该填到主域名，不要带 /chat/completions）"
		default:
			v.ErrorText = fmt.Sprintf("AI 服务返回 HTTP %d", resp.StatusCode)
		}
		return v
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil || len(parsed.Choices) == 0 {
		v.Source, v.ErrorText = "error", "AI 返回的不是预期的 JSON 结构"
		return v
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	verdict, ok := parseAIVerdict(content)
	if !ok {
		v.Source, v.ErrorText = "error", "AI 返回的结论看不懂（不是合法 JSON）"
		return v
	}
	verdict.Source = "model"
	verdict.Model = cfg.Model
	verdict.Elapsed = v.Elapsed
	return verdict
}

// parseAIVerdict 从模型输出里抠出判定。
//
// 即使开了 json_object 响应格式，仍要容忍 markdown 围栏 —— 换模型、
// 换网关、服务商降级都可能让它突然包一层 ```json。解析脆性导致的
// 后果是"全部 fallback 成放行"，静默失效，所以这里必须宽容。
func parseAIVerdict(content string) (*aiVerdict, bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, false
	}
	if i := strings.Index(content, "```"); i >= 0 {
		s := content[i+3:]
		s = strings.TrimPrefix(s, "json")
		s = strings.TrimPrefix(s, "JSON")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
		content = strings.TrimSpace(s)
	}
	// 再退一步：直接取第一个 { 到最后一个 }
	if i, j := strings.Index(content, "{"), strings.LastIndex(content, "}"); i >= 0 && j > i {
		content = content[i : j+1]
	}

	var raw struct {
		Violation  bool     `json:"violation"`
		Category   string   `json:"category"`
		Confidence float64  `json:"confidence"`
		Reason     string   `json:"reason"`
		Terms      []string `json:"terms"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return nil, false
	}

	cat := strings.ToLower(strings.TrimSpace(raw.Category))
	if cat == "" || cat == "none" {
		cat = "other"
	}
	if _, ok := wordCategoryLabels[cat]; !ok {
		cat = "other" // 认不出的分类一律归"其他"，不要让它进后台的筛选维度
	}
	// 置信度夹到 [0,1]：模型偶尔会回 95（当成百分数）或 -1
	conf := raw.Confidence
	if conf > 1 {
		conf = conf / 100
	}
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}

	terms := make([]string, 0, len(raw.Terms))
	for _, t := range raw.Terms {
		t = strings.TrimSpace(t)
		if t != "" && len(terms) < 10 {
			terms = append(terms, t)
		}
	}
	reason := strings.TrimSpace(raw.Reason)
	if r := []rune(reason); len(r) > 200 {
		reason = string(r[:200])
	}

	return &aiVerdict{
		Violation:  raw.Violation,
		Category:   cat,
		Confidence: conf,
		Reason:     reason,
		Terms:      terms,
	}, true
}

// ---------------------------------------------------------------------------
// 审计记录
// ---------------------------------------------------------------------------

// moderationAuditRow 是 AI 一次复核的审计记录。
//
// 为什么要**连"判合规"的也记**：这些恰恰是漏放的高发区。
// 只记"判违规"的话，事后复盘的永远是已经处理过的那些，看不到真正的盲区。
type moderationAuditRow struct {
	ID         string   `json:"id"`
	Field      string   `json:"field"`
	Excerpt    string   `json:"excerpt"`
	Hits       []string `json:"hits"`
	Violation  bool     `json:"violation"`
	Category   string   `json:"category"`
	Confidence float64  `json:"confidence"`
	Reason     string   `json:"reason"`
	Source     string   `json:"source"`
	ErrorText  string   `json:"error_text"`
	Model      string   `json:"model"`
	Elapsed    int64    `json:"elapsed_ms"`
	UserID     string   `json:"user_id"`
	UserName   string   `json:"user_name"`
	CreatedAt  int64    `json:"created_at"`
}

// logModerationAudit 落一条审计。**失败只记日志，不向上报错** ——
// 审计写不进去不该让用户的保存操作失败。
func (s *Server) logModerationAudit(uid, uname, field, excerpt string, hits []WordHit, v *aiVerdict) {
	if v == nil {
		return
	}
	terms := make([]string, 0, len(hits))
	for _, h := range hits {
		terms = append(terms, h.Raw)
	}
	ex := []rune(strings.TrimSpace(excerpt))
	if len(ex) > 300 {
		ex = ex[:300]
	}
	_, err := s.db.sql.Exec(`
		INSERT INTO moderation_audit
		  (id, field, excerpt, hits, violation, category, confidence, reason,
		   source, error_text, model, elapsed_ms, user_id, user_name, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID("ma_"), field, string(ex), strings.Join(terms, "|"),
		boolToInt(v.Violation), v.Category, v.Confidence, v.Reason,
		v.Source, v.ErrorText, v.Model, v.Elapsed, uid, uname, time.Now().Unix())
	if err != nil {
		log.Printf("moderation: 写审计失败: %v", err)
	}
}
