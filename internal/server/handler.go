// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/checkin"
	"lobsterai2api/internal/models"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/schedule"
	"lobsterai2api/internal/stats"
	"lobsterai2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string        // 空 = 不鉴权
	MaxRotate    int           // 单请求最多换号次数，默认 3
	HardCooldown time.Duration // 余额不足冷却，默认 12h
	SoftCooldown time.Duration // 429 冷却，默认 60s
	ErrThreshold int           // 连续其他错误冷却阈值，默认 3
	ErrCooldown  time.Duration // 错误冷却时长，默认 10m
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// OAuth 为空时管理页仍可访问，但登录接口返回配置缺失。
	OAuth   *auth.OAuthClient
	AuthDir string // 管理页登录后写入凭证的目录，与 pool 扫描目录一致
	// Records 签到记录存储，供管理页查询；nil 时接口返回空列表。
	Records *checkin.Store
	// Schedule 定时设置，供管理页查看/修改；nil 时查询返回空、修改返回 503。
	Schedule *schedule.Settings
	// Stats 请求计数器，供管理页"统计"页展示；nil 时接口返回空数据。
	Stats *stats.Recorder
	// Models 运行时模型存储，供 /v1/models 直接读取；nil 时回退静态表。
	Models *models.Store
}

// Handler 主路由。
type Handler struct {
	cfg   Config
	mux   *http.ServeMux
	oauth oauthSessions // 未完成的管理页登录会话
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /api/v1/auth/me", h.withAuth(h.balance))
	h.mux.HandleFunc("GET /status", h.status)
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// 管理页与管理接口共用 /v1 的 API 密钥；页面本身不含密钥，由浏览器输入后逐次携带。
	h.mux.HandleFunc("GET /admin", h.adminIndex)
	h.mux.HandleFunc("GET /admin/api/accounts", h.withAuth(h.adminAccounts))
	h.mux.HandleFunc("POST /admin/api/accounts/refresh", h.withAuth(h.adminAccountsRefresh))
	h.mux.HandleFunc("POST /admin/api/accounts/manage", h.withAuth(h.adminAccountsManage))
	h.mux.HandleFunc("GET /admin/api/checkins", h.withAuth(h.adminCheckins))
	h.mux.HandleFunc("GET /admin/api/models", h.withAuth(h.adminModels))
	h.mux.HandleFunc("POST /admin/api/models/refresh", h.withAuth(h.adminModelsRefresh))
	h.mux.HandleFunc("GET /admin/api/stats", h.withAuth(h.adminStats))
	h.mux.HandleFunc("GET /admin/api/schedule", h.withAuth(h.adminScheduleGet))
	h.mux.HandleFunc("POST /admin/api/schedule", h.withAuth(h.adminScheduleSet))
	h.mux.HandleFunc("DELETE /admin/api/schedule", h.withAuth(h.adminScheduleReset))
	h.mux.HandleFunc("POST /admin/api/oauth/login", h.withAuth(h.adminOAuthStart))
	h.mux.HandleFunc("POST /admin/api/oauth/complete", h.withAuth(h.adminOAuthComplete))
	// 门户回调可能落到本服务（部署端口与回调端口一致时）；只提示复制地址栏，不在服务端消费 code。
	h.mux.HandleFunc("GET "+OAuthCallbackPath, h.oauthCallbackHint)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// 静态模型表不再独立维护，启动时由 models.Store 从 StaticModels 种子。
// /v1/models 直接返回 Store 中的 ID 列表，不再每次请求拉上游。
type modelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	ContextLength int    `json:"context_length"`
}

// toModelEntries 把模型 ID 包装成 OpenAI 兼容格式。
func toModelEntries(ids []string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{
			"id":             id,
			"object":         "model",
			"created":        1753600000,
			"owned_by":       "lobsterai",
			"context_length": 131072,
		})
	}
	return out
}

// models 返回当前 Store 模型的模型列表；Store 未配置时回退 StaticModels。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	ids := models.StaticModels
	if h.cfg.Models != nil {
		ids = h.cfg.Models.IDs()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   toModelEntries(ids),
	})
}

// balanceResponse 账户余额查询响应，单位 USD。
type balanceResponse struct {
	Balance float64 `json:"balance"` // 当前账户余额，单位 USD
}

// balance 查询账户余额：挑一个可用账号调上游 GET /api/v1/auth/me，原样返回 USD 余额。
// 上游不可达时回退到账号池缓存积分（按 1 积分 = 1 USD），保证接口在上游抖动时仍可返回数值。
func (h *Handler) balance(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "账号池未初始化")
		return
	}
	acct := h.cfg.Pool.Pick()
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "没有可用账号")
		return
	}
	if h.cfg.Upstream != nil {
		if v, err := h.cfg.Upstream.USDBalance(acct); err == nil {
			writeJSON(w, http.StatusOK, balanceResponse{Balance: v})
			return
		}
	}
	writeJSON(w, http.StatusOK, balanceResponse{Balance: float64(h.cfg.Pool.CreditsOf(acct.UID))})
}

// modelList 同 models，但供内部其它 handler 复用（如 adminModels）。
func (h *Handler) modelList() []map[string]any {
	ids := models.StaticModels
	if h.cfg.Models != nil {
		ids = h.cfg.Models.IDs()
	}
	return toModelEntries(ids)
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			_ = acct.SaveAtomic()
		}

		rc, status, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			lastErr = terr
			h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			if h.cfg.Stats != nil {
				h.cfg.Stats.AddFailure(peek.Model)
			}
			continue
		}
		if status >= 400 {
			kind := upstream.Classify(status, string(h.cfg.Upstream.LastBody))
			if h.cfg.Stats != nil {
				h.cfg.Stats.AddFailure(peek.Model)
			}
			switch kind {
			case upstream.ErrHardCredit:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额不足")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			case upstream.ErrSoftRate:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			case upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UID, "session dead")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			case upstream.ErrNotFound:
				// 404 短冷却不累计 errCount（防雪崩）
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			default:
				// 轮转下一个账号，不直接返回（防雪崩）
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(h.cfg.Upstream.LastBody)}
				continue
			}
		}
		defer rc.Close()
		h.cfg.Pool.NoteSuccess(acct.UID)
		if peek.Stream {
			// 流式：转发到客户端的同时捕获 usage 帧累计 tokens（不影响响应体）。
			if h.cfg.Stats != nil {
				tapModel := peek.Model
				tapErr := upstream.Stream(w, &stats.TapReader{Source: rc, OnUsage: func(prompt, completion int64) {
					h.cfg.Stats.RecordTokens(tapModel, prompt, completion)
				}})
				if tapErr != nil {
					writeOpenAIError(w, http.StatusBadGateway, "upstream_stream", tapErr.Error())
					return
				}
			} else {
				_ = upstream.Stream(w, rc)
			}
			return
		}
		resp, err := upstream.Aggregate(rc)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		if h.cfg.Stats != nil {
			prompt, completion := extractUsageTokens(resp)
			h.cfg.Stats.RecordTokens(peek.Model, prompt, completion)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// extractUsageTokens 从聚合后的 OpenAI 响应中取 usage；字段缺失或类型不符返回 0。
func extractUsageTokens(resp map[string]any) (prompt, completion int64) {
	u, _ := resp["usage"].(map[string]any)
	if u == nil {
		return 0, 0
	}
	prompt = toInt64(u["prompt_tokens"])
	completion = toInt64(u["completion_tokens"])
	return prompt, completion
}

// toInt64 把 JSON 数字（解码后是 float64）安全转 int64；其它类型返回 0。
func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	}
	return 0
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
