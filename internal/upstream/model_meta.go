package upstream

// ModelMeta 上游单个模型的元数据；管理页用 Name / ContextWindow / CostMultiplier / Description
// 来渲染展示，运行时仍以 ID 作为客户端调用 /v1/chat/completions 的 model 参数。
type ModelMeta struct {
	ID             string  `json:"id"`              // modelId，客户端调用参数
	Name           string  `json:"name,omitempty"`  // modelName，管理页显示名
	Provider       string  `json:"provider,omitempty"`
	ApiFormat      string  `json:"api_format,omitempty"`
	ContextWindow  int64   `json:"context_window,omitempty"`  // 上下文窗口（tokens）；0 = 未知
	CostMultiplier float64 `json:"cost_multiplier,omitempty"` // 消耗倍率（bridge）
	Description    string  `json:"description,omitempty"`
	// ReasoningEffort 该模型可用的思考等级（available 接口 thinkingConfig.options 最后一项的
	// openclawLevel）；空表示上游未提供，即无法指定思考等级，转发时移除 reasoning_effort。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}