package usage

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseResponse(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantOK     bool
		wantPrompt int64
		wantComp   int64
		wantCached int64

		wantHasCached bool
	}{
		{
			name:       "完整 usage 含缓存明细",
			body:       `{"choices":[{"text":"hi"}],"usage":{"prompt_tokens":30,"completion_tokens":6,"total_tokens":36,"prompt_tokens_details":{"cached_tokens":24}}}`,
			wantOK:     true,
			wantPrompt: 30, wantComp: 6, wantCached: 24,
			wantHasCached: true,
		},
		{
			name:       "有 usage 但无缓存明细",
			body:       `{"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			wantOK:     true,
			wantPrompt: 10, wantComp: 2, wantCached: 0,
		},
		{
			name:       "字段为 0 与字段缺失可区分",
			body:       `{"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			wantOK:     true, // 后端确实返回了 usage，只是都是 0
			wantPrompt: 0, wantComp: 0,
		},
		{
			name:   "完全没有 usage 字段",
			body:   `{"choices":[{"text":"hi"}]}`,
			wantOK: false,
		},
		{
			// cached_tokens=0 但字段存在：必须与「字段缺失」区分开，
			// 否则缓存命中率会把「真没命中」和「后端没这能力」混为一谈。
			name:       "上报了 cached 但值为 0",
			body:       `{"usage":{"prompt_tokens":20,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}}`,
			wantOK:     true,
			wantPrompt: 20, wantComp: 1, wantCached: 0,
			wantHasCached: true,
		},
		{
			name:       "未上报 cached 字段",
			body:       `{"usage":{"prompt_tokens":20,"completion_tokens":1}}`,
			wantOK:     true,
			wantPrompt: 20, wantComp: 1, wantCached: 0,
			wantHasCached: false,
		},
		{
			name:   "非 JSON",
			body:   `not json at all`,
			wantOK: false,
		},
		{
			name:   "usage 为 null",
			body:   `{"usage":null}`,
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, ok := ParseResponse([]byte(c.body))
			if ok != c.wantOK {
				t.Fatalf("ok = %v, 期望 %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if u.PromptTokens != c.wantPrompt || u.CompletionTokens != c.wantComp || u.CachedTokens != c.wantCached {
				t.Errorf("得到 %+v，期望 prompt=%d completion=%d cached=%d",
					u, c.wantPrompt, c.wantComp, c.wantCached)
			}
			if u.HasCached != c.wantHasCached {
				t.Errorf("HasCached = %v，期望 %v（区分「上报 0」与「未上报」）", u.HasCached, c.wantHasCached)
			}
		})
	}
}

// TestParseStreamChunkSkipsWithoutLiteralUsage 验证廉价预检确实生效：
// 纯文本 chunk（占流式响应的绝大多数）不应进入 JSON 解析。
// 这里用一个「含 usage 语义但不含 "usage" 字面量」的载荷做反向验证。
func TestParseStreamChunkSkipsWithoutLiteralUsage(t *testing.T) {
	payload := []byte(`{"choices":[{"delta":{"content":"今天讲讲怎么用"}}]}`)
	if _, ok := ParseStreamChunk(payload); ok {
		t.Error("不含 usage 字面量的 chunk 不应解析出用量")
	}

	withUsage := []byte(`{"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":24}}}`)
	u, ok := ParseStreamChunk(withUsage)
	if !ok {
		t.Fatal("末尾 usage chunk 应当被解析出来")
	}
	if u.PromptTokens != 30 || u.CompletionTokens != 6 || u.CachedTokens != 24 {
		t.Errorf("解析结果错误: %+v", u)
	}
}

// TestStreamScannerSplitAcrossFeeds 是这套扫描器最关键的用例：
// SSE 的一行可能被任意一次 TCP Read 切成两半，若不拼接残留就会永远扫不到 usage。
func TestStreamScannerSplitAcrossFeeds(t *testing.T) {
	full := `data: {"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":24}}}` + "\n\n"

	s := &StreamScanner{}
	// 从 "usage" 这个词的正中间切开——最刁钻的切分点
	cut := strings.Index(full, "usage") + 2
	s.Feed([]byte(full[:cut]))
	s.Feed([]byte(full[cut:]))

	u, ok := s.Usage()
	if !ok {
		t.Fatal("行被切成两半后扫描失败——说明没有拼接残留数据")
	}
	if u.PromptTokens != 30 || u.CachedTokens != 24 {
		t.Errorf("结果错误: %+v", u)
	}
}

func TestStreamScannerMultipleLinesOneFeed(t *testing.T) {
	s := &StreamScanner{}
	s.Feed([]byte(
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}\n\n" +
			"data: [DONE]\n\n"))

	// [DONE] 之后流结束，最后一条 usage 应被保留
	u, ok := s.Usage()
	if !ok {
		t.Fatal("多行的单次 Feed 未扫到 usage")
	}
	if u.PromptTokens != 9 || u.CompletionTokens != 3 {
		t.Errorf("结果错误: %+v", u)
	}
}

func TestStreamScannerIgnoresDoneAndNonData(t *testing.T) {
	s := &StreamScanner{}
	s.Feed([]byte("event: ping\n\ndata: [DONE]\n\n"))
	if _, ok := s.Usage(); ok {
		t.Error("只有 [DONE] 与非 data 行时不应产生用量")
	}
}

// TestStreamScannerPendingCap 验证畸形流（后端一直不发换行）不会让缓冲无界增长。
func TestStreamScannerPendingCap(t *testing.T) {
	s := &StreamScanner{}
	big := strings.Repeat("x", maxPendingLine+1024)
	s.Feed([]byte(big))
	if len(s.pending) > maxPendingLine {
		t.Errorf("残留缓冲未受上限约束: %d 字节", len(s.pending))
	}
}

func TestStreamScannerCRLF(t *testing.T) {
	s := &StreamScanner{}
	s.Feed([]byte("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\r\n\r\n"))
	u, ok := s.Usage()
	if !ok {
		t.Fatal("CRLF 换行未被正确处理")
	}
	if u.PromptTokens != 5 {
		t.Errorf("结果错误: %+v", u)
	}
}

func TestCappedBuffer(t *testing.T) {
	cb := NewCappedBuffer(10)
	n, err := cb.Write([]byte("12345"))
	if n != 5 || err != nil {
		t.Fatalf("Write 返回 %d, %v；期望 5, nil", n, err)
	}
	n, _ = cb.Write([]byte("67890")) // 刚好到上限
	if n != 5 {
		t.Errorf("第二次 Write 返回 %d，期望 5", n)
	}
	if cb.Truncated() {
		t.Error("刚好等于上限时不应判定为截断")
	}

	// 超限后：内容丢弃，但仍必须「吃掉」全部字节，否则 TeeReader 会少写数据
	n, _ = cb.Write([]byte("ABCDEF"))
	if n != 6 {
		t.Errorf("超限后 Write 返回 %d，期望 6（必须吞掉全部字节）", n)
	}
	if !cb.Truncated() {
		t.Error("超限后应判定为截断")
	}
	if cb.Bytes() != nil {
		t.Errorf("截断后 Bytes 应为 nil，实际 %q", cb.Bytes())
	}
}

func TestEnsureStreamOptions(t *testing.T) {
	t.Run("缺失时注入", func(t *testing.T) {
		in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
		out := EnsureStreamOptions(in)
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("注入后不是合法 JSON: %v", err)
		}
		if string(m["stream_options"]) != `{"include_usage":true}` {
			t.Errorf("stream_options = %s", m["stream_options"])
		}
		// 原有字段必须原样保留
		if string(m["model"]) != `"m"` {
			t.Errorf("model 被改写: %s", m["model"])
		}
		if !json.Valid(out) {
			t.Error("输出非法 JSON")
		}
	})

	t.Run("已存在时不覆盖", func(t *testing.T) {
		in := []byte(`{"stream":true,"stream_options":{"include_usage":false}}`)
		out := EnsureStreamOptions(in)
		if string(out) != string(in) {
			t.Errorf("客户端显式指定了 stream_options 却仍被改写: %s", out)
		}
	})

	// 数字格式被改写会让某些做请求体校验的上游拒绝请求。
	// 用 RawMessage 就是为了让 0.7 保持 0.7、1 保持 1。
	t.Run("不改写数字格式", func(t *testing.T) {
		in := []byte(`{"temperature":0.7,"top_p":1,"max_tokens":1.0}`)
		out := EnsureStreamOptions(in)
		if !strings.Contains(string(out), "0.7") {
			t.Errorf("temperature 精度被破坏: %s", out)
		}
		if !strings.Contains(string(out), `"top_p":1`) {
			t.Errorf("整数被改写成浮点: %s", out)
		}
	})

	t.Run("非法 JSON 原样返回", func(t *testing.T) {
		in := []byte(`not json`)
		if out := EnsureStreamOptions(in); string(out) != string(in) {
			t.Errorf("非法输入应原样返回，实际 %s", out)
		}
	})
}
