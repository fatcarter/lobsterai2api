// Package checkin 签到记录存储：每次签到结果追加到 JSON 文件，只保留最近若干条。
package checkin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StatusError 签到执行失败（网络/上游报错）；其余状态沿用 upstream 包的签到状态字面量。
const StatusError = "error"

// MaxRecords 默认保留的记录条数上限，超出后丢弃最旧的。
const MaxRecords = 500

// Record 单次签到记录。CreditsBefore/After 为 -1 表示当时查询失败、数值未知。
type Record struct {
	Time          time.Time `json:"time"`
	UID           string    `json:"uid"`
	Nickname      string    `json:"nickname,omitempty"`
	Status        string    `json:"status"`            // success / already_claimed / no_activity / error
	Message       string    `json:"message,omitempty"` // 失败原因或附加说明
	CreditsBefore int64     `json:"credits_before"`    // 签到前积分
	CreditsAfter  int64     `json:"credits_after"`     // 签到后积分
	CreditsGained int64     `json:"credits_gained"`    // after-before；两端未知时取签到接口返回值
}

// fileFormat 落盘格式。
type fileFormat struct {
	Records []Record `json:"records"`
}

// Store 线程安全的签到记录存储：内存缓存 + 原子写回。
type Store struct {
	mu      sync.Mutex
	fp      string
	max     int
	records []Record
}

// Load 从 fp 读取记录；文件不存在或损坏时从空开始，不返回错误。
func Load(fp string, max int) *Store {
	s := &Store{fp: fp, max: max}
	if s.max <= 0 {
		s.max = MaxRecords
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return s
	}
	var ff fileFormat
	if json.Unmarshal(raw, &ff) != nil {
		return s
	}
	if len(ff.Records) > s.max {
		ff.Records = ff.Records[len(ff.Records)-s.max:]
	}
	s.records = ff.Records
	return s
}

// Add 追加一条记录并写回文件；超过上限时丢弃最旧的。
func (s *Store) Add(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	if len(s.records) > s.max {
		s.records = s.records[len(s.records)-s.max:]
	}
	s.saveLocked()
}

// List 返回最近的 limit 条记录（新的在前）；limit<=0 表示全部。
func (s *Store) List(limit int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyNewestFirstLocked(limit, 0)
}

// Page 返回第 page 页的记录（page 从 1 开始），size 为每页条数。
// 返回的 records 顺序为最新在前；page<=0 视作 1，size<=0 视作 50。
// 同时返回总数 total，便于管理页渲染分页。
// excludeStatus 非空时从结果与 total 中过滤掉匹配状态（如 "already_claimed"）的记录，
// 避免 "已签到" 大量挤占列表，分页参数照旧按过滤后的计数计算。
func (s *Store) Page(page, size int, excludeStatus ...string) (records []Record, total int) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := filterRecordsLocked(s.records, excludeStatus)
	total = len(filtered)
	if total == 0 {
		return nil, 0
	}
	// 最新在前：按"倒数索引"切片。
	end := total - (page-1)*size
	if end <= 0 {
		return nil, total
	}
	start := end - size
	if start < 0 {
		start = 0
	}
	out := make([]Record, 0, end-start)
	for i := end - 1; i >= start; i-- {
		out = append(out, filtered[i])
	}
	return out, total
}

// filterRecordsLocked 过滤掉 status 与任一 excludeStatus 相等的记录；调用方持锁。
func filterRecordsLocked(in []Record, exclude []string) []Record {
	if len(exclude) == 0 {
		return in
	}
	skip := make(map[string]struct{}, len(exclude))
	for _, s := range exclude {
		if s != "" {
			skip[s] = struct{}{}
		}
	}
	if len(skip) == 0 {
		return in
	}
	out := make([]Record, 0, len(in))
	for _, r := range in {
		if _, bad := skip[r.Status]; bad {
			continue
		}
		out = append(out, r)
	}
	return out
}

// copyNewestFirstLocked 内部：调用方持锁；limit<=0 取全部，offset 跳过前 N 条（最新在前）。
func (s *Store) copyNewestFirstLocked(limit, offset int) []Record {
	if limit <= 0 || limit > len(s.records)-offset {
		limit = len(s.records) - offset
	}
	if limit <= 0 {
		return nil
	}
	out := make([]Record, limit)
	for i := 0; i < limit; i++ {
		out[i] = s.records[len(s.records)-1-offset-i]
	}
	return out
}

// saveLocked 原子写回（tmp + rename），与 pool 的 state.json 同套路。
func (s *Store) saveLocked() {
	if s.fp == "" {
		return
	}
	raw, err := json.MarshalIndent(fileFormat{Records: s.records}, "", "  ")
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
