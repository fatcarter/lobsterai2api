// admin.go 管理页与 OAuth 登录接口：生成门户授权地址，再由用户粘贴登录后的返回地址完成账号添加。
//
// 关键业务流程（登录属于核心链路，日志覆盖全流程）：
//
//	POST /admin/api/oauth/login    生成一次性 state 与授权地址，会话仅存内存
//	POST /admin/api/oauth/complete 校验 state → 上游换取凭证 → 落盘 auths/ → 加入账号池
//
// 服务本身不监听 OAuth 回调；授权地址中的 127.0.0.1 回调只用于让门户把 code/state 带回浏览器地址栏。
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/checkin"
	"lobsterai2api/internal/models"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/schedule"
	"lobsterai2api/internal/stats"
)

//go:embed admin.html
var adminPageHTML []byte

// OAuthCallbackPath 是授权地址中回调路径，与命令行登录保持一致：
// 门户校验 redirect_uri 必须是 http://127.0.0.1:{port}/auth/callback，换成别的路径会被拒绝。
const OAuthCallbackPath = "/auth/callback"

const (
	// oauthSessionTTL 授权会话有效期，超时后必须重新生成授权地址，避免长期驻留可用的 state。
	oauthSessionTTL = 10 * time.Minute
	// maxOAuthSessions 并发保留的授权会话上限，防止反复生成授权地址耗尽内存。
	maxOAuthSessions = 32
	// maxReturnURLLen 允许粘贴的返回地址长度上限。
	maxReturnURLLen = 4096
	// maxAdminBodyLen 管理接口请求体上限。
	maxAdminBodyLen = 16 << 10
)

// oauthSession 单个未完成的授权会话。
type oauthSession struct {
	login     auth.OAuthLogin // 授权地址、state 与安装 UUID
	expiresAt time.Time       // 过期时间，到期后不可再用于交换
}

// oauthSessions 内存中的授权会话表；进程重启后需重新生成授权地址。
type oauthSessions struct {
	mu   sync.Mutex
	byID map[string]oauthSession
}

// put 登记新会话，同时清理过期项；会话数超限时拒绝，避免无限增长。
func (s *oauthSessions) put(login auth.OAuthLogin, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[string]oauthSession{}
	}
	for id, sess := range s.byID {
		if now.After(sess.expiresAt) {
			delete(s.byID, id)
		}
	}
	if len(s.byID) >= maxOAuthSessions {
		return errors.New("待完成的登录过多，请先完成或等待现有登录过期")
	}
	s.byID[login.ID] = oauthSession{login: login, expiresAt: now.Add(oauthSessionTTL)}
	return nil
}

// take 取出并消费会话；授权码只允许使用一次，取出即删除。
func (s *oauthSessions) take(id string, now time.Time) (auth.OAuthLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return auth.OAuthLogin{}, false
	}
	delete(s.byID, id)
	if now.After(sess.expiresAt) {
		return auth.OAuthLogin{}, false
	}
	return sess.login, true
}

// oauthStartResponse 生成授权地址的响应；不含 state，页面只需回传 login_id。
type oauthStartResponse struct {
	LoginID          string `json:"login_id"`          // 后续完成登录时的会话标识
	AuthorizationURL string `json:"authorization_url"` // 浏览器打开的门户授权地址
	RedirectURI      string `json:"redirect_uri"`      // 门户登录后跳转的回调地址，用于提示用户复制哪个地址
	ExpiresAt        int64  `json:"expires_at"`        // 会话过期时间（Unix 秒）
}

// oauthCompleteRequest 粘贴返回地址完成登录的请求体。
type oauthCompleteRequest struct {
	LoginID   string `json:"login_id"`   // 生成授权地址时返回的会话标识
	ReturnURL string `json:"return_url"` // 登录完成后浏览器地址栏的完整地址
}

// oauthCompleteResponse 登录结果；只返回账号信息，绝不返回任何令牌。
type oauthCompleteResponse struct {
	UID       string        `json:"uid"`                  // 账号唯一标识
	Nickname  string        `json:"nickname,omitempty"`   // 账号昵称
	ExpiresAt int64         `json:"expires_at,omitempty"` // 访问令牌过期时间（Unix 秒）
	Accounts  []pool.Status `json:"accounts"`             // 最新账号池状态，便于页面直接刷新列表
}

// accountsResponse 账号列表响应，与 /status 保持同一结构。
type accountsResponse struct {
	Accounts []pool.Status `json:"accounts"`
}

// accountRefreshEntry 单账号的余额刷新结果。
type accountRefreshEntry struct {
	UID      string `json:"uid"`               // 账号 UID
	Nickname string `json:"nickname,omitempty"`
	Status   string `json:"status"`            // ok / error / skipped
	Credits  int64  `json:"credits,omitempty"` // ok 时携带最新余额
	Message  string `json:"message,omitempty"` // error / skipped 时的原因
}

// checkinsResponse 签到记录分页响应：page 从 1 开始，size 为每页条数。
type checkinsResponse struct {
	Records []checkin.Record `json:"records"` // 当前页的记录，最新在前
	Total   int              `json:"total"`   // 总记录数（管理页渲染分页用）
	Page    int              `json:"page"`    // 当前页码
	Size    int              `json:"size"`    // 当前页大小
}

// scheduleRequest/Response 定时设置的查询与保存载荷；小时为空列表表示关闭，间隔为空表示关闭。
type scheduleRequest struct {
	CheckinHours          []int   `json:"checkin_hours"`
	KeepaliveHours        []int   `json:"keepalive_hours"`
	CreditRefreshInterval *string `json:"credit_refresh_interval,omitempty"`
	ModelRefreshHours     []int   `json:"model_refresh_hours"`
}

type scheduleResponse struct {
	CheckinHours          []int  `json:"checkin_hours"`
	KeepaliveHours        []int  `json:"keepalive_hours"`
	CreditRefreshInterval string `json:"credit_refresh_interval"`
	ModelRefreshHours     []int  `json:"model_refresh_hours"`
}

// scheduleResponseOf 用当前生效值构造响应。
func scheduleResponseOf(s *schedule.Settings) scheduleResponse {
	resp := scheduleResponse{}
	if s != nil {
		var interval time.Duration
		resp.CheckinHours, resp.KeepaliveHours, resp.ModelRefreshHours, interval = s.Get()
		resp.CreditRefreshInterval = schedule.FormatInterval(interval)
	}
	return resp
}

// adminIndex 返回内置管理页；页面不含密钥，全部管理操作由页面携带 Bearer 密钥调用接口完成。
func (h *Handler) adminIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(adminPageHTML)
}

// oauthCallbackHint 门户回调落到本服务时的提示页；仅静态文案，不回显任何请求参数。
func (h *Handler) oauthCallbackHint(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = io.WriteString(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width, initial-scale=1"><title>复制返回地址</title></head>`+
		`<body style="font:16px/1.6 system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem">`+
		`<h1 style="font-size:1.25rem">登录完成</h1>`+
		`<p>复制浏览器地址栏中的完整地址，粘贴回管理页的“返回地址”后提交，即可添加账号。</p>`+
		`</body></html>`)
}

// accountRefreshResponse 手动刷新余额的响应：每个账号的结果 + 最新账号池状态。
type accountRefreshResponse struct {
	Accounts []pool.Status             `json:"accounts"`            // 最新账号池状态，便于页面直接刷新列表
	Results  []accountRefreshEntry     `json:"results,omitempty"`   // 每个账号的刷新结果，便于页面提示错误
}

// adminAccountsRefresh 主动遍历账号池，对每个非禁用账号调用 QuotaUsage 更新余额；
// 余额 > 0 时自动解冻冷却。返回最新账号状态 + 每账号刷新结果。
func (h *Handler) adminAccountsRefresh(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil || h.cfg.Upstream == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "not_configured", "账号池或上游客户端未初始化")
		return
	}
	resp := accountRefreshResponse{Accounts: h.cfg.Pool.List()}
	for _, st := range h.cfg.Pool.List() {
		entry := accountRefreshEntry{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			entry.Status = "skipped"
			entry.Message = "账号已禁用，跳过刷新"
			resp.Results = append(resp.Results, entry)
			continue
		}
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			entry.Status = "error"
			entry.Message = "缺少访问令牌"
			resp.Results = append(resp.Results, entry)
			continue
		}
		remain, _, err := h.cfg.Upstream.QuotaUsage(a)
		if err != nil {
			entry.Status = "error"
			entry.Message = err.Error()
			log.Printf("管理页刷新余额 %s: %v", st.UID, err)
		} else {
			h.cfg.Pool.ReenableIfCredits(st.UID, remain)
			entry.Status = "ok"
			entry.Credits = remain
			log.Printf("管理页刷新余额 %s: credits=%d", st.UID, remain)
		}
		resp.Results = append(resp.Results, entry)
	}
	resp.Accounts = h.cfg.Pool.List()
	writeJSON(w, http.StatusOK, resp)
}

// adminAccounts 返回账号池状态（脱敏），供管理页刷新列表。
func (h *Handler) adminAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, accountsResponse{Accounts: h.cfg.Pool.List()})
}

// accountsManageRequest 管理动作：uid 数组 + 单个动作（enable/disable/cooldown）。
// action = enable: 取消禁用并解除冷却。
// action = disable: 立即禁用账号（不可用直到重新启用）。
// action = cooldown: 手动加入冷却，时长来自 cooldown_seconds（默认 600s，最小 1s）。
type accountsManageRequest struct {
	UIDs            []string `json:"uids"`
	Action          string   `json:"action"`
	CooldownSeconds int      `json:"cooldown_seconds,omitempty"`
}

// accountsManageResponse 动作结果汇总。
type accountsManageResponse struct {
	Accounts []pool.Status `json:"accounts"`
	Applied  int           `json:"applied"`
	Skipped  int           `json:"skipped"`
	Message  string        `json:"message,omitempty"`
}

// adminAccountsManage 批量启用/禁用/冷却账号；uid 数组为空时作用于全部账号。
func (h *Handler) adminAccountsManage(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "not_configured", "账号池未初始化")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBodyLen+1))
	if err != nil || len(raw) > maxAdminBodyLen {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	var req accountsManageRequest
	if json.Unmarshal(raw, &req) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是有效的 JSON")
		return
	}
	switch req.Action {
	case "enable", "disable", "cooldown":
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_action", "action 必须为 enable / disable / cooldown")
		return
	}
	var cooldown time.Duration
	if req.Action == "cooldown" {
		secs := req.CooldownSeconds
		if secs <= 0 {
			secs = 600
		}
		if secs < 1 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_cooldown", "cooldown_seconds 不能小于 1")
			return
		}
		cooldown = time.Duration(secs) * time.Second
	}

	targets := make(map[string]bool, len(req.UIDs))
	for _, u := range req.UIDs {
		if u != "" {
			targets[u] = true
		}
	}

	resp := accountsManageResponse{}
	for _, st := range h.cfg.Pool.List() {
		if len(targets) > 0 && !targets[st.UID] {
			continue
		}
		switch req.Action {
		case "enable":
			h.cfg.Pool.Enable(st.UID)
		case "disable":
			h.cfg.Pool.Disable(st.UID, "manually disabled")
		case "cooldown":
			h.cfg.Pool.ForceCooldown(st.UID, "manually cooled", cooldown)
		}
		resp.Applied++
	}
	resp.Accounts = h.cfg.Pool.List()
	resp.Message = fmt.Sprintf("已对 %d 个账号执行 %s", resp.Applied, req.Action)
	log.Printf("管理页账号管理：action=%s applied=%d cooldown=%s", req.Action, resp.Applied, cooldown)
	writeJSON(w, http.StatusOK, resp)
}

// adminCheckins 返回签到记录分页（最新在前），供管理页表格 + 分页器使用。
// 分页参数：?page=N（默认 1）&size=N（默认 20，最大 200）；超出范围时夹紧。
func (h *Handler) adminCheckins(w http.ResponseWriter, r *http.Request) {
	page, size := parsePagination(r, 1, 20, 200)
	resp := checkinsResponse{Page: page, Size: size}
	if h.cfg.Records != nil {
		var exclude []string
		if r.URL.Query().Get("hide_claimed") == "1" {
			exclude = append(exclude, "already_claimed")
		}
		resp.Records, resp.Total = h.cfg.Records.Page(page, size, exclude...)
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminModels 返回当前可用模型 + 最后刷新时间；管理页 Models 页使用。
func (h *Handler) adminModels(w http.ResponseWriter, r *http.Request) {
	items := models.StaticModelsItems()
	if h.cfg.Models != nil {
		items = h.cfg.Models.Items()
	}
	var refreshedAt int64
	if h.cfg.Models != nil {
		if t := h.cfg.Models.LastRefreshed(); !t.IsZero() {
			refreshedAt = t.Unix()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":          items,
		"last_refreshed": refreshedAt,
	})
}

// adminModelsRefresh 触发后台拉上游刷新模型列表；返回新的元数据 + 刷新时间戳。
func (h *Handler) adminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Models == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "models_not_configured", "模型存储未初始化")
		return
	}
	if h.cfg.Pool == nil || h.cfg.Pool.Pick() == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "无可用账号，无法拉上游模型")
		return
	}
	acct := h.cfg.Pool.Pick()
	items, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(items) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "model_refresh_failed", err.Error())
		return
	}
	h.cfg.Models.Set(items)
	log.Printf("admin: 模型列表已刷新，共 %d 个", len(items))
	writeJSON(w, http.StatusOK, map[string]any{
		"items":          items,
		"last_refreshed": h.cfg.Models.LastRefreshed().Unix(),
		"count":          len(items),
	})
}

// adminStats 返回累计请求统计（管理页 Stats 页使用）；Stats 为 nil 时返回零值快照。
func (h *Handler) adminStats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Stats == nil {
		writeJSON(w, http.StatusOK, stats.Snapshot{})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Stats.Report())
}

// parsePagination 从 query 解析分页参数；缺省或非法值回退到 defaultPage / defaultSize，
// size 超出 maxSize 时夹紧到 maxSize。
func parsePagination(r *http.Request, defaultPage, defaultSize, maxSize int) (int, int) {
	page := defaultPage
	size := defaultSize
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := r.URL.Query().Get("size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			size = n
		}
	}
	if size > maxSize {
		size = maxSize
	}
	return page, size
}

// adminScheduleGet 返回当前生效的定时设置。
func (h *Handler) adminScheduleGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, scheduleResponseOf(h.cfg.Schedule))
}

// adminScheduleSet 保存定时设置：校验后写入持久化文件，调度器立即按新时间执行。
func (h *Handler) adminScheduleSet(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Schedule == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "schedule_not_configured", "服务未启用定时设置")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBodyLen+1))
	if err != nil || len(raw) > maxAdminBodyLen {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	var req scheduleRequest
	if json.Unmarshal(raw, &req) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是有效的 JSON")
		return
	}
	if err := schedule.ValidateHours(req.CheckinHours); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_hours", "签到时间："+err.Error())
		return
	}
	if err := schedule.ValidateHours(req.KeepaliveHours); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_hours", "保活时间："+err.Error())
		return
	}
	if err := schedule.ValidateHours(req.ModelRefreshHours); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_hours", "模型刷新时间："+err.Error())
		return
	}
	_, _, _, interval := h.cfg.Schedule.Get()
	if req.CreditRefreshInterval != nil {
		parsed, err := schedule.ParseInterval(*req.CreditRefreshInterval)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_interval", "额度刷新间隔："+err.Error())
			return
		}
		interval = parsed
	}
	h.cfg.Schedule.Set(req.CheckinHours, req.KeepaliveHours, req.ModelRefreshHours, interval)
	log.Printf("管理页定时设置：已保存 checkin=%v keepalive=%v credit_refresh=%s",
		req.CheckinHours, req.KeepaliveHours, schedule.FormatInterval(interval))
	writeJSON(w, http.StatusOK, scheduleResponseOf(h.cfg.Schedule))
}

// adminScheduleReset 删除持久化文件并恢复配置文件的默认定时设置。
func (h *Handler) adminScheduleReset(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Schedule == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "schedule_not_configured", "服务未启用定时设置")
		return
	}
	if err := h.cfg.Schedule.Reset(); err != nil {
		log.Printf("管理页定时设置：恢复默认失败: %v", err)
		writeOpenAIError(w, http.StatusInternalServerError, "schedule_reset_failed", err.Error())
		return
	}
	log.Printf("管理页定时设置：已恢复默认")
	writeJSON(w, http.StatusOK, scheduleResponseOf(h.cfg.Schedule))
}

// adminOAuthStart 创建授权会话并返回门户授权地址。
func (h *Handler) adminOAuthStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.OAuth == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "oauth_not_configured", auth.ErrOAuthConfig.Error())
		return
	}
	now := time.Now()
	login, err := h.cfg.OAuth.NewLogin(now)
	if err != nil {
		if errors.Is(err, auth.ErrOAuthConfig) {
			writeOpenAIError(w, http.StatusServiceUnavailable, "oauth_not_configured", err.Error())
			return
		}
		log.Printf("管理页登录：生成授权会话失败: %v", err)
		writeOpenAIError(w, http.StatusInternalServerError, "oauth_start_failed", "生成授权地址失败，请重试")
		return
	}
	if err := h.oauth.put(*login, now); err != nil {
		writeOpenAIError(w, http.StatusTooManyRequests, "too_many_logins", err.Error())
		return
	}
	log.Printf("管理页登录：已创建授权会话 login_id=%s，有效期 %s", login.ID, oauthSessionTTL)
	writeJSON(w, http.StatusOK, oauthStartResponse{
		LoginID:          login.ID,
		AuthorizationURL: login.AuthorizationURL,
		RedirectURI:      login.RedirectURI,
		ExpiresAt:        now.Add(oauthSessionTTL).Unix(),
	})
}

// adminOAuthComplete 用粘贴的返回地址完成登录：校验 state → 交换凭证 → 落盘 → 加入账号池。
func (h *Handler) adminOAuthComplete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.OAuth == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "oauth_not_configured", auth.ErrOAuthConfig.Error())
		return
	}
	if h.cfg.AuthDir == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "auth_dir_missing", "服务未配置账号目录，无法保存凭证")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBodyLen+1))
	if err != nil || len(raw) > maxAdminBodyLen {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	var req oauthCompleteRequest
	if json.Unmarshal(raw, &req) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是有效的 JSON")
		return
	}
	code, state, err := parseOAuthReturn(req.ReturnURL)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_return_url", err.Error())
		return
	}
	login, ok := h.oauth.take(strings.TrimSpace(req.LoginID), time.Now())
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "login_expired", "登录会话不存在或已过期，请重新生成授权地址")
		return
	}
	// state 必须来自本次会话，避免其他会话或他人的返回地址被误用。
	if subtle.ConstantTimeCompare([]byte(login.State), []byte(state)) != 1 {
		log.Printf("管理页登录：返回地址 state 不匹配 login_id=%s", login.ID)
		writeOpenAIError(w, http.StatusBadRequest, "state_mismatch", "返回地址与本次登录不匹配，请重新生成授权地址")
		return
	}
	// 授权码只能用一次，页面断开也要把交换做完，避免凭证悬空。
	acct, err := h.cfg.OAuth.Exchange(context.WithoutCancel(r.Context()), login, code)
	if err != nil {
		log.Printf("管理页登录：授权交换失败 login_id=%s: %v", login.ID, err)
		writeOpenAIError(w, http.StatusBadGateway, "oauth_exchange_failed", err.Error())
		return
	}
	if err := h.saveAccount(acct); err != nil {
		log.Printf("管理页登录：保存账号凭证失败 uid=%s: %v", acct.UID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "auth_save_failed", "保存账号凭证失败，请检查账号目录权限")
		return
	}
	h.cfg.Pool.Add(acct)
	log.Printf("管理页登录：账号已加入池 uid=%s file=%s", acct.UID, acct.FilePath)
	writeJSON(w, http.StatusOK, oauthCompleteResponse{
		UID:       acct.UID,
		Nickname:  acct.Nickname,
		ExpiresAt: acct.ExpiresAt,
		Accounts:  h.cfg.Pool.List(),
	})
}

// saveAccount 将新凭证写入账号目录，文件名沿用 lobsterai-<uid>.json 以便重启后自动加载。
func (h *Handler) saveAccount(a *auth.Auth) error {
	if err := os.MkdirAll(h.cfg.AuthDir, 0o700); err != nil {
		return err
	}
	a.FilePath = filepath.Join(h.cfg.AuthDir, "lobsterai-"+authFileKey(a.UID)+".json")
	return a.SaveAtomic()
}

// authFileKey 生成安全的文件名片段：uid 由上游返回，含分隔符或超长时改用其哈希，避免越出账号目录。
func authFileKey(uid string) string {
	safe := uid != "" && len(uid) <= 64
	for _, r := range uid {
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		safe = false
		break
	}
	if safe {
		return uid
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(uid)))[:16]
}

// parseOAuthReturn 从粘贴的返回地址提取授权码和 state；门户可能用查询串或哈希路由携带参数，两种都接受。
func parseOAuthReturn(raw string) (code, state string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("请粘贴登录完成后浏览器地址栏中的完整地址")
	}
	if len(raw) > maxReturnURLLen {
		return "", "", errors.New("返回地址过长，请确认只粘贴一个地址")
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "", "", errors.New("返回地址格式无效")
	}
	query := u.Query()
	code, state = query.Get("code"), query.Get("state")
	if code == "" || state == "" {
		// 门户使用 /portal#/login 形式的哈希路由，登录结果可能落在片段里。
		if frag := u.Fragment; frag != "" {
			if i := strings.IndexByte(frag, '?'); i >= 0 {
				frag = frag[i+1:]
			}
			if fragQuery, fragErr := url.ParseQuery(frag); fragErr == nil {
				if code == "" {
					code = fragQuery.Get("code")
				}
				if state == "" {
					state = fragQuery.Get("state")
				}
			}
		}
	}
	if code == "" || state == "" {
		return "", "", errors.New("返回地址缺少 code 或 state 参数，请复制完整地址")
	}
	if len(code) > 512 || len(state) > 512 ||
		strings.ContainsAny(code, " \t\r\n") || strings.ContainsAny(state, " \t\r\n") {
		return "", "", errors.New("返回地址中的登录参数无效，请重新登录")
	}
	return code, state, nil
}
