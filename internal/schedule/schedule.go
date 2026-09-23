// Package schedule 定时设置：内存读写 + 变更通知 + 文件持久化。
// 文件（schedule_file）存在时优先于 config.json / 环境变量，管理页保存后写入该文件并即时生效。
package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fileFormat 落盘格式。
type fileFormat struct {
	CheckinHours          []int  `json:"checkin_hours"`           // 签到整点小时；空 = 关闭
	KeepaliveHours        []int  `json:"keepalive_hours"`         // 保活整点小时；空 = 关闭
	CreditRefreshInterval string `json:"credit_refresh_interval"` // 额度刷新间隔；空 = 关闭
	ModelRefreshHours     []int  `json:"model_refresh_hours"`     // 模型刷新整点小时；空 = 关闭
}

// Settings 线程安全的定时设置。initial* 是 Load 时的配置默认值，
// 供 Reset（恢复默认）使用；checkin/keepalive/creditRefresh/modelRefresh 是当前生效值。
type Settings struct {
	mu                   sync.Mutex
	fp                   string // 持久化文件；空 = 不落盘
	ch                   chan struct{}
	initialCheckin       []int
	initialKeepalive     []int
	initialCreditRefresh time.Duration
	checkin              []int
	keepalive            []int
	creditRefresh        time.Duration
	modelRefresh         []int
}

// Load 从 fp 读取设置；文件不存在或损坏时使用配置默认值（checkin/keepalive/creditRefresh/modelRefresh）。
func Load(fp string, checkin, keepalive, modelRefresh []int, creditRefresh time.Duration) *Settings {
	s := &Settings{
		fp:                   fp,
		ch:                   make(chan struct{}),
		initialCheckin:       clone(checkin),
		initialKeepalive:     clone(keepalive),
		initialCreditRefresh: creditRefresh,
		checkin:              clone(checkin),
		keepalive:            clone(keepalive),
		creditRefresh:        creditRefresh,
		modelRefresh:         clone(modelRefresh),
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return s
	}
	var ff fileFormat
	if json.Unmarshal(raw, &ff) != nil {
		return s
	}
	interval, err := ParseInterval(ff.CreditRefreshInterval)
	if err != nil {
		return s
	}
	s.checkin = ff.CheckinHours
	s.keepalive = ff.KeepaliveHours
	s.creditRefresh = interval
	s.modelRefresh = ff.ModelRefreshHours
	return s
}

// Get 返回当前生效的签到/保活/模型刷新小时列表（副本）和额度刷新间隔。
func (s *Settings) Get() (checkin, keepalive, modelRefresh []int, creditRefresh time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.checkin), clone(s.keepalive), clone(s.modelRefresh), s.creditRefresh
}

// Snapshot 原子返回当前设置和对应的变更通道，避免读取设置后错过紧接着发生的变更通知。
func (s *Settings) Snapshot() (checkin, keepalive, modelRefresh []int, creditRefresh time.Duration, changed <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.checkin), clone(s.keepalive), clone(s.modelRefresh), s.creditRefresh, s.ch
}

// Set 更新设置、落盘并通知订阅者；小时值和间隔需由调用方先校验。
func (s *Settings) Set(checkin, keepalive, modelRefresh []int, creditRefresh time.Duration) {
	s.mu.Lock()
	s.checkin = clone(checkin)
	s.keepalive = clone(keepalive)
	s.creditRefresh = creditRefresh
	s.modelRefresh = clone(modelRefresh)
	close(s.ch)
	s.ch = make(chan struct{})
	s.saveLocked()
	s.mu.Unlock()
}

// Reset 删除持久化文件并恢复为 Load 时的配置默认值。
func (s *Settings) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fp != "" {
		if err := os.Remove(s.fp); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除定时设置文件失败: %w", err)
		}
	}
	s.checkin = clone(s.initialCheckin)
	s.keepalive = clone(s.initialKeepalive)
	s.creditRefresh = s.initialCreditRefresh
	s.modelRefresh = nil
	close(s.ch)
	s.ch = make(chan struct{})
	return nil
}

// C 返回变更通知通道；每次 Set/Reset 都会关闭并替换该通道。
func (s *Settings) C() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ch
}

// ValidateHours 校验小时列表；每个值必须在 0-23。空列表合法（表示关闭）。
func ValidateHours(hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("小时必须为 0-23，收到 %d", h)
		}
	}
	return nil
}

// ParseInterval 解析周期任务间隔；空、"-"、"0" 表示关闭，非零间隔不能小于 1 分钟。
func ParseInterval(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "-" || v == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("间隔格式无效，应为 30m、2h 或 1h30m")
	}
	if d < time.Minute {
		return 0, fmt.Errorf("间隔不能小于 1 分钟")
	}
	return d, nil
}

// FormatInterval 返回管理接口和持久化文件使用的间隔字符串；0 表示关闭。
func FormatInterval(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d%time.Minute == 0 {
		hours := d / time.Hour
		minutes := (d % time.Hour) / time.Minute
		switch {
		case hours > 0 && minutes > 0:
			return fmt.Sprintf("%dh%dm", hours, minutes)
		case hours > 0:
			return fmt.Sprintf("%dh", hours)
		default:
			return fmt.Sprintf("%dm", minutes)
		}
	}
	return d.String()
}

// saveLocked 原子写回（tmp + rename），与 pool 的 state.json 同套路。
func (s *Settings) saveLocked() {
	if s.fp == "" {
		return
	}
	raw, err := json.MarshalIndent(fileFormat{
		CheckinHours:          s.checkin,
		KeepaliveHours:        s.keepalive,
		CreditRefreshInterval: FormatInterval(s.creditRefresh),
		ModelRefreshHours:     s.modelRefresh,
	}, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.fp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.fp)
}

func clone(in []int) []int {
	return append([]int(nil), in...)
}
