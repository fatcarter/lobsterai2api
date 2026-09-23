package stats

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errEOF = errors.New("eof")

func TestRecorderRecordTokens(t *testing.T) {
	r := Load("")
	r.RecordTokens("m1", 10, 20)
	r.RecordTokens("m1", 5, 7)
	r.AddFailure("m2")
	snap := r.Report()
	if snap.Total != 3 {
		t.Fatalf("total=%d, want 3", snap.Total)
	}
	if snap.Success != 2 {
		t.Fatalf("success=%d, want 2", snap.Success)
	}
	if snap.Failed != 1 {
		t.Fatalf("failed=%d, want 1", snap.Failed)
	}
	if snap.PromptTokens != 15 {
		t.Fatalf("prompt=%d, want 15", snap.PromptTokens)
	}
	if snap.CompletionTokens != 27 {
		t.Fatalf("completion=%d, want 27", snap.CompletionTokens)
	}
	if snap.TotalTokens != 42 {
		t.Fatalf("total_tokens=%d, want 42", snap.TotalTokens)
	}
	if snap.SuccessRate < 0.66 || snap.SuccessRate > 0.67 {
		t.Fatalf("success_rate=%v", snap.SuccessRate)
	}
	if len(snap.ByModel) != 2 {
		t.Fatalf("by_model 应有 2 行，实际 %d", len(snap.ByModel))
	}
}

func TestRecorderPersists(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "stats.json")
	r := Load(fp)
	r.RecordTokens("foo", 3, 4)
	r.AddFailure("foo")
	r.Flush()

	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("Flush 未写盘: %v", err)
	}
	raw, _ := os.ReadFile(fp)
	var ff fileFormat
	if err := json.Unmarshal(raw, &ff); err != nil {
		t.Fatalf("落盘 JSON 解析失败: %v", err)
	}
	if ff.Total != 2 || ff.Success != 1 || ff.Failed != 1 {
		t.Fatalf("落盘数据不正确: %+v", ff)
	}

	// 重启后 Load 应还原累计。
	r2 := Load(fp)
	snap := r2.Report()
	if snap.Total != 2 || snap.Success != 1 || snap.Failed != 1 {
		t.Fatalf("重载数据不一致: %+v", snap)
	}
	if r2.data.StartedAt == 0 || r2.data.StartedAt > time.Now().Unix()+1 {
		t.Fatalf("StartedAt 应被首次写入：%d", r2.data.StartedAt)
	}
}

func TestRecorderFlushSkipsWhenClean(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "stats.json")
	r := Load(fp)
	// 未写任何数据，Flush 不应创建文件。
	r.Flush()
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("未写数据不应创建文件，实际 %v", err)
	}
}

func TestParseUsageLine(t *testing.T) {
	cases := []struct {
		in      string
		prompt  int64
		compl   int64
		wantHit bool
	}{
		{`data:{"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`, 12, 34, true},
		{`data:{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, 0, 0, true},
		{`data:{"choices":[]}`, 0, 0, false},
		{`data:{"usage":{"prompt_tokens":1200,"completion_tokens":0,"total_tokens":1200}}`, 1200, 0, true},
	}
	for _, tc := range cases {
		p, c, ok := parseUsageLine(tc.in)
		if ok != tc.wantHit {
			t.Errorf("命中=%v, want %v (line=%q)", ok, tc.wantHit, tc.in)
			continue
		}
		if !ok {
			continue
		}
		if p != tc.prompt || c != tc.compl {
			t.Errorf("(%q) prompt=%d completion=%d, want %d/%d", tc.in, p, c, tc.prompt, tc.compl)
		}
	}
}

func TestTapReaderFeedsUsage(t *testing.T) {
	body := "data:{\"choices\":[]}\ndata:{\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33}}\ndata:[DONE]\n\n"
	src := newFakeSource(body)
	var gotPrompt, gotCompl int64
	var calls int
	tap := &TapReader{Source: src, OnUsage: func(p, c int64) {
		gotPrompt, gotCompl = p, c
		calls++
	}}
	buf := make([]byte, 64)
	for {
		_, err := tap.Read(buf)
		if err != nil {
			break
		}
	}
	if calls != 1 {
		t.Fatalf("usage 应回调 1 次，实际 %d", calls)
	}
	if gotPrompt != 11 || gotCompl != 22 {
		t.Fatalf("prompt=%d completion=%d, want 11/22", gotPrompt, gotCompl)
	}
}

// fakeSource 模拟 io.ReadCloser 提供固定 SSE 文本。
type fakeSource struct {
	pos  int
	data []byte
	done bool
}

func newFakeSource(s string) *fakeSource { return &fakeSource{data: []byte(s)} }
func (f *fakeSource) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		if f.done {
			return 0, errEOF
		}
		f.done = true
		return 0, errEOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}
func (f *fakeSource) Close() error { return nil }