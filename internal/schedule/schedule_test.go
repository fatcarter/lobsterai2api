package schedule

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadUsesInitialsWithoutFile(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "nope.json"), []int{9, 21}, []int{22}, nil, 30*time.Minute)
	chk, keep, _, interval := s.Get()
	if len(chk) != 2 || chk[0] != 9 || chk[1] != 21 || len(keep) != 1 || keep[0] != 22 {
		t.Fatalf("初始值不正确: %v %v", chk, keep)
	}
	if interval != 30*time.Minute {
		t.Fatalf("初始额度刷新间隔不正确: %v", interval)
	}
}

func TestLoadFileOverridesInitials(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "schedule.json")
	raw, _ := json.Marshal(fileFormat{CheckinHours: []int{8}, KeepaliveHours: []int{}, CreditRefreshInterval: "2h"})
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	s := Load(fp, []int{9, 21}, []int{22}, nil, 30*time.Minute)
	chk, keep, _, interval := s.Get()
	if len(chk) != 1 || chk[0] != 8 {
		t.Fatalf("文件应覆盖默认值: %v", chk)
	}
	if len(keep) != 0 {
		t.Fatalf("空列表应表示关闭: %v", keep)
	}
	if interval != 2*time.Hour {
		t.Fatalf("文件应覆盖额度刷新间隔: %v", interval)
	}
}

func TestLoadCorruptFileFallsBack(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "schedule.json")
	if err := os.WriteFile(fp, []byte("{bad"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	s := Load(fp, []int{9, 21}, []int{22}, nil, 30*time.Minute)
	chk, _, _, interval := s.Get()
	if len(chk) != 2 || chk[0] != 9 {
		t.Fatalf("损坏文件应回退默认值: %v", chk)
	}
	if interval != 30*time.Minute {
		t.Fatalf("损坏文件应回退默认额度刷新间隔: %v", interval)
	}
}

func TestSetPersistsAndNotifies(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "schedule.json")
	s := Load(fp, []int{9, 21}, []int{22}, nil, 0)
	ch := s.C()
	s.Set([]int{7, 12, 20}, []int{23}, nil, 45*time.Minute)

	select {
	case <-ch:
	default:
		t.Fatal("Set 后变更通道应立即关闭")
	}

	// 重载文件验证持久化。
	reloaded := Load(fp, []int{9, 21}, []int{22}, nil, 0)
	chk, keep, _, interval := reloaded.Get()
	if len(chk) != 3 || chk[0] != 7 || chk[2] != 20 || len(keep) != 1 || keep[0] != 23 {
		t.Fatalf("持久化值不正确: %v %v", chk, keep)
	}
	if interval != 45*time.Minute {
		t.Fatalf("额度刷新间隔持久化值不正确: %v", interval)
	}
}

func TestResetRestoresInitials(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "schedule.json")
	s := Load(fp, []int{9, 21}, []int{22}, nil, 30*time.Minute)
	s.Set([]int{1}, []int{2}, nil, 2*time.Hour)
	ch := s.C()
	if err := s.Reset(); err != nil {
		t.Fatalf("恢复默认失败: %v", err)
	}
	chk, keep, _, interval := s.Get()
	if len(chk) != 2 || chk[0] != 9 || len(keep) != 1 || keep[0] != 22 {
		t.Fatalf("恢复默认值不正确: %v %v", chk, keep)
	}
	if interval != 30*time.Minute {
		t.Fatalf("恢复默认额度刷新间隔不正确: %v", interval)
	}
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatal("恢复默认后应删除持久化文件")
	}
	select {
	case <-ch:
	default:
		t.Fatal("Reset 后变更通道应立即关闭")
	}
}

func TestValidateHours(t *testing.T) {
	if err := ValidateHours([]int{0, 23}); err != nil {
		t.Fatalf("边界值应合法: %v", err)
	}
	if err := ValidateHours(nil); err != nil {
		t.Fatalf("空列表应合法: %v", err)
	}
	if err := ValidateHours([]int{24}); err == nil {
		t.Fatal("越界值应报错")
	}
	if err := ValidateHours([]int{-1}); err == nil {
		t.Fatal("负值应报错")
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s := Load("", []int{9}, []int{22}, nil, 0)
	chk, _, _, _ := s.Get()
	chk[0] = 99
	got, _, _, _ := s.Get()
	if got[0] != 9 {
		t.Fatal("Get 返回的切片修改不应影响内部状态")
	}
}

func TestSetNotificationReplacesChannel(t *testing.T) {
	s := Load("", []int{9}, []int{22}, nil, 0)
	first := s.C()
	s.Set([]int{1}, []int{2}, nil, time.Hour)
	if got := s.C(); got == first {
		t.Fatal("通知通道应被替换")
	}
	// 旧通道已关闭，新通道可等待。
	select {
	case <-first:
	default:
		t.Fatal("旧通道应已关闭")
	}
	select {
	case <-s.C():
		t.Fatal("新通道不应已关闭")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestParseInterval(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "关闭空值", in: "", want: 0},
		{name: "关闭横线", in: "-", want: 0},
		{name: "关闭零", in: "0", want: 0},
		{name: "分钟", in: "30m", want: 30 * time.Minute},
		{name: "小时", in: "2h", want: 2 * time.Hour},
		{name: "复合", in: "1h30m", want: 90 * time.Minute},
		{name: "过短", in: "30s", wantErr: true},
		{name: "非法", in: "abc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseInterval(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("输入 %q 应报错", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("得到 %v，期望 %v", got, tc.want)
			}
		})
	}
}
