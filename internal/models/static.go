package models

import "lobsterai2api/internal/upstream"

// StaticModelsItems 把 StaticModels（仅 ID 的种子列表）转成上游 ModelMeta 切片，
// 供 Store 为 nil 时的管理页兜底展示。
func StaticModelsItems() []upstream.ModelMeta {
	out := make([]upstream.ModelMeta, 0, len(StaticModels))
	for _, id := range StaticModels {
		out = append(out, upstream.ModelMeta{ID: id})
	}
	return out
}