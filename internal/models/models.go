// Package models 维护运行时可用模型列表：
// 启动时从静态表种子，文件存在时优先；管理页可触发刷新、定时刷新覆盖并落盘。
// 不再每次请求都打上游 /api/models/available。
package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lobsterai2api/internal/upstream"
)

// StaticModels 是已知模型列表（仅 ID），作为首次启动 / 文件丢失时的种子。
// 完整元数据（Name / ContextWindow 等）由运行期首次刷新后填充。
var StaticModels = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"MiniMax-M3",
	"MiniMax-M2.7",
	"qwen3.7-max",
	"qwen3.7-plus",
	"qwen3.6-plus",
	"qwen3.5-plus-2026-04-20",
	"kimi-k2.7-code",
	"kimi-k2.7-code-highspeed",
	"kimi-k2.6",
	"kimi-k2.5",
	"doubao-seed-2-1-pro-260628",
	"doubao-seed-2-1-turbo-260628",
	"doubao-seed-2-0-code-preview-260215",
	"glm-5.2",
	"glm-5.1",
	"glm-5v-turbo",
	"glm-5",
}

// fileFormat 落盘格式：保留每个模型的完整元数据。
type fileFormat struct {
	Items         []upstream.ModelMeta `json:"items"`
	LastRefreshed int64                 `json:"last_refreshed"` // 最后一次成功刷新的 Unix 秒；0 = 从未刷新
}

// Store 线程安全的运行时模型存储。
type Store struct {
	mu        sync.RWMutex
	fp        string
	items     []upstream.ModelMeta
	refreshed time.Time
}

// Load 加载 fp 上的列表；文件缺失或损坏回退到 StaticModels（仅 ID，无元数据）。
func Load(fp string) *Store {
	s := &Store{fp: fp}
	if fp != "" {
		if raw, err := os.ReadFile(fp); err == nil && len(raw) > 0 {
			var ff fileFormat
			if json.Unmarshal(raw, &ff) == nil && len(ff.Items) > 0 {
				s.items = append([]upstream.ModelMeta(nil), ff.Items...)
				if ff.LastRefreshed > 0 {
					s.refreshed = time.Unix(ff.LastRefreshed, 0)
				}
				return s
			}
		}
	}
	seeds := make([]upstream.ModelMeta, 0, len(StaticModels))
	for _, id := range StaticModels {
		seeds = append(seeds, upstream.ModelMeta{ID: id})
	}
	s.items = seeds
	return s
}

// IDs 返回当前模型 ID 列表的副本；调用方修改不影响内部状态。
func (s *Store) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.items))
	for _, m := range s.items {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out
}

// Items 返回当前所有模型元数据的副本（管理页 Models 页使用）。
func (s *Store) Items() []upstream.ModelMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]upstream.ModelMeta(nil), s.items...)
}

// LastRefreshed 返回最后一次成功刷新的时间；零值表示尚未刷新。
func (s *Store) LastRefreshed() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.refreshed
}

// Set 替换模型列表并刷新 LastRefreshed；随后落盘。
func (s *Store) Set(items []upstream.ModelMeta) {
	s.mu.Lock()
	s.items = append([]upstream.ModelMeta(nil), items...)
	s.refreshed = time.Now()
	s.saveLocked()
	s.mu.Unlock()
}

// saveLocked 写盘（tmp+rename）。空 fp 时跳过。
func (s *Store) saveLocked() {
	if s.fp == "" {
		return
	}
	ff := fileFormat{Items: s.items}
	if !s.refreshed.IsZero() {
		ff.LastRefreshed = s.refreshed.Unix()
	}
	raw, err := json.MarshalIndent(ff, "", "  ")
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