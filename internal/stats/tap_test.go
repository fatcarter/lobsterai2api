package stats

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestTapReaderBasic(t *testing.T) {
	body := "data:{\"choices\":[]}\ndata:{\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33}}\ndata:[DONE]\n\n"
	src := io.NopCloser(strings.NewReader(body))
	var gotPrompt, gotCompl int64
	var calls int
	tap := &TapReader{Source: src, OnUsage: func(p, c int64) {
		gotPrompt, gotCompl = p, c
		calls++
	}}
	out, err := io.ReadAll(tap)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Contains(out, []byte("data:[DONE]")) {
		t.Fatalf("转发内容缺失: %q", out)
	}
	if calls != 1 {
		t.Fatalf("usage 应回调 1 次，实际 %d (out=%q)", calls, out)
	}
	if gotPrompt != 11 || gotCompl != 22 {
		t.Fatalf("prompt=%d completion=%d, want 11/22", gotPrompt, gotCompl)
	}
}

func TestTapReaderBufferingAcrossReads(t *testing.T) {
	body := "data:{\"usage\":{\"prompt_tokens\":99,\"completion_tokens\":1,\"total_tokens\":100}}\ndata:[DONE]\n\n"
	src := io.NopCloser(strings.NewReader(body))
	var prompt, compl int64
	tap := &TapReader{Source: src, OnUsage: func(p, c int64) { prompt, compl = p, c }}
	// 用 5 字节的小 buffer，强制按多次 Read 切分。
	buf := make([]byte, 5)
	for {
		_, err := tap.Read(buf)
		if err != nil {
			break
		}
	}
	if prompt != 99 || compl != 1 {
		t.Fatalf("prompt=%d completion=%d, want 99/1", prompt, compl)
	}
}