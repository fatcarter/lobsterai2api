package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrOAuthConfig 表示登录门户、上游或回调地址缺失/无效，普通 API 服务仍可继续运行。
var ErrOAuthConfig = errors.New("请正确配置 LB2A_UPSTREAM_BASE 和 LB2A_LOGIN_PORTAL 后重试")

// OAuthClient 沿用命令行登录的门户授权和 /api/auth/exchange 协议。
// RedirectURI 由服务监听端口生成，保持上游要求的本机回调格式；管理页只接受粘贴返回地址。
type OAuthClient struct {
	BaseURL     string       // 上游 API 根地址，不包含查询参数或片段。
	PortalURL   string       // 登录门户根地址，授权页位于 /portal#/login。
	RedirectURI string       // 固定的本机回调地址，不由浏览器提交或修改。
	HTTP        *http.Client // 可复用连接池；授权交换单独限制为 30 秒且禁止跳转。
}

// OAuthLogin 保存一次授权所需的关联信息，State 和 UUID 不应写入日志。
type OAuthLogin struct {
	ID               string // 非敏感的会话标识，用于接口寻址与日志关联。
	State            string // 一次性随机校验值，防止其他会话的返回地址被误用。
	UUID             string // 本次登录的安装标识，后续刷新令牌沿用此值。
	FirstKeyfrom     string // 创建登录时的毫秒时间戳，沿用现有客户端协议。
	RedirectURI      string // 生成授权链接时使用的回调地址。
	AuthorizationURL string // 浏览器授权地址，仅在创建会话时返回管理页。
}

// oauthExchangeRequest 是上游授权码交换的固定请求结构。
type oauthExchangeRequest struct {
	AuthCode      string `json:"authCode"`      // 门户返回的一次性授权码。
	FirstKeyfrom  string `json:"firstKeyfrom"`  // 授权会话创建时间。
	LatestKeyfrom string `json:"latestKeyfrom"` // 本次交换请求时间。
	UUID          string `json:"uuid"`          // 授权会话的安装标识。
	Version       string `json:"version"`       // 与现有命令行客户端保持一致的协议版本。
}

// oauthExchangeResponse 只解析落盘凭证所需的上游字段，不转发上游消息或原始响应。
type oauthExchangeResponse struct {
	Code int `json:"code"` // 零表示成功，非零业务码仅用于脱敏错误定位。
	Data struct {
		AccessToken  string `json:"accessToken"`  // 调用上游的访问令牌。
		RefreshToken string `json:"refreshToken"` // 后续保活使用的刷新令牌。
		ExpiresIn    int64  `json:"expiresIn"`    // 访问令牌剩余有效秒数。
		User         struct {
			ID       string `json:"id"`       // 首选的账号标识。
			Yid      string `json:"yid"`      // 兼容旧响应的账号标识。
			UserID   string `json:"userId"`   // 上游请求所需的用户标识。
			Nickname string `json:"nickname"` // 管理页展示的昵称。
		} `json:"user"` // 登录账号信息。
	} `json:"data"` // 授权交换结果。
}

// Ready 报告上游、门户和回调地址是否都已正确配置，可用于启动日志与接口前置检查。
func (c *OAuthClient) Ready() bool {
	if c == nil {
		return false
	}
	for _, value := range []string{c.BaseURL, c.PortalURL, c.RedirectURI} {
		if !validOAuthURL(value) {
			return false
		}
	}
	return true
}

// NewLogin 生成独立的 state、安装 UUID 和授权链接，不启动本地回调监听器。
func (c *OAuthClient) NewLogin(now time.Time) (*OAuthLogin, error) {
	if !c.Ready() {
		return nil, ErrOAuthConfig
	}
	random := make([]byte, 64)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("生成授权会话随机值失败: %w", err)
	}
	uuid := random[16:32]
	uuid[6] = uuid[6]&0x0f | 0x40
	uuid[8] = uuid[8]&0x3f | 0x80
	login := &OAuthLogin{
		ID:           hex.EncodeToString(random[:16]),
		State:        hex.EncodeToString(random[32:]),
		UUID:         fmt.Sprintf("%x-%x-%x-%x-%x", uuid[:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:]),
		FirstKeyfrom: strconv.FormatInt(now.UnixMilli(), 10),
		RedirectURI:  c.RedirectURI,
	}
	query := url.Values{
		"source":       {"electron"},
		"redirect_uri": {login.RedirectURI},
		"state":        {login.State},
	}
	login.AuthorizationURL = strings.TrimRight(c.PortalURL, "/") + "/portal#/login?" + query.Encode()
	return login, nil
}

// Exchange 用本次会话的授权码换取账号凭证；错误只包含状态码，不包含令牌、授权码或上游正文。
func (c *OAuthClient) Exchange(ctx context.Context, login OAuthLogin, code string) (*Auth, error) {
	if c == nil || !validOAuthURL(c.BaseURL) {
		return nil, ErrOAuthConfig
	}
	now := time.Now()
	body := oauthExchangeRequest{
		AuthCode: code, FirstKeyfrom: login.FirstKeyfrom,
		LatestKeyfrom: strconv.FormatInt(now.UnixMilli(), 10), UUID: login.UUID, Version: "0.1.0",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("构造授权交换请求失败")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/api/auth/exchange", bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("构造授权交换请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "LobsterAI/0.1.0")
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	// 授权码仅发送给配置的上游，禁止 307/308 等跳转将请求体带往其他地址。
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("无法完成上游授权交换，请检查上游连接并重新登录")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游授权交换失败（HTTP %d），请重新登录", resp.StatusCode)
	}
	raw, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("读取上游授权结果失败")
	}
	var result oauthExchangeResponse
	if json.Unmarshal(raw, &result) != nil {
		return nil, errors.New("上游授权结果格式无效")
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("上游拒绝授权（业务码 %d），请重新登录", result.Code)
	}
	ex := result.Data
	if strings.TrimSpace(ex.AccessToken) == "" || ex.ExpiresIn < 0 || ex.ExpiresIn > int64((1<<63-1)/time.Second) {
		return nil, errors.New("上游未返回有效的账号凭证")
	}
	uid := ex.User.ID
	if uid == "" {
		uid = ex.User.UserID
	}
	if uid == "" {
		uid = ex.User.Yid
	}
	if uid == "" {
		uid = fmt.Sprintf("%x", sha256.Sum256([]byte(ex.AccessToken)))[:16]
	}
	expiresAt := int64(0)
	if ex.ExpiresIn > 0 {
		expiresAt = now.Add(time.Duration(ex.ExpiresIn) * time.Second).Unix()
	} else if parts := strings.Split(ex.AccessToken, "."); len(parts) == 3 {
		if claims, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var payload struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(claims, &payload) == nil {
				expiresAt = payload.Exp
			}
		}
	}
	return &Auth{
		AccessToken: ex.AccessToken, RefreshToken: ex.RefreshToken, ExpiresAt: expiresAt,
		UID: uid, UserId: ex.User.UserID, Nickname: ex.User.Nickname,
		Uuid: login.UUID, FirstKeyfrom: login.FirstKeyfrom, LatestKeyfrom: body.LatestKeyfrom,
	}, nil
}

// validOAuthURL 限制配置为 HTTP(S) 根地址，避免凭据、查询串和片段进入授权请求地址。
func validOAuthURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && !strings.ContainsAny(value, "\r\n\t ")
}
