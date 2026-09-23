package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/checkin"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/schedule"
	"lobsterai2api/internal/upstream"
)

func TestParseOAuthReturn(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantCode    string
		wantState   string
		wantErrLike string
	}{
		{name: "查询串", raw: "http://127.0.0.1:8367/auth/callback?code=abc&state=s1", wantCode: "abc", wantState: "s1"},
		{name: "哈希路由", raw: "http://portal.example/portal#/callback?code=abc&state=s1", wantCode: "abc", wantState: "s1"},
		{name: "片段无问号", raw: "http://portal.example/p#code=abc&state=s1", wantCode: "abc", wantState: "s1"},
		{name: "查询串补片段", raw: "http://127.0.0.1:8367/auth/callback?code=abc#/x?state=s1", wantCode: "abc", wantState: "s1"},
		{name: "空地址", raw: "   ", wantErrLike: "完整地址"},
		{name: "缺少参数", raw: "http://127.0.0.1:8367/auth/callback?code=abc", wantErrLike: "缺少 code 或 state"},
		{name: "超长地址", raw: "http://127.0.0.1/?code=a&state=" + strings.Repeat("x", maxReturnURLLen), wantErrLike: "过长"},
		{name: "参数超长", raw: "http://127.0.0.1/?code=" + strings.Repeat("y", 513) + "&state=s1", wantErrLike: "登录参数无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, state, err := parseOAuthReturn(tc.raw)
			if tc.wantErrLike != "" {
				if err == nil {
					t.Fatalf("期望报错包含 %q，实际成功 code=%q state=%q", tc.wantErrLike, code, state)
				}
				if !strings.Contains(err.Error(), tc.wantErrLike) {
					t.Fatalf("错误信息 %q 不包含 %q", err.Error(), tc.wantErrLike)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if code != tc.wantCode || state != tc.wantState {
				t.Fatalf("得到 code=%q state=%q，期望 code=%q state=%q", code, state, tc.wantCode, tc.wantState)
			}
		})
	}
}

func TestOAuthSessionsTakeConsumesOnce(t *testing.T) {
	var s oauthSessions
	now := time.Now()
	if err := s.put(auth.OAuthLogin{ID: "a", State: "s"}, now); err != nil {
		t.Fatalf("登记会话失败: %v", err)
	}
	if _, ok := s.take("a", now); !ok {
		t.Fatal("首次取用应成功")
	}
	if _, ok := s.take("a", now); ok {
		t.Fatal("授权会话必须一次性消费")
	}
}

func TestOAuthSessionsRejectsExpired(t *testing.T) {
	var s oauthSessions
	now := time.Now()
	if err := s.put(auth.OAuthLogin{ID: "a"}, now); err != nil {
		t.Fatalf("登记会话失败: %v", err)
	}
	if _, ok := s.take("a", now.Add(oauthSessionTTL+time.Second)); ok {
		t.Fatal("过期会话不应可用")
	}
}

func TestOAuthSessionsLimitAndExpiryCleanup(t *testing.T) {
	var s oauthSessions
	now := time.Now()
	for i := 0; i < maxOAuthSessions; i++ {
		if err := s.put(auth.OAuthLogin{ID: string(rune('a' + i))}, now); err != nil {
			t.Fatalf("第 %d 个会话登记失败: %v", i, err)
		}
	}
	if err := s.put(auth.OAuthLogin{ID: "overflow"}, now); err == nil {
		t.Fatal("超出上限时应拒绝新会话")
	}
	// 过期会话在下一次登记时清理，因此让出容量后可以继续登录。
	if err := s.put(auth.OAuthLogin{ID: "later"}, now.Add(oauthSessionTTL+time.Second)); err != nil {
		t.Fatalf("清理过期会话后应可登记: %v", err)
	}
}

func TestAuthFileKey(t *testing.T) {
	cases := map[string]bool{
		"user-123_A":            true,  // 允许原样作为文件名
		"":                      false, // 空 uid 退回哈希
		"../../etc/passwd":      false,
		"用户":                    false,
		strings.Repeat("u", 65): false,
	}
	for uid, keepAsIs := range cases {
		key := authFileKey(uid)
		if keepAsIs {
			if key != uid {
				t.Fatalf("uid %q 应原样使用，得到 %q", uid, key)
			}
			continue
		}
		if key == uid {
			t.Fatalf("uid %q 必须改用哈希", uid)
		}
		if len(key) != 16 || strings.ContainsAny(key, "/.\\") {
			t.Fatalf("uid %q 的哈希文件名不安全: %q", uid, key)
		}
	}
}

// newTestHandler 构建带假上游的 handler；假上游只实现授权交换接口。
func newTestHandler(t *testing.T, apiKey string) (*Handler, string, *pool.Pool) {
	t.Helper()
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/exchange" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at-1","refreshToken":"rt-1",` +
			`"expiresIn":3600,"user":{"id":"uid-1","userId":"yid-1","nickname":"张三"}}}`))
	}))
	t.Cleanup(fakeUpstream.Close)

	authDir := filepath.Join(t.TempDir(), "auths")
	p := pool.New("")
	h := NewHandler(Config{
		Pool:     p,
		Upstream: upstream.New(),
		APIKey:   apiKey,
		OAuth: &auth.OAuthClient{
			BaseURL:     fakeUpstream.URL,
			PortalURL:   fakeUpstream.URL,
			RedirectURI: "http://127.0.0.1:8367" + OAuthCallbackPath,
		},
		AuthDir: authDir,
	})
	return h, authDir, p
}

// startLogin 调用生成授权地址接口并返回会话标识与 state。
func startLogin(t *testing.T, h *Handler, apiKey string) (loginID, state string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/login", nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("生成授权地址失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp oauthStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.LoginID == "" || resp.AuthorizationURL == "" {
		t.Fatalf("响应缺少会话标识或授权地址: %s", rec.Body.String())
	}
	u, err := url.Parse(resp.AuthorizationURL)
	if err != nil {
		t.Fatalf("授权地址无效: %v", err)
	}
	frag := u.Fragment
	if i := strings.IndexByte(frag, '?'); i >= 0 {
		frag = frag[i+1:]
	}
	q, err := url.ParseQuery(frag)
	if err != nil {
		t.Fatalf("授权地址查询串无效: %v", err)
	}
	if got := q.Get("redirect_uri"); got != "http://127.0.0.1:8367"+OAuthCallbackPath {
		t.Fatalf("回调地址被改写: %q", got)
	}
	if q.Get("state") == "" {
		t.Fatal("授权地址缺少 state")
	}
	// 响应体不应带出 state，只能从授权地址中取得。
	if strings.Contains(rec.Body.String(), `"state"`) {
		t.Fatalf("响应体不应包含 state: %s", rec.Body.String())
	}
	return resp.LoginID, q.Get("state")
}

func completeLogin(t *testing.T, h *Handler, apiKey, loginID, returnURL string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(oauthCompleteRequest{LoginID: loginID, ReturnURL: returnURL})
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/complete", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdminOAuthLoginFlowAddsAccount(t *testing.T) {
	const apiKey = "sk-test"
	h, authDir, p := newTestHandler(t, apiKey)
	loginID, state := startLogin(t, h, apiKey)

	rec := completeLogin(t, h, apiKey, loginID, "http://127.0.0.1:8367"+OAuthCallbackPath+"?code=ac-1&state="+state)
	if rec.Code != http.StatusOK {
		t.Fatalf("完成登录失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp oauthCompleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.UID != "uid-1" || resp.Nickname != "张三" {
		t.Fatalf("账号信息不正确: %+v", resp)
	}
	if len(resp.Accounts) != 1 || resp.Accounts[0].UID != "uid-1" {
		t.Fatalf("响应未返回最新账号池: %+v", resp.Accounts)
	}
	// 响应绝不能带出令牌。
	if body := rec.Body.String(); strings.Contains(body, "at-1") || strings.Contains(body, "rt-1") {
		t.Fatalf("响应泄露令牌: %s", body)
	}
	// 凭证落盘到 pool 扫描的文件名，重启后可自动加载。
	fp := filepath.Join(authDir, "lobsterai-uid-1.json")
	info, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("凭证未落盘: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("凭证文件权限应为 600，实际 %o", perm)
	}
	loaded, err := auth.LoadDir(authDir)
	if err != nil || len(loaded) != 1 || loaded[0].AccessToken != "at-1" {
		t.Fatalf("落盘文件无法被 LoadDir 解析: err=%v loaded=%+v", err, loaded)
	}
	if p.AuthByUID("uid-1") == nil {
		t.Fatal("账号未加入运行中的账号池")
	}
	// 授权码一次性，重复提交同一返回地址必须失败。
	if again := completeLogin(t, h, apiKey, loginID, "http://127.0.0.1:8367"+OAuthCallbackPath+"?code=ac-1&state="+state); again.Code == http.StatusOK {
		t.Fatal("重复使用同一授权会话应失败")
	}
}

func TestAdminOAuthCompleteRejectsForeignState(t *testing.T) {
	const apiKey = "sk-test"
	h, authDir, p := newTestHandler(t, apiKey)
	loginID, _ := startLogin(t, h, apiKey)

	rec := completeLogin(t, h, apiKey, loginID, "http://127.0.0.1:8367"+OAuthCallbackPath+"?code=ac-1&state=other-session")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state 不匹配应返回 400，实际 HTTP %d %s", rec.Code, rec.Body.String())
	}
	if len(p.List()) != 0 {
		t.Fatal("state 不匹配时不应添加账号")
	}
	if entries, err := os.ReadDir(authDir); err == nil && len(entries) != 0 {
		t.Fatalf("state 不匹配时不应落盘凭证: %d 个文件", len(entries))
	}
	// state 不匹配同样消费会话，必须重新生成授权地址。
	if again := completeLogin(t, h, apiKey, loginID, "http://127.0.0.1:8367"+OAuthCallbackPath+"?code=ac-1&state=x"); again.Code != http.StatusBadRequest {
		t.Fatalf("会话应已作废，实际 HTTP %d", again.Code)
	}
}

func TestAdminOAuthCompleteRejectsUnknownSession(t *testing.T) {
	const apiKey = "sk-test"
	h, _, _ := newTestHandler(t, apiKey)
	rec := completeLogin(t, h, apiKey, "does-not-exist", "http://127.0.0.1:8367"+OAuthCallbackPath+"?code=ac-1&state=s")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知会话应返回 400，实际 HTTP %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminEndpointsRequireAPIKey(t *testing.T) {
	const apiKey = "sk-test"
	h, _, _ := newTestHandler(t, apiKey)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/accounts"},
		{http.MethodPost, "/admin/api/accounts/refresh"},
		{http.MethodPost, "/admin/api/accounts/manage"},
		{http.MethodPost, "/admin/api/oauth/login"},
		{http.MethodPost, "/admin/api/oauth/complete"},
		{http.MethodGet, "/admin/api/schedule"},
		{http.MethodPost, "/admin/api/schedule"},
		{http.MethodDelete, "/admin/api/schedule"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未鉴权应返回 401，实际 HTTP %d", tc.method, tc.path, rec.Code)
		}
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer wrong-key")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 错误密钥应返回 401，实际 HTTP %d", tc.method, tc.path, rec.Code)
		}
	}
	// 管理页本身不含密钥，允许匿名打开后在页面里输入。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("管理页应可打开，实际 HTTP %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), apiKey) {
		t.Fatal("管理页不应包含 API 密钥")
	}
}

func TestAdminOAuthUnconfigured(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), AuthDir: t.TempDir()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/oauth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置门户时应返回 503，实际 HTTP %d %s", rec.Code, rec.Body.String())
	}
}

func TestOAuthCallbackHintLeaksNoParams(t *testing.T) {
	h, _, _ := newTestHandler(t, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, OAuthCallbackPath+"?code=secret-code&state=secret-state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("回调提示页应返回 200，实际 HTTP %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "secret-code") || strings.Contains(body, "secret-state") {
		t.Fatalf("提示页回显了登录参数: %s", body)
	}
}

func TestAdminCheckinsReturnsRecords(t *testing.T) {
	records := checkin.Load(filepath.Join(t.TempDir(), "checkin.json"), checkin.MaxRecords)
	records.Add(checkin.Record{
		Time: time.Now(), UID: "u1", Status: "success",
		CreditsBefore: 5000, CreditsAfter: 5100, CreditsGained: 100,
	})
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), Records: records})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/checkins?page=1&size=20", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询签到记录失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp checkinsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(resp.Records) != 1 || resp.Records[0].CreditsGained != 100 {
		t.Fatalf("签到记录不正确: %+v", resp.Records)
	}
	if resp.Page != 1 || resp.Size != 20 || resp.Total != 1 {
		t.Fatalf("分页元数据不正确: %+v", resp)
	}
}

func TestAdminCheckinsPaginates(t *testing.T) {
	records := checkin.Load(filepath.Join(t.TempDir(), "checkin.json"), checkin.MaxRecords)
	for i := 1; i <= 12; i++ {
		records.Add(checkin.Record{Time: time.Unix(int64(i), 0), UID: "u", Status: "success"})
	}
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), Records: records, APIKey: "sk"})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/checkins?page=2&size=5", nil)
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp checkinsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Page != 2 || resp.Size != 5 || resp.Total != 12 || len(resp.Records) != 5 {
		t.Fatalf("分页不正确: %+v", resp)
	}
	// 最新在前：page 2 应比 page 1 旧。
	if resp.Records[0].UID != "u" {
		t.Fatalf("page 2 内容不正确: %+v", resp.Records)
	}
}

func TestAdminCheckinsClampsSize(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), APIKey: "sk"})
	req := httptest.NewRequest(http.MethodGet, "/admin/api/checkins?size=9999", nil)
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var resp checkinsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Size != 200 {
		t.Fatalf("size 应夹紧到 200，实际 %d", resp.Size)
	}
}

func TestAdminStatsRequiresAPIKey(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), APIKey: "sk-test"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权应返回 401，实际 HTTP %d", rec.Code)
	}
}

func TestAdminModelsListsAll(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), APIKey: "sk"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/models", nil)
	req.Header.Set("Authorization", "Bearer sk")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []upstream.ModelMeta `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Items) == 0 {
		t.Fatal("应返回至少一个模型")
	}
}

func TestAdminCheckinsRequiresAPIKey(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), APIKey: "sk-test"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/checkins", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权应返回 401，实际 HTTP %d", rec.Code)
	}
}

func TestAdminScheduleGetAndSetCreditRefreshInterval(t *testing.T) {
	settings := schedule.Load(filepath.Join(t.TempDir(), "schedule.json"), []int{9, 21}, []int{22}, nil, 30*time.Minute)
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), APIKey: "sk-test", Schedule: settings})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/schedule", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询定时设置失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var got scheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.CreditRefreshInterval != "30m" {
		t.Fatalf("额度刷新间隔不正确: %+v", got)
	}

	interval := "2h"
	body, _ := json.Marshal(scheduleRequest{
		CheckinHours:          []int{8},
		KeepaliveHours:        []int{},
		CreditRefreshInterval: &interval,
	})
	req = httptest.NewRequest(http.MethodPost, "/admin/api/schedule", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存定时设置失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var saved scheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("解析保存响应失败: %v", err)
	}
	if len(saved.CheckinHours) != 1 || saved.CheckinHours[0] != 8 ||
		len(saved.KeepaliveHours) != 0 || saved.CreditRefreshInterval != "2h" {
		t.Fatalf("保存后的定时设置不正确: %+v", saved)
	}
}

func TestAdminScheduleSetPreservesMissingCreditRefreshInterval(t *testing.T) {
	settings := schedule.Load("", []int{9, 21}, []int{22}, nil, time.Hour)
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), Schedule: settings})
	body, _ := json.Marshal(scheduleRequest{CheckinHours: []int{7}, KeepaliveHours: []int{23}})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/schedule", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存定时设置失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var saved scheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if saved.CreditRefreshInterval != "1h" {
		t.Fatalf("未传 interval 时应保留当前值，实际 %+v", saved)
	}
}

func TestAdminScheduleRejectsInvalidCreditRefreshInterval(t *testing.T) {
	interval := "30s"
	body, _ := json.Marshal(scheduleRequest{CreditRefreshInterval: &interval})
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), Schedule: schedule.Load("", nil, nil, nil, 0)})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/schedule", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法间隔应返回 400，实际 HTTP %d %s", rec.Code, rec.Body.String())
	}
}

// seedPool 直接构造带若干账号的池（绕过 OAuth 登录流程），返回 *pool.Pool。
func seedPool(t *testing.T) *pool.Pool {
	t.Helper()
	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid, Nickname: "user-" + uid})
	}
	return p
}

func TestAdminAccountsManageEnableDisableCooldown(t *testing.T) {
	const apiKey = "sk"
	p := seedPool(t)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: apiKey})

	// 禁用 u1、冷却 u2。
	p.Disable("u1", "test")
	p.ForceCooldown("u2", "test-cool", time.Minute)

	// 启用 u1、u2，禁用 u3（其余不受影响）。
	body, _ := json.Marshal(map[string]any{
		"uids":   []string{"u1", "u2"},
		"action": "enable",
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/manage", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("启用失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Applied  int           `json:"applied"`
		Skipped  int           `json:"skipped"`
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Applied != 2 {
		t.Fatalf("applied 应为 2，实际 %d", resp.Applied)
	}
	for _, st := range resp.Accounts {
		switch st.UID {
		case "u1":
			if st.Disabled {
				t.Fatal("u1 启用后不应再禁用")
			}
		case "u2":
			if st.Cooling {
				t.Fatal("u2 启用后应解除冷却")
			}
		case "u3":
			if st.Disabled {
				t.Fatal("u3 未在操作列表中，不应被禁用")
			}
		}
	}

	// 禁用 u3。
	body, _ = json.Marshal(map[string]any{"uids": []string{"u3"}, "action": "disable"})
	req = httptest.NewRequest(http.MethodPost, "/admin/api/accounts/manage", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("禁用失败：HTTP %d", rec.Code)
	}
	var disabled struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &disabled); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var u3 pool.Status
	for _, st := range disabled.Accounts {
		if st.UID == "u3" {
			u3 = st
		}
	}
	if !u3.Disabled {
		t.Fatal("u3 禁用后应处于禁用状态")
	}

	// 冷却 u3 60s。
	body, _ = json.Marshal(map[string]any{"uids": []string{"u3"}, "action": "cooldown", "cooldown_seconds": 60})
	req = httptest.NewRequest(http.MethodPost, "/admin/api/accounts/manage", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("冷却失败：HTTP %d", rec.Code)
	}
	var cooled struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cooled); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var u3c pool.Status
	for _, st := range cooled.Accounts {
		if st.UID == "u3" {
			u3c = st
		}
	}
	if !u3c.Disabled || !u3c.Cooling {
		t.Fatalf("u3 应同时处于禁用 + 冷却，实际 disabled=%v cooling=%v", u3c.Disabled, u3c.Cooling)
	}
	if u3c.Until.IsZero() {
		t.Fatal("u3 应有冷却到期时间")
	}
}

func TestAdminAccountsManageRejectsUnknownAction(t *testing.T) {
	p := seedPool(t)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "sk"})
	body, _ := json.Marshal(map[string]any{"uids": []string{"u1"}, "action": "explode"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/manage", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知 action 应返回 400，实际 HTTP %d", rec.Code)
	}
}

func TestAdminAccountsManageAppliesToAllWhenUIDsEmpty(t *testing.T) {
	p := seedPool(t)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "sk"})
	body, _ := json.Marshal(map[string]any{"action": "disable"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/manage", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Applied int `json:"applied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Applied != 3 {
		t.Fatalf("空 uids 应作用于全部 3 个账号，实际 %d", resp.Applied)
	}
}

// fakeBalanceUpstream 起一个实现 GET /api/v1/auth/me 的假上游，返回给定 USD 余额。
func fakeBalanceUpstream(t *testing.T, balance float64) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/me" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"balance":%g}}`, balance)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LB2A_UPSTREAM_BASE", srv.URL)
}

func TestBalanceReturnsUpstreamUSD(t *testing.T) {
	fakeBalanceUpstream(t, 12.345678)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-1"})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "sk"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("查询余额失败：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp balanceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.TotalGranted != 12.345678 || resp.TotalUsed != 0 || resp.TotalAvailable != 12.345678 {
		t.Fatalf("额度不正确: %+v", resp)
	}
	if resp.Object != "credit_summary" {
		t.Fatalf("object 不正确: %q", resp.Object)
	}
}

func TestBalanceRequiresAPIKey(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), APIKey: "sk"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权应返回 401，实际 HTTP %d", rec.Code)
	}
}

func TestBalanceFallsBackToCachedCredits(t *testing.T) {
	t.Setenv("LB2A_UPSTREAM_BASE", "http://127.0.0.1:1") // 不可达，触发回退
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "tok-1"})
	p.SetCredits("u1", 5000)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "sk"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("上游不可达应回退缓存：HTTP %d %s", rec.Code, rec.Body.String())
	}
	var resp balanceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.TotalAvailable != 5000 || resp.TotalGranted != 5000 || resp.TotalUsed != 0 {
		t.Fatalf("回退额度不正确: %+v", resp)
	}
}

func TestBalanceNoHealthyAccount(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), APIKey: "sk"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer sk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无可用账号应返回 503，实际 HTTP %d", rec.Code)
	}
}
