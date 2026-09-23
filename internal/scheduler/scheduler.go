// Package scheduler 定时任务：每日签到（默认 9/21 点）+ token keepalive（默认 22 点）+ 周期刷新额度。
// 签到前后各查一次余额并落签到记录，余额 > 0 的冷却账号自动解冻。
// 定时设置在 schedule.Settings 中，管理页修改后即时生效。
package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/checkin"
	"lobsterai2api/internal/models"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/schedule"
	"lobsterai2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	Records  *checkin.Store     // 签到记录存储；nil 时不落盘
	Schedule *schedule.Settings // 定时设置（签到/保活/额度刷新/模型刷新）；nil 时任务都关闭
	// Models 用于定时刷新模型列表；nil 时定时刷新被跳过。
	Models *models.Store
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
}

// New 构建。
func New(cfg Config) *Scheduler {
	return &Scheduler{cfg: cfg}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。每次设置变更立即重算触发时间；
// 任务都未配置时挂起等待设置变更或退出。
func (s *Scheduler) Run(ctx context.Context) {
	if s.cfg.Schedule == nil {
		return
	}
	checkinH, keepaliveH, modelH, creditEvery, changed := s.cfg.Schedule.Snapshot()
	creditNext := nextIntervalFire(time.Now(), creditEvery)
	for {
		all := append(append([]int{}, checkinH...), keepaliveH...)
		all = append(all, modelH...)
		now := time.Now()
		hourNext := time.Time{}
		if len(all) > 0 {
			hourNext = nextFire(now, all)
		}
		next := earlier(hourNext, creditNext)
		var timer *time.Timer
		var timerC <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(time.Until(next))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-timerC:
			now := time.Now()
			if !hourNext.IsZero() && !now.Before(hourNext) {
				h := hourNext.Hour()
				if contains(checkinH, h) {
					s.RunCheckinNow()
				}
				if contains(keepaliveH, h) {
					s.RunKeepaliveNow()
				}
				if contains(modelH, h) {
					s.RunModelRefreshNow()
				}
			}
			if !creditNext.IsZero() && !now.Before(creditNext) {
				s.RunCreditRefreshNow()
				creditNext = nextIntervalFire(time.Now(), creditEvery)
			}
		case <-changed:
			// 管理页修改了设置：停掉旧定时器，下一轮按新设置重算。
			if timer != nil {
				timer.Stop()
			}
			checkinH, keepaliveH, modelH, creditEvery, changed = s.cfg.Schedule.Snapshot()
			creditNext = nextIntervalFire(time.Now(), creditEvery)
		}
	}
}

func nextIntervalFire(now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return time.Time{}
	}
	return now.Add(interval)
}

func earlier(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case a.Before(b):
		return a
	default:
		return b
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻，并落一条签到记录。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		s.checkinOne(st.UID, a)
	}
}

// RunCreditRefreshNow 立即刷新所有非禁用账号的额度；余额大于 0 时解除冷却。
func (s *Scheduler) RunCreditRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		remain, _, err := s.cfg.Upstream.QuotaUsage(a)
		if err != nil {
			log.Printf("额度刷新 %s: %v", st.UID, err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		log.Printf("额度刷新 %s: credits=%d", st.UID, remain)
	}
}

// checkinOne 单账号签到：签到前查余额 → 签到 → 签到后查余额 → 落记录 → 解冻。
func (s *Scheduler) checkinOne(uid string, a *auth.Auth) {
	rec := checkin.Record{
		Time:          time.Now(),
		UID:           uid,
		Nickname:      a.Nickname,
		Status:        checkin.StatusError,
		CreditsBefore: -1,
		CreditsAfter:  -1,
	}

	if before, _, err := s.cfg.Upstream.QuotaUsage(a); err == nil {
		rec.CreditsBefore = before
	} else {
		rec.Message = "签到前查积分失败: " + err.Error()
		log.Printf("checkin %s: pre-quota: %v", uid, err)
	}

	res, err := s.cfg.Upstream.DailyCheckin(a)
	switch {
	case err != nil:
		// 已签到等业务错误也继续走余额查询
		rec.Message = joinMsg(rec.Message, err.Error())
		log.Printf("checkin %s: %v", uid, err)
	default:
		rec.Status = res.Status
		rec.CreditsGained = res.Gained
	}

	if after, _, err := s.cfg.Upstream.QuotaUsage(a); err == nil {
		rec.CreditsAfter = after
		s.cfg.Pool.ReenableIfCredits(uid, after)
	} else {
		rec.Message = joinMsg(rec.Message, "签到后查积分失败: "+err.Error())
		log.Printf("checkin %s: post-quota: %v", uid, err)
	}

	// 变化量以余额差值为准；两端查询失败时才用签到接口返回值。
	if rec.CreditsBefore >= 0 && rec.CreditsAfter >= 0 {
		rec.CreditsGained = rec.CreditsAfter - rec.CreditsBefore
	}
	if s.cfg.Records != nil {
		s.cfg.Records.Add(rec)
	}
	log.Printf("checkin %s: status=%s before=%d after=%d gained=%d", uid, rec.Status, rec.CreditsBefore, rec.CreditsAfter, rec.CreditsGained)
}

// joinMsg 拼接两条记录说明。
func joinMsg(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "；" + b
	}
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "refresh session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
	}
}

// RunModelRefreshNow 立即拉上游刷新模型列表；Store / Pool / Upstream 任一为空时跳过。
func (s *Scheduler) RunModelRefreshNow() {
	if s.cfg.Models == nil || s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	acct := s.cfg.Pool.Pick()
	if acct == nil {
		log.Printf("model_refresh: 无可用账号，跳过")
		return
	}
	items, err := s.cfg.Upstream.FetchModels(acct)
	if err != nil {
		log.Printf("model_refresh: 拉上游失败: %v", err)
		return
	}
	s.cfg.Models.Set(items)
	log.Printf("model_refresh: 已刷新模型列表，共 %d 个", len(items))
}
