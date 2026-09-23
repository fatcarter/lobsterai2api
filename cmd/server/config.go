// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"lobsterai2api/internal/schedule"
)

// Config 顶层配置。
type Config struct {
	Listen       string `json:"listen"`        // ":8367"
	APIKey       string `json:"api_key"`       // 空 = 不鉴权
	AuthDir      string `json:"auth_dir"`      // ./auths
	StateFile    string `json:"state_file"`    // ./data/state.json
	CheckinFile  string `json:"checkin_file"`  // ./data/checkin.json 签到记录
	ScheduleFile string `json:"schedule_file"` // ./data/schedule.json 定时设置（管理页修改后写入，优先于 schedule 配置）
	StatsFile    string `json:"stats_file"`    // ./data/stats.json 请求计数（管理页统计页）
	ModelsFile   string `json:"models_file"`   // ./data/models.json 运行时模型列表（管理页刷新后落盘）

	Cooldown struct {
		HardCredit  string `json:"hard_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 默认 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours          []int  `json:"checkin_hours"`           // [9,21]
		KeepaliveHours        []int  `json:"keepalive_hours"`         // [22]
		CreditRefreshInterval string `json:"credit_refresh_interval"` // 额度刷新间隔，例如 "30m" / "2h"；空 = 关闭
		ModelRefreshHours     []int  `json:"model_refresh_hours"`     // 模型定时刷新整点小时；空 = 关闭
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 默认 180
		// UpdateURL 客户端版本解析接口（签到用）；空值用官方默认地址。
		UpdateURL string `json:"update_url"`
	} `json:"upstream"`

	// Login 管理页 OAuth 登录配置；未配置门户时管理页仍可打开，登录接口返回配置缺失。
	Login struct {
		Portal string `json:"portal"` // 登录门户根地址，授权页位于 /portal#/login
		// CallbackPort 授权地址里 127.0.0.1 回调使用的端口，默认取 listen 端口。
		// Docker 部署下容器内固定 8367，需填宿主机端口，登录后浏览器才能落到本服务的提示页。
		CallbackPort int `json:"callback_port"`
	} `json:"login"`

	// 解析后
	HardCreditDur    time.Duration `json:"-"`
	SoftRateDur      time.Duration `json:"-"`
	ErrCooldownDur   time.Duration `json:"-"`
	CreditRefreshDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:       ":8367",
		APIKey:       "",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		CheckinFile:  "./data/checkin.json",
		ScheduleFile: "./data/schedule.json",
		StatsFile:    "./data/stats.json",
		ModelsFile:   "./data/models.json",
	}
	c.Cooldown.HardCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.CreditRefreshInterval = "30m"
	c.Upstream.TimeoutSeconds = 180
	return c
}

// Load 从文件读，再用 LB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := applyEnv(c); err != nil {
		return nil, err
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) error {
	if v := os.Getenv("LB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("LB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("LB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("LB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("LB2A_CHECKIN_FILE"); v != "" {
		c.CheckinFile = v
	}
	if v := os.Getenv("LB2A_SCHEDULE_FILE"); v != "" {
		c.ScheduleFile = v
	}
	if v := os.Getenv("LB2A_STATS_FILE"); v != "" {
		c.StatsFile = v
	}
	if v := os.Getenv("LB2A_MODELS_FILE"); v != "" {
		c.ModelsFile = v
	}
	if v := os.Getenv("LB2A_CHECKIN_HOURS"); v != "" {
		hours, err := parseHours(v)
		if err != nil {
			return fmt.Errorf("LB2A_CHECKIN_HOURS: %w", err)
		}
		c.Schedule.CheckinHours = hours
	}
	if v := os.Getenv("LB2A_KEEPALIVE_HOURS"); v != "" {
		hours, err := parseHours(v)
		if err != nil {
			return fmt.Errorf("LB2A_KEEPALIVE_HOURS: %w", err)
		}
		c.Schedule.KeepaliveHours = hours
	}
	if v := os.Getenv("LB2A_CREDIT_REFRESH_INTERVAL"); v != "" {
		c.Schedule.CreditRefreshInterval = v
	}
	if v := os.Getenv("LB2A_HARD_CREDIT"); v != "" {
		c.Cooldown.HardCredit = v
	}
	if v := os.Getenv("LB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("LB2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("LB2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("LB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("LB2A_LOGIN_PORTAL"); v != "" {
		c.Login.Portal = v
	}
	if v := os.Getenv("LB2A_CALLBACK_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Login.CallbackPort = n
		}
	}
	if v := os.Getenv("LB2A_UPDATE_URL"); v != "" {
		c.Upstream.UpdateURL = v
	}
	return nil
}

// parseHours 解析 "9,21" 形式的整点小时列表；"-" 表示关闭对应任务。
func parseHours(v string) ([]int, error) {
	if v == "-" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	hours := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 23 {
			return nil, fmt.Errorf("无效的小时 %q（应为 0-23 的逗号分隔列表）", p)
		}
		hours = append(hours, n)
	}
	return hours, nil
}

func (c *Config) normalize() error {
	var err error
	if c.HardCreditDur, err = time.ParseDuration(c.Cooldown.HardCredit); err != nil {
		return fmt.Errorf("cooldown.hard_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	for _, h := range c.Schedule.CheckinHours {
		if h < 0 || h > 23 {
			return fmt.Errorf("schedule.checkin_hours: 无效的小时 %d（应为 0-23）", h)
		}
	}
	for _, h := range c.Schedule.KeepaliveHours {
		if h < 0 || h > 23 {
			return fmt.Errorf("schedule.keepalive_hours: 无效的小时 %d（应为 0-23）", h)
		}
	}
	if c.CreditRefreshDur, err = schedule.ParseInterval(c.Schedule.CreditRefreshInterval); err != nil {
		return fmt.Errorf("schedule.credit_refresh_interval: %w", err)
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 180
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 回调端口默认取监听端口；Docker 下容器内监听 8367，宿主机端口不同的话必须显式配置。
	if c.Login.CallbackPort <= 0 || c.Login.CallbackPort > 65535 {
		c.Login.CallbackPort = 0
		if _, port, err := net.SplitHostPort(c.Listen); err == nil {
			if n, err := strconv.Atoi(port); err == nil && n > 0 && n <= 65535 {
				c.Login.CallbackPort = n
			}
		}
	}
	return nil
}

// CallbackURI 返回授权地址使用的本机回调地址；回调端口不可用时返回空串，登录接口据此报配置缺失。
func (c *Config) CallbackURI(path string) string {
	if c.Login.CallbackPort <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", c.Login.CallbackPort, path)
}
