package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"lobsterai2api/internal/auth"
)

// newFakeUpstream 起一个实现 update/slot/context/check_in 的假上游，
// 返回客户端（HTTP 与 UpdateURL 都指向假服务）和请求记录。
func newFakeUpstream(t *testing.T, ctxClaimed bool, slotState string, postCode int) (*Client, *[]string, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/update":
			_, _ = w.Write([]byte(`{"data":{"value":{"version":"2.3.4"}}}`))
		case r.URL.Path == "/api/client-activities/slot":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"slotState":"` + slotState +
				`","activity":{"activityCode":"act-1","configRevision":3}}}`))
		case r.URL.Path == "/api/client-activities/act-1/context":
			claimed := "false"
			if ctxClaimed {
				claimed = "true"
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"state":{"claimedToday":` + claimed +
				`},"actions":["check_in","share"]}}`))
		case r.URL.Path == "/api/client-activities/act-1/actions/check_in":
			if postCode != 0 {
				_, _ = w.Write([]byte(`{"code":` + jsonNum(postCode) + `,"msg":"bad"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"result":{"creditsGranted":100}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LB2A_UPSTREAM_BASE", srv.URL)
	c := &Client{HTTP: srv.Client(), UpdateURL: srv.URL + "/update"}
	return c, &calls, &mu
}

func jsonNum(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestDailyCheckinSuccess(t *testing.T) {
	c, calls, mu := newFakeUpstream(t, false, "available", 0)
	a := &auth.Auth{AccessToken: "tok-1", UID: "u1"}
	res, err := c.DailyCheckin(a)
	if err != nil {
		t.Fatalf("签到失败: %v", err)
	}
	if res.Status != CheckinSuccess || res.Gained != 100 || res.ActivityCode != "act-1" {
		t.Fatalf("签到结果不正确: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	// 三步调用必须按序执行，且携带解析出的客户端版本。
	if len(*calls) < 4 { // update + slot + context + check_in
		t.Fatalf("调用次数不足: %v", *calls)
	}
	if !strings.Contains((*calls)[1], "clientVersion=2.3.4") {
		t.Fatalf("slot 未携带解析出的版本: %v", *calls)
	}
	if (*calls)[3] != "POST /api/client-activities/act-1/actions/check_in?" {
		t.Fatalf("check_in 调用不正确: %v", *calls)
	}
}

func TestDailyCheckinAlreadyClaimed(t *testing.T) {
	c, calls, mu := newFakeUpstream(t, true, "available", 0)
	res, err := c.DailyCheckin(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	if err != nil {
		t.Fatalf("已签到不应报错: %v", err)
	}
	if res.Status != CheckinAlreadyClaimed {
		t.Fatalf("状态应为 already_claimed，实际 %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, call := range *calls {
		if strings.HasPrefix(call, "POST ") {
			t.Fatalf("已签到时不应发起 check_in: %v", *calls)
		}
	}
}

func TestDailyCheckinNoActivity(t *testing.T) {
	c, _, _ := newFakeUpstream(t, false, "unavailable", 0)
	res, err := c.DailyCheckin(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	if err != nil {
		t.Fatalf("无活动不应报错: %v", err)
	}
	if res.Status != CheckinNoActivity {
		t.Fatalf("状态应为 no_activity，实际 %+v", res)
	}
}

func TestDailyCheckinBusinessError(t *testing.T) {
	c, _, _ := newFakeUpstream(t, false, "available", 5001)
	if _, err := c.DailyCheckin(&auth.Auth{AccessToken: "tok-1", UID: "u1"}); err == nil {
		t.Fatal("业务失败应返回错误")
	}
}

func TestClientVersionFallback(t *testing.T) {
	c, _, _ := newFakeUpstream(t, false, "available", 0)
	// 更新接口不可用（404）时回退内置版本，签到仍可执行。
	c.UpdateURL += "/gone"
	res, err := c.DailyCheckin(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	if err != nil {
		t.Fatalf("版本解析失败不应阻塞签到: %v", err)
	}
	if res.Status != CheckinSuccess {
		t.Fatalf("状态应为 success，实际 %+v", res)
	}
}

func TestClientVersionCached(t *testing.T) {
	var mu sync.Mutex
	updateCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/update" {
			mu.Lock()
			updateCalls++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"value":{"version":"9.9.9"}}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LB2A_UPSTREAM_BASE", srv.URL)
	c := &Client{HTTP: srv.Client(), UpdateURL: srv.URL + "/update"}
	if v := c.clientVersion(); v != "9.9.9" {
		t.Fatalf("版本解析结果不正确: %q", v)
	}
	if v := c.clientVersion(); v != "9.9.9" {
		t.Fatalf("缓存版本不正确: %q", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if updateCalls != 1 {
		t.Fatalf("版本应缓存只请求一次，实际 %d 次", updateCalls)
	}
}

func TestQuotaUsageAllowsZeroCredits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/profile-summary" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"totalCreditsRemaining":0}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LB2A_UPSTREAM_BASE", srv.URL)
	c := &Client{HTTP: srv.Client()}

	remain, _, err := c.QuotaUsage(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	if err != nil {
		t.Fatalf("0 额度不应报错: %v", err)
	}
	if remain != 0 {
		t.Fatalf("0 额度解析不正确: %d", remain)
	}
}
