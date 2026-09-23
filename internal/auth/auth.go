// Package auth 解析 LobsterAI auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Auth 是归一化后的账号凭证（来源可以是登录工具 OAuth 嵌套形或手建扁平形）。
type Auth struct {
	AccessToken   string
	RefreshToken  string
	ExpiresAt     int64  // Unix 秒
	UID           string // 用户唯一 ID
	UserId        string // 有道 yid
	Nickname      string
	Uuid          string // 安装 UUID（exchange/refresh 用）
	FirstKeyfrom  string // 首次登录时间戳
	LatestKeyfrom string // 最近活动时间戳
	FilePath      string // 来源文件；refresh 后原子写回此处
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// KeyfromBody 返回 exchange/refresh 请求体的 keyfrom 载荷。
func (a *Auth) KeyfromBody() map[string]any {
	body := map[string]any{
		"firstKeyfrom":  a.FirstKeyfrom,
		"latestKeyfrom": a.LatestKeyfrom,
		"version":       "0.1.0",
	}
	if a.Uuid != "" {
		body["uuid"] = a.Uuid
	}
	if a.UserId != "" {
		body["userId"] = a.UserId
	}
	return body
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （登录工具 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手建）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken   string `json:"accessToken"`
				RefreshToken  string `json:"refreshToken"`
				ExpiresAt     int64  `json:"expiresAt"`
				Uuid          string `json:"uuid"`
				FirstKeyfrom  string `json:"firstKeyfrom"`
				LatestKeyfrom string `json:"latestKeyfrom"`
			} `json:"auth"`
			Account struct {
				UID      string `json:"uid"`
				UserId   string `json:"userId"`
				Nickname string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:   n.Auth.AccessToken,
			RefreshToken:  n.Auth.RefreshToken,
			ExpiresAt:     n.Auth.ExpiresAt,
			Uuid:          n.Auth.Uuid,
			FirstKeyfrom:  n.Auth.FirstKeyfrom,
			LatestKeyfrom: n.Auth.LatestKeyfrom,
			UID:           n.Account.UID,
			UserId:        n.Account.UserId,
			Nickname:      n.Account.Nickname,
		}
	} else {
		var f struct {
			AccessToken   string `json:"accessToken"`
			RefreshToken  string `json:"refreshToken"`
			ExpiresAt     int64  `json:"expiresAt"`
			Uuid          string `json:"uuid"`
			FirstKeyfrom  string `json:"firstKeyfrom"`
			LatestKeyfrom string `json:"latestKeyfrom"`
			UID           string `json:"uid"`
			UserId        string `json:"userId"`
			Nickname      string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:   f.AccessToken,
			RefreshToken:  f.RefreshToken,
			ExpiresAt:     f.ExpiresAt,
			Uuid:          f.Uuid,
			FirstKeyfrom:  f.FirstKeyfrom,
			LatestKeyfrom: f.LatestKeyfrom,
			UID:           f.UID,
			UserId:        f.UserId,
			Nickname:      f.Nickname,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持登录工具可读格式。
func (a *Auth) SaveAtomic() (resultErr error) {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":   a.AccessToken,
			"refreshToken":  a.RefreshToken,
			"expiresAt":     a.ExpiresAt,
			"uuid":          a.Uuid,
			"firstKeyfrom":  a.FirstKeyfrom,
			"latestKeyfrom": a.LatestKeyfrom,
		},
		"account": map[string]any{
			"uid":      a.UID,
			"userId":   a.UserId,
			"nickname": a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// 网页登录与后台刷新可能同时落盘；独立临时文件避免相互截断，失败时清理凭证残片。
	tmp, err := os.CreateTemp(filepath.Dir(a.FilePath), ".lobsterai-auth-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(tmp.Name()); err != nil && !os.IsNotExist(err) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	_, writeErr := tmp.Write(raw)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), a.FilePath)
}

// LoadDir 扫描 dir 下 lobsterai-*.json，解析失败的文件静默跳过。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "lobsterai-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
