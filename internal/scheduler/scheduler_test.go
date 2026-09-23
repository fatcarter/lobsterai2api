package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/checkin"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/schedule"
	"lobsterai2api/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 16, 10, 30, 0, 0, loc)
	cases := []struct {
		name  string
		now   time.Time
		hours []int
		want  time.Time
	}{
		{name: "当天下一整点", now: now, hours: []int{9, 21}, want: time.Date(2026, 9, 16, 21, 0, 0, 0, loc)},
		{name: "全部已过取次日最早", now: time.Date(2026, 9, 16, 21, 30, 0, 0, loc), hours: []int{9, 21}, want: time.Date(2026, 9, 17, 9, 0, 0, 0, loc)},
		{name: "恰好整点顺延次日", now: time.Date(2026, 9, 16, 21, 0, 0, 0, loc), hours: []int{21}, want: time.Date(2026, 9, 17, 21, 0, 0, 0, loc)},
		{name: "无小时无触发", now: now, hours: nil, want: time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextFire(tc.now, tc.hours); !got.Equal(tc.want) {
				t.Fatalf("得到 %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestRunIdleWithoutHours(t *testing.T) {
	s := New(Config{Schedule: schedule.Load("", nil, nil, nil, 0)})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// 未配置任何小时时 Run 挂起等待设置变更或退出，而不是空转；取消后应立即返回。
	s.Run(ctx)
}

func TestRunNilScheduleReturns(t *testing.T) {
	s := New(Config{})
	// 未提供定时设置时 Run 直接返回。
	s.Run(context.Background())
}

// newFakeUpstream 起一个假上游：profile-summary 依次返回 5000/5100，签到成功 +100。
func newFakeUpstream(t *testing.T, quotaCalls *int, postCode int) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/update":
			_, _ = w.Write([]byte(`{"data":{"value":{"version":"2.3.4"}}}`))
		case "/api/user/profile-summary":
			*quotaCalls++
			if *quotaCalls == 1 {
				_, _ = w.Write([]byte(`{"code":0,"data":{"totalCreditsRemaining":5000}}`))
			} else {
				_, _ = w.Write([]byte(`{"code":0,"data":{"totalCreditsRemaining":5100}}`))
			}
		case "/api/client-activities/slot":
			_, _ = w.Write([]byte(`{"code":0,"data":{"slotState":"available","activity":{"activityCode":"act-1","configRevision":3}}}`))
		case "/api/client-activities/act-1/context":
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`))
		case "/api/client-activities/act-1/actions/check_in":
			if postCode != 0 {
				_, _ = w.Write([]byte(`{"code":5001,"msg":"bad"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"result":{"creditsGranted":100}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LB2A_UPSTREAM_BASE", srv.URL)
	return &upstream.Client{HTTP: srv.Client(), UpdateURL: srv.URL + "/update"}
}

func TestRunCheckinNowRecordsCreditsDelta(t *testing.T) {
	var quotaCalls int
	up := newFakeUpstream(t, &quotaCalls, 0)

	p := pool.New("")
	p.Add(&auth.Auth{AccessToken: "tok-1", UID: "u1", Nickname: "测试账号"})
	records := checkin.Load(filepath.Join(t.TempDir(), "checkin.json"), checkin.MaxRecords)
	s := New(Config{Pool: p, Upstream: up, Records: records, Schedule: schedule.Load("", []int{9, 21}, []int{22}, nil, 0)})

	s.RunCheckinNow()

	got := records.List(0)
	if len(got) != 1 {
		t.Fatalf("应落 1 条签到记录，实际 %d", len(got))
	}
	rec := got[0]
	if rec.UID != "u1" || rec.Status != upstream.CheckinSuccess {
		t.Fatalf("记录不正确: %+v", rec)
	}
	if rec.CreditsBefore != 5000 || rec.CreditsAfter != 5100 || rec.CreditsGained != 100 {
		t.Fatalf("签到前后积分变化不正确: before=%d after=%d gained=%d",
			rec.CreditsBefore, rec.CreditsAfter, rec.CreditsGained)
	}
	// 余额刷新进池，供挑选策略使用。
	if st := p.List()[0]; st.Credits != 5100 {
		t.Fatalf("池中余额未刷新，实际 %d", st.Credits)
	}
}

func TestRunCheckinNowSkipsDisabled(t *testing.T) {
	var quotaCalls int
	up := newFakeUpstream(t, &quotaCalls, 0)
	p := pool.New("")
	p.Add(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	p.Disable("u1", "session dead")
	records := checkin.Load(filepath.Join(t.TempDir(), "checkin.json"), checkin.MaxRecords)
	s := New(Config{Pool: p, Upstream: up, Records: records, Schedule: schedule.Load("", []int{9, 21}, []int{22}, nil, 0)})
	s.RunCheckinNow()
	if got := records.List(0); len(got) != 0 {
		t.Fatalf("禁用账号不应签到，实际 %d 条记录", len(got))
	}
}

func TestRunCheckinNowRecordsError(t *testing.T) {
	var quotaCalls int
	up := newFakeUpstream(t, &quotaCalls, 5001)
	p := pool.New("")
	p.Add(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	records := checkin.Load(filepath.Join(t.TempDir(), "checkin.json"), checkin.MaxRecords)
	s := New(Config{Pool: p, Upstream: up, Records: records, Schedule: schedule.Load("", []int{9, 21}, []int{22}, nil, 0)})
	s.RunCheckinNow()
	got := records.List(0)
	if len(got) != 1 {
		t.Fatalf("签到失败也应落记录，实际 %d 条", len(got))
	}
	if got[0].Status != checkin.StatusError || got[0].Message == "" {
		t.Fatalf("失败记录不正确: %+v", got[0])
	}
}

func TestRunCreditRefreshNowUpdatesCredits(t *testing.T) {
	var quotaCalls int
	up := newFakeUpstream(t, &quotaCalls, 0)
	p := pool.New("")
	p.Add(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	s := New(Config{Pool: p, Upstream: up, Schedule: schedule.Load("", nil, nil, nil, 30*time.Minute)})

	s.RunCreditRefreshNow()

	if st := p.List()[0]; st.Credits != 5000 {
		t.Fatalf("额度未刷新，实际 %d", st.Credits)
	}
}

func TestRunCreditRefreshNowReenablesCoolingAccount(t *testing.T) {
	var quotaCalls int
	up := newFakeUpstream(t, &quotaCalls, 0)
	p := pool.New("")
	p.Add(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	s := New(Config{Pool: p, Upstream: up, Schedule: schedule.Load("", nil, nil, nil, 30*time.Minute)})

	s.RunCreditRefreshNow()

	st := p.List()[0]
	if st.Cooling || st.Reason != "" || st.Credits != 5000 {
		t.Fatalf("余额恢复后应解除冷却并更新额度: %+v", st)
	}
}

func TestRunCreditRefreshNowSkipsDisabled(t *testing.T) {
	var quotaCalls int
	up := newFakeUpstream(t, &quotaCalls, 0)
	p := pool.New("")
	p.Add(&auth.Auth{AccessToken: "tok-1", UID: "u1"})
	p.Disable("u1", "session dead")
	s := New(Config{Pool: p, Upstream: up, Schedule: schedule.Load("", nil, nil, nil, 30*time.Minute)})

	s.RunCreditRefreshNow()

	if quotaCalls != 0 {
		t.Fatalf("禁用账号不应刷新额度，实际调用 %d 次", quotaCalls)
	}
}

// TestRunCheckinNowConcurrentAdds 记录存储并发写不丢数据。
func TestRunCheckinNowConcurrentAdds(t *testing.T) {
	records := checkin.Load(filepath.Join(t.TempDir(), "checkin.json"), checkin.MaxRecords)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			records.Add(checkin.Record{UID: "u1", Status: "success", Time: time.Now()})
		}()
	}
	wg.Wait()
	if got := records.List(0); len(got) != 8 {
		t.Fatalf("并发写丢记录，实际 %d 条", len(got))
	}
}
