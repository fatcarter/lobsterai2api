package checkin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T, max int) *Store {
	t.Helper()
	return Load(filepath.Join(t.TempDir(), "checkin.json"), max)
}

func TestStoreRoundtrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "checkin.json")
	s := Load(fp, MaxRecords)
	now := time.Now()
	s.Add(Record{Time: now, UID: "u1", Status: "success", CreditsBefore: 100, CreditsAfter: 200, CreditsGained: 100})

	reloaded := Load(fp, MaxRecords)
	got := reloaded.List(0)
	if len(got) != 1 {
		t.Fatalf("重载后应有 1 条记录，实际 %d", len(got))
	}
	if got[0].UID != "u1" || got[0].CreditsBefore != 100 || got[0].CreditsAfter != 200 || got[0].CreditsGained != 100 {
		t.Fatalf("记录内容不正确: %+v", got[0])
	}
}

func TestStoreTrimOldest(t *testing.T) {
	s := newStore(t, 3)
	for i := 0; i < 5; i++ {
		s.Add(Record{UID: string(rune('a' + i)), Time: time.Unix(int64(i+1), 0)})
	}
	got := s.List(0)
	if len(got) != 3 {
		t.Fatalf("超上限后应保留 3 条，实际 %d", len(got))
	}
	// 保留最新 3 条：c d e（新在前）
	if got[0].UID != "e" || got[1].UID != "d" || got[2].UID != "c" {
		t.Fatalf("应丢弃最旧的记录，实际 %+v", got)
	}
}

func TestStoreListNewestFirst(t *testing.T) {
	s := newStore(t, MaxRecords)
	s.Add(Record{UID: "old", Time: time.Unix(1, 0)})
	s.Add(Record{UID: "new", Time: time.Unix(2, 0)})
	got := s.List(0)
	if len(got) != 2 || got[0].UID != "new" || got[1].UID != "old" {
		t.Fatalf("记录应按时间倒序返回，实际 %+v", got)
	}
}

func TestStoreLoadCorrupt(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "checkin.json")
	if err := os.WriteFile(fp, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("写入损坏文件失败: %v", err)
	}
	s := Load(fp, MaxRecords)
	if got := s.List(0); len(got) != 0 {
		t.Fatalf("损坏文件应从空开始，实际 %+v", got)
	}
	// 损坏文件不阻塞后续写入。
	s.Add(Record{UID: "u1", Status: "success"})
	if got := s.List(0); len(got) != 1 {
		t.Fatalf("损坏文件恢复后应可写入，实际 %d 条", len(got))
	}
}

func TestStorePageExcludesClaimed(t *testing.T) {
	s := newStore(t, MaxRecords)
	for i := 1; i <= 10; i++ {
		status := "success"
		if i%2 == 0 {
			status = "already_claimed"
		}
		s.Add(Record{UID: "u" + string(rune('a'+i-1)), Time: time.Unix(int64(i), 0), Status: status})
	}
	// 默认：不过滤 → 10 条。
	rows, total := s.Page(1, 10)
	if total != 10 || len(rows) != 10 {
		t.Fatalf("默认应返回 10 条，实际 total=%d rows=%d", total, len(rows))
	}
	// hide_claimed：5 条 success 留下。
	rows, total = s.Page(1, 10, "already_claimed")
	if total != 5 || len(rows) != 5 {
		t.Fatalf("过滤后应剩 5 条 success，实际 total=%d rows=%d", total, len(rows))
	}
	for _, r := range rows {
		if r.Status == "already_claimed" {
			t.Fatalf("过滤后不应包含已签到记录: %+v", r)
		}
	}
	// size=3：分页应按过滤后的 5 条计算。
	rows, total = s.Page(2, 3, "already_claimed")
	if total != 5 || len(rows) != 2 {
		t.Fatalf("过滤后分页应按 5 条计算，实际 total=%d rows=%d", total, len(rows))
	}
}

func TestStorePage(t *testing.T) {
	s := newStore(t, MaxRecords)
	for i := 1; i <= 25; i++ {
		s.Add(Record{UID: string(rune('a' + (i-1)%26)), Time: time.Unix(int64(i), 0)})
	}
	cases := []struct {
		page, size int
		wantTotal  int
		wantCount  int
		wantFirst  string
		wantLast   string
	}{
		{page: 1, size: 10, wantTotal: 25, wantCount: 10, wantFirst: "y", wantLast: "p"},
		{page: 2, size: 10, wantTotal: 25, wantCount: 10, wantFirst: "o", wantLast: "f"},
		{page: 3, size: 10, wantTotal: 25, wantCount: 5, wantFirst: "e", wantLast: "a"},
		{page: 4, size: 10, wantTotal: 25, wantCount: 0},
		{page: 1, size: 0, wantTotal: 25, wantCount: 25, wantFirst: "y", wantLast: "a"},
		{page: -1, size: 5, wantTotal: 25, wantCount: 5, wantFirst: "y", wantLast: "u"},
	}
	for _, tc := range cases {
		rows, total := s.Page(tc.page, tc.size)
		if total != tc.wantTotal {
			t.Errorf("page=%d size=%d total=%d, want %d", tc.page, tc.size, total, tc.wantTotal)
		}
		if len(rows) != tc.wantCount {
			t.Errorf("page=%d size=%d rows=%d, want %d", tc.page, tc.size, len(rows), tc.wantCount)
		}
		if tc.wantCount > 0 {
			if rows[0].UID != tc.wantFirst {
				t.Errorf("page=%d size=%d 首条=%s, want %s", tc.page, tc.size, rows[0].UID, tc.wantFirst)
			}
			if rows[len(rows)-1].UID != tc.wantLast {
				t.Errorf("page=%d size=%d 末条=%s, want %s", tc.page, tc.size, rows[len(rows)-1].UID, tc.wantLast)
			}
		}
	}
}
