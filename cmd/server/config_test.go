package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeCallbackPortFromListen(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		want   int
	}{
		{name: "仅端口", listen: ":8367", want: 8367},
		{name: "带主机", listen: "127.0.0.1:9000", want: 9000},
		{name: "裸端口补冒号", listen: "9100", want: 9100},
		{name: "端口非法", listen: ":0", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Listen = tc.listen
			if err := c.normalize(); err != nil {
				t.Fatalf("normalize 失败: %v", err)
			}
			if c.Login.CallbackPort != tc.want {
				t.Fatalf("回调端口得到 %d，期望 %d", c.Login.CallbackPort, tc.want)
			}
		})
	}
}

func TestNormalizeKeepsExplicitCallbackPort(t *testing.T) {
	c := Default()
	c.Listen = ":8367"
	c.Login.CallbackPort = 18367 // Docker 下宿主机端口与容器端口不同
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize 失败: %v", err)
	}
	if c.Login.CallbackPort != 18367 {
		t.Fatalf("显式配置的回调端口被覆盖: %d", c.Login.CallbackPort)
	}
}

func TestNormalizeRejectsOutOfRangeCallbackPort(t *testing.T) {
	c := Default()
	c.Listen = ":8367"
	c.Login.CallbackPort = 70000
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize 失败: %v", err)
	}
	if c.Login.CallbackPort != 8367 {
		t.Fatalf("越界端口应回退到监听端口，实际 %d", c.Login.CallbackPort)
	}
}

func TestCallbackURI(t *testing.T) {
	c := Default()
	c.Login.CallbackPort = 8367
	if got := c.CallbackURI("/auth/callback"); got != "http://127.0.0.1:8367/auth/callback" {
		t.Fatalf("回调地址不正确: %q", got)
	}
	c.Login.CallbackPort = 0
	if got := c.CallbackURI("/auth/callback"); got != "" {
		t.Fatalf("端口不可用时应返回空串，实际 %q", got)
	}
}

func TestDefaultScheduleAndCheckinFile(t *testing.T) {
	c := Default()
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
		t.Fatalf("默认签到小时应为 [9 21]，实际 %v", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
		t.Fatalf("默认保活小时应为 [22]，实际 %v", c.Schedule.KeepaliveHours)
	}
	if c.CheckinFile != "./data/checkin.json" {
		t.Fatalf("默认签到记录文件应为 ./data/checkin.json，实际 %q", c.CheckinFile)
	}
	if c.Schedule.CreditRefreshInterval != "30m" {
		t.Fatalf("默认额度刷新间隔应为 30m，实际 %q", c.Schedule.CreditRefreshInterval)
	}
}

func TestParseHours(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []int
		wantErr bool
	}{
		{name: "常规", in: "9,21", want: []int{9, 21}},
		{name: "带空格", in: " 8 , 20 ", want: []int{8, 20}},
		{name: "空段忽略", in: "9,", want: []int{9}},
		{name: "关闭", in: "-", want: nil},
		{name: "越界", in: "24", wantErr: true},
		{name: "非数字", in: "9,x", wantErr: true},
		{name: "负数", in: "-1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHours(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("输入 %q 应报错", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("得到 %v，期望 %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("得到 %v，期望 %v", got, tc.want)
				}
			}
		})
	}
}

func TestLoadCheckinEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    []int
		wantErr bool
	}{
		{name: "覆盖默认", env: "8,20", want: []int{8, 20}},
		{name: "关闭签到", env: "-", want: nil},
		{name: "非法值", env: "99", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LB2A_CHECKIN_HOURS", tc.env)
			c, err := Load("")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LB2A_CHECKIN_HOURS=%q 应报错", tc.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("加载失败: %v", err)
			}
			if len(c.Schedule.CheckinHours) != len(tc.want) {
				t.Fatalf("得到 %v，期望 %v", c.Schedule.CheckinHours, tc.want)
			}
			for i := range tc.want {
				if c.Schedule.CheckinHours[i] != tc.want[i] {
					t.Fatalf("得到 %v，期望 %v", c.Schedule.CheckinHours, tc.want)
				}
			}
		})
	}
}

func TestLoadCheckinFileAndUpdateURLEnv(t *testing.T) {
	t.Setenv("LB2A_CHECKIN_FILE", "/tmp/x/checkin.json")
	t.Setenv("LB2A_UPDATE_URL", "https://example.com/update")
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if c.CheckinFile != "/tmp/x/checkin.json" {
		t.Fatalf("签到记录文件未生效，实际 %q", c.CheckinFile)
	}
	if c.Upstream.UpdateURL != "https://example.com/update" {
		t.Fatalf("版本解析接口未生效，实际 %q", c.Upstream.UpdateURL)
	}
}

func TestLoadCreditRefreshIntervalEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "覆盖默认", env: "2h", want: "2h"},
		{name: "关闭", env: "-", want: "-"},
		{name: "非法值", env: "30s", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LB2A_CREDIT_REFRESH_INTERVAL", tc.env)
			c, err := Load("")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LB2A_CREDIT_REFRESH_INTERVAL=%q 应报错", tc.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("加载失败: %v", err)
			}
			if c.Schedule.CreditRefreshInterval != tc.want {
				t.Fatalf("额度刷新间隔未保留原始配置，实际 %q", c.Schedule.CreditRefreshInterval)
			}
			if tc.env == "-" && c.CreditRefreshDur != 0 {
				t.Fatalf("关闭额度刷新后持续时间应为 0，实际 %v", c.CreditRefreshDur)
			}
		})
	}
}

func TestLoadJSONCanDisableCheckin(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[]}}`), 0o600); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(c.Schedule.CheckinHours) != 0 {
		t.Fatalf("空列表应表示关闭签到，实际 %v", c.Schedule.CheckinHours)
	}
}
