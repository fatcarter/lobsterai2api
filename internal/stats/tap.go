package stats

import (
	"io"
	"strings"
)

// TapReader 在透传 SSE 流的同时观察 usage 帧；每命中一次 usage 块，回调 OnUsage。
// 用于流式 chat 响应：客户端实时拿到增量，统计页实时累计 prompt/completion tokens。
type TapReader struct {
	Source  io.ReadCloser
	OnUsage func(promptTokens, completionTokens int64)
	// pending 用于缓存半行数据；每次 Read 解析一行（命中即回调），
	// 未命中或跨行部分留到下一次 Read 继续。
	pending string
}

const usageKey = `"usage":`

// Read 实现 io.Reader：把 Source 的内容拷贝到 p，同时解析完整行触发回调。
func (t *TapReader) Read(p []byte) (int, error) {
	if t == nil || t.Source == nil {
		return 0, io.EOF
	}
	n, err := t.Source.Read(p)
	if n > 0 {
		t.feed(string(p[:n]))
	}
	return n, err
}

// Close 关闭底层读取器。
func (t *TapReader) Close() error {
	if t == nil || t.Source == nil {
		return nil
	}
	return t.Source.Close()
}

// feed 把新读取到的文本追加到 pending，按行切分并回调命中 usage 的行。
func (t *TapReader) feed(chunk string) {
	if t.OnUsage == nil {
		t.pending += chunk
		// 即使没有回调，也要把 pending 控制在不会无限增长的范围内。
		if len(t.pending) > 1<<16 {
			t.pending = t.pending[len(t.pending)-1<<16:]
		}
		return
	}
	t.pending += chunk
	for {
		i := strings.IndexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		line := t.pending[:i]
		t.pending = t.pending[i+1:]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") && strings.Contains(trimmed, usageKey) {
			if prompt, completion, ok := parseUsageLine(trimmed); ok {
				t.OnUsage(prompt, completion)
			}
		}
	}
	if len(t.pending) > 1<<16 {
		t.pending = t.pending[len(t.pending)-1<<16:]
	}
}

// parseUsageLine 从 "data:{...usage:{...}}" 一行中提取 prompt_tokens / completion_tokens。
// 仅做轻量字符串扫描（不解析完整 JSON），避免每次都做全量 Unmarshal。
// 命中条件：行内同时含 `"prompt_tokens"` 与 `"completion_tokens"` 两个 key。
func parseUsageLine(line string) (prompt, completion int64, ok bool) {
	if !strings.Contains(line, `"prompt_tokens"`) || !strings.Contains(line, `"completion_tokens"`) {
		return 0, 0, false
	}
	prompt = findIntValue(line, `"prompt_tokens":`)
	if prompt < 0 {
		prompt = 0
	}
	completion = findIntValue(line, `"completion_tokens":`)
	if completion < 0 {
		completion = 0
	}
	return prompt, completion, true
}

// findIntValue 在 line 中找 key 后面的第一个整数（含 0）；找不到返回 -1。
func findIntValue(line, key string) int64 {
	idx := strings.Index(line, key)
	if idx < 0 {
		return -1
	}
	rest := line[idx+len(key):]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return -1
	}
	neg := false
	if rest[0] == '-' {
		neg = true
		rest = rest[1:]
	} else if rest[0] == '+' {
		rest = rest[1:]
	}
	n := int64(0)
	any := false
	for len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
		n = n*10 + int64(rest[0]-'0')
		rest = rest[1:]
		any = true
	}
	if !any {
		return -1
	}
	if neg {
		n = -n
	}
	return n
}

// 编译期断言：TapReader 同时实现 io.Reader 与 io.Closer。
var _ io.ReadCloser = (*TapReader)(nil)