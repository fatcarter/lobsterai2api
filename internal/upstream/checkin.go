// checkin.go 每日签到：活动位 → 活动上下文 → check_in 三步，与客户端行为一致。
package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lobsterai2api/internal/auth"
)

const (
	// CheckinSuccess 签到成功，获得积分。
	CheckinSuccess = "success"
	// CheckinAlreadyClaimed 当天已签到，无需重复执行。
	CheckinAlreadyClaimed = "already_claimed"
	// CheckinNoActivity 当前无可用签到活动。
	CheckinNoActivity = "no_activity"

	// defaultUpdateURL 官方更新接口，用于解析客户端版本号；可用 LB2A_UPDATE_URL 覆盖。
	defaultUpdateURL = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"
	// clientVersionTTL 解析出的客户端版本缓存时长。
	clientVersionTTL = 24 * time.Hour
	// clientVersionTimeout 更新接口请求超时。
	clientVersionTimeout = 15 * time.Second
)

// clientVersionRe 校验版本号形如 1.2.3 或 1.2.3-beta.1（与 Python 签到脚本一致）。
var clientVersionRe = regexp.MustCompile(`^(\d+(?:\.\d+)*)(?:-[0-9A-Za-z.-]+)?$`)

// CheckinResult 一次签到动作的结果；只有 Status == CheckinSuccess 时 Gained 有效。
type CheckinResult struct {
	Status       string `json:"status"`        // success / already_claimed / no_activity
	ActivityCode string `json:"activity_code"` // 命中的签到活动
	Gained       int64  `json:"gained"`        // 签到接口返回的积分增量
}

// clientVersion 解析当前客户端版本号（带缓存）；解析失败回退内置版本，
// 保证签到主流程不被更新接口的可用性阻塞。
func (c *Client) clientVersion() string {
	c.verMu.Lock()
	defer c.verMu.Unlock()
	if c.verResolved != "" && time.Since(c.verAt) < clientVersionTTL {
		return c.verResolved
	}
	updateURL := c.UpdateURL
	if updateURL == "" {
		updateURL = defaultUpdateURL
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientVersionTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateURL, nil)
	if err == nil {
		resp, doErr := client.Do(req)
		if doErr == nil {
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if resp.StatusCode == http.StatusOK {
				var env struct {
					Data struct {
						Value struct {
							Version string `json:"version"`
						} `json:"value"`
					} `json:"data"`
				}
				v := ""
				if json.Unmarshal(raw, &env) == nil {
					v = strings.TrimSpace(env.Data.Value.Version)
				}
				if clientVersionRe.MatchString(v) {
					c.verResolved = v
					c.verAt = time.Now()
					return c.verResolved
				}
				log.Printf("checkin: update 接口版本格式异常 %q，回退 %s", truncate(v, 40), clientVersion)
				return clientVersion
			}
		}
	}
	log.Printf("checkin: 解析客户端版本失败，回退 %s", clientVersion)
	return clientVersion
}

// DailyCheckin 执行每日签到：查活动位 → 查活动上下文 → 执行 check_in。
// 三种正常结果（成功/已签到/无活动）通过返回值区分，只有网络或业务失败才返回 error。
func (c *Client) DailyCheckin(a *auth.Auth) (CheckinResult, error) {
	ver := c.clientVersion()
	ua := "LobsterAI/" + ver

	// 1) 活动位：拿到当日活动的 code 与 configRevision。
	query := fmt.Sprintf("placement=desktop_sidebar&clientVersion=%s&containerApiVersion=2&platform=win32",
		url.QueryEscape(ver))
	req, err := http.NewRequest(http.MethodGet, ServerBase()+"/api/client-activities/slot?"+query, nil)
	if err != nil {
		return CheckinResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	data, err := c.doJSON(req)
	if err != nil {
		return CheckinResult{}, err
	}
	var slot struct {
		SlotState string `json:"slotState"`
		Activity  *struct {
			ActivityCode   string `json:"activityCode"`
			ConfigRevision any    `json:"configRevision"` // 上游可能返回数字或字符串，原样透传
		} `json:"activity"`
	}
	if err := json.Unmarshal(data, &slot); err != nil {
		return CheckinResult{}, fmt.Errorf("slot parse: %w", err)
	}
	if slot.SlotState != "available" || slot.Activity == nil || slot.Activity.ActivityCode == "" {
		return CheckinResult{Status: CheckinNoActivity}, nil
	}
	code := slot.Activity.ActivityCode
	rev, ok := revisionValue(slot.Activity.ConfigRevision)
	if !ok {
		return CheckinResult{}, fmt.Errorf("slot configRevision 类型异常: %v", slot.Activity.ConfigRevision)
	}
	res := CheckinResult{ActivityCode: code}

	// 2) 活动上下文：当天已签到或无 check_in 动作则跳过。
	req, err = http.NewRequest(http.MethodGet,
		ServerBase()+"/api/client-activities/"+url.PathEscape(code)+"/context?configRevision="+url.QueryEscape(rev), nil)
	if err != nil {
		return CheckinResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	data, err = c.doJSON(req)
	if err != nil {
		return CheckinResult{}, err
	}
	var actCtx struct {
		State struct {
			ClaimedToday bool `json:"claimedToday"`
		} `json:"state"`
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(data, &actCtx); err != nil {
		return CheckinResult{}, fmt.Errorf("context parse: %w", err)
	}
	if actCtx.State.ClaimedToday || !containsStr(actCtx.Actions, "check_in") {
		res.Status = CheckinAlreadyClaimed
		return res, nil
	}

	// 3) 执行签到；幂等键防止同一次请求被重放。
	body, _ := json.Marshal(map[string]any{
		"configRevision": slot.Activity.ConfigRevision,
		"idempotencyKey": newUUID(),
		"payload":        map[string]any{},
	})
	req, err = http.NewRequest(http.MethodPost,
		ServerBase()+"/api/client-activities/"+url.PathEscape(code)+"/actions/check_in",
		bytes.NewReader(body))
	if err != nil {
		return CheckinResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	data, err = c.doJSON(req)
	if err != nil {
		return CheckinResult{}, err
	}
	res.Status = CheckinSuccess
	res.Gained = gainedCredits(data)
	return res, nil
}

// gainedCredits 从 check_in 响应中取积分增量，按键顺序匹配 Python 脚本：
// creditsGranted → rewardCredits → credits，取第一个出现的数值。
func gainedCredits(data json.RawMessage) int64 {
	var env struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &env) != nil {
		return 0
	}
	for _, key := range []string{"creditsGranted", "rewardCredits", "credits"} {
		raw, ok := env.Result[key]
		if !ok {
			continue
		}
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			return int64(v)
		}
	}
	return 0
}

// revisionValue 把 configRevision（数字或字符串）转成查询串安全的形式。
func revisionValue(v any) (string, bool) {
	switch t := v.(type) {
	case float64:
		return strconv.FormatInt(int64(t), 10), true
	case string:
		if t == "" {
			return "", false
		}
		return t, true
	}
	return "", false
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// newUUID 生成 v4 UUID 字符串（签到幂等键），与 oauth.go 的生成方式一致。
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
