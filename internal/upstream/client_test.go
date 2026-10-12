package upstream

import (
	"encoding/json"
	"testing"
)

// TestLastOpenclawLevel 覆盖 available 接口 thinkingConfig 的三种形态：多个等级、null、空 options。
func TestLastOpenclawLevel(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "多个等级取最后一个",
			raw:  `{"options":[{"level":"off","openclawLevel":"off"},{"level":"high","openclawLevel":"high"},{"level":"max","openclawLevel":"xhigh"}],"defaultLevel":"high"}`,
			want: "xhigh",
		},
		{name: "无思考配置", raw: `null`, want: ""},
		{name: "options 为空", raw: `{"options":[]}`, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tc *modelThinkingConfig
			if err := json.Unmarshal([]byte(c.raw), &tc); err != nil {
				t.Fatalf("解析 thinkingConfig 失败: %v", err)
			}
			if got := tc.lastOpenclawLevel(); got != c.want {
				t.Fatalf("lastOpenclawLevel() = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestPrepareChatBodyReasoningEffort 覆盖 reasoning_effort 的替换与移除。
func TestPrepareChatBodyReasoningEffort(t *testing.T) {
	const absent = "<absent>"
	cases := []struct {
		name   string
		body   string
		effort string
		want   string
	}{
		{
			name:   "能指定等级时替换客户端值",
			body:   `{"model":"deepseek-flash","reasoning_effort":"medium"}`,
			effort: "xhigh",
			want:   "xhigh",
		},
		{
			name:   "无法指定等级时移除字段",
			body:   `{"model":"kimi-k2.6","reasoning_effort":"high"}`,
			effort: "",
			want:   absent,
		},
		{
			name:   "客户端未传时不添加",
			body:   `{"model":"deepseek-flash"}`,
			effort: "xhigh",
			want:   absent,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]any
			if err := json.Unmarshal(prepareChatBody([]byte(c.body), c.effort), &got); err != nil {
				t.Fatalf("解析转发体失败: %v", err)
			}
			v, ok := got["reasoning_effort"]
			if c.want == absent {
				if ok {
					t.Fatalf("reasoning_effort 应被移除，实际为 %v", v)
				}
				return
			}
			if !ok || v != c.want {
				t.Fatalf("reasoning_effort = %v，期望 %q", v, c.want)
			}
		})
	}
}
