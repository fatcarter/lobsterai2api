// Package stats 提供请求计数与 token 用量的聚合；内存热路径 + 异步落盘，
// 重启后从 JSON 文件恢复，确保管理页的累计数据不会丢。
package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ModelRow 管理页使用的按模型聚合行。
type ModelRow struct {
	Model       string `json:"model"`
	Total       int64  `json:"total"`
	Success     int64  `json:"success"`
	Failed      int64  `json:"failed"`
	Prompt      int64  `json:"prompt_tokens"`
	Completion  int64  `json:"completion_tokens"`
	SuccessRate float64 `json:"success_rate"`
}

// Snapshot 管理页读取的聚合快照。
type Snapshot struct {
	Total            int64      `json:"total"`
	Success          int64      `json:"success"`
	Failed           int64      `json:"failed"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	TotalTokens      int64      `json:"total_tokens"`
	SuccessRate      float64    `json:"success_rate"`
	ByModel          []ModelRow `json:"by_model"`
	StartedAt        int64      `json:"started_at,omitempty"`
}

// fileFormat 落盘格式（最小化：模型键 → 行，聚合字段平铺）。
type fileFormat struct {
	StartedAt     int64               `json:"started_at,omitempty"`
	Total         int64               `json:"total"`
	Success       int64               `json:"success"`
	Failed        int64               `json:"failed"`
	Prompt        int64               `json:"prompt_tokens"`
	Completion    int64               `json:"completion_tokens"`
	ByModel       map[string]*ModelRow `json:"by_model,omitempty"`
}

// Recorder 线程安全的请求计数器；Record / AddSuccess / AddFailure / RecordTokens
// 走 hot path 仅操作内存，落盘由 Flush 异步触发。
type Recorder struct {
	mu       sync.Mutex
	file     string
	data     *fileFormat
	dirty    bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// Load 从 file 读取上次累计；文件不存在或损坏时返回空 Recorder，
// 不视为错误（首次启动场景）。
func Load(file string) *Recorder {
	r := &Recorder{file: file, data: newData()}
	if file == "" {
		return r
	}
	raw, err := os.ReadFile(file)
	if err != nil || len(raw) == 0 {
		return r
	}
	var ff fileFormat
	if json.Unmarshal(raw, &ff) != nil {
		return r
	}
	if ff.ByModel == nil {
		ff.ByModel = map[string]*ModelRow{}
	}
	r.data = &ff
	return r
}

// Run 启动定期刷盘协程，stop 关闭时优雅退出；周期为 30 秒。
func (r *Recorder) Run(stop <-chan struct{}) {
	if r.file == "" {
		<-stop
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			r.Flush()
			return
		case <-ticker.C:
			r.Flush()
		}
	}
}

// Flush 立即把内存数据写到文件（原子写）；未脏时跳过。
func (r *Recorder) Flush() {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return
	}
	raw, err := json.MarshalIndent(r.data, "", "  ")
	r.mu.Unlock()
	if err != nil {
		return
	}
	if dir := filepath.Dir(r.file); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := r.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, r.file)
}

// RecordTokens 累计指定模型的 prompt / completion tokens（取自上游 usage 帧）。
func (r *Recorder) RecordTokens(model string, prompt, completion int64) {
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data.StartedAt == 0 {
		r.data.StartedAt = time.Now().Unix()
	}
	r.data.Total++
	r.data.Success++
	r.data.Prompt += prompt
	r.data.Completion += completion
	m := r.getOrCreateModelLocked(model)
	m.Total++
	m.Success++
	m.Prompt += prompt
	m.Completion += completion
	r.dirty = true
}

// AddFailure 记录一次失败；model 为空时记入 "unknown"。
func (r *Recorder) AddFailure(model string) {
	if model == "" {
		model = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data.StartedAt == 0 {
		r.data.StartedAt = time.Now().Unix()
	}
	r.data.Total++
	r.data.Failed++
	m := r.getOrCreateModelLocked(model)
	m.Total++
	m.Failed++
	r.dirty = true
}

// getOrCreateModelLocked 内部：调用方持锁。
func (r *Recorder) getOrCreateModelLocked(model string) *ModelRow {
	if r.data.ByModel == nil {
		r.data.ByModel = map[string]*ModelRow{}
	}
	if model == "" {
		model = "unknown"
	}
	m, ok := r.data.ByModel[model]
	if !ok {
		m = &ModelRow{Model: model}
		r.data.ByModel[model] = m
	}
	return m
}

// Report 返回当前聚合快照（按请求数倒序）。
func (r *Recorder) Report() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := make([]ModelRow, 0, len(r.data.ByModel))
	for _, m := range r.data.ByModel {
		row := *m
		if row.Total > 0 {
			row.SuccessRate = float64(row.Success) / float64(row.Total)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return rows[i].Model < rows[j].Model
	})
	snap := Snapshot{
		Total:            r.data.Total,
		Success:          r.data.Success,
		Failed:           r.data.Failed,
		PromptTokens:     r.data.Prompt,
		CompletionTokens: r.data.Completion,
		TotalTokens:      r.data.Prompt + r.data.Completion,
		ByModel:          rows,
		StartedAt:        r.data.StartedAt,
	}
	if snap.Total > 0 {
		snap.SuccessRate = float64(snap.Success) / float64(snap.Total)
	}
	return snap
}

func newData() *fileFormat {
	return &fileFormat{ByModel: map[string]*ModelRow{}}
}