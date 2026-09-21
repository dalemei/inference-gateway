// Package usage 从后端响应中提取 token 用量，用于成本观测与按 Key 分账。
//
// 为什么要单独一个包：
// 用量解析涉及「非流式整包 JSON」「流式 SSE 逐行扫描」「请求体注入」三种互不相同的
// 处理逻辑，塞进 proxy 会让转发主流程被计量细节淹没。抽出来之后 proxy 只留三处调用点。
package usage

import (
	"bytes"
	"encoding/json"
)

// Usage 一次请求的 token 用量。
//
// CachedTokens 取自 usage.prompt_tokens_details.cached_tokens（前缀缓存命中的 token 数）。
// 它衡量的是「这次请求里有多少输入 token 没真正走 GPU 计算」，直接决定真实成本：
// 命中率越高，同样的 prompt_tokens 花的算力越少。
//
// ⚠️ 该字段可能整个缺失（vLLM 需显式加 --enable-prompt-tokens-details 才会填充，
// Ollama 默认返回）。缺失时这里填 0，与「命中 0 个」在数值上无法区分，
// 所以缓存命中率必须结合 usage_missing 指标一起看，不能单看数字下降就以为成本降了。
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64

	// HasCached 后端是否**上报**了 cached_tokens 字段（与值为多少无关）。
	//
	// 必须单独区分：cached_tokens=0 有两种完全不同的含义——
	// 「这次真没命中缓存」和「后端压根没统计这个字段」（vLLM 未开
	// --enable-prompt-tokens-details 时就没有）。只看数值两者都是 0，
	// 会让缓存命中率在没有该能力时显示成「命中率 0%」，
	// 结论从「未观测」被误读成「缓存完全没生效」——排查方向直接跑偏。
	HasCached bool
}

// Total 输入+输出总 token（不含缓存拆分，缓存是输入的一部分不是额外量）。
func (u Usage) Total() int64 { return u.PromptTokens + u.CompletionTokens }

// rawUsage 后端 usage 字段的原始结构。
// 全部用指针：JSON 里「字段缺失」和「字段为 0」必须可区分，
// 用值类型会让两者都变成 0，导致「后端没返回」被误记成「返回了 0 个 token」。
type rawUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// envelope 只需 usage 字段，其余（choices 等）一律忽略。
type envelope struct {
	Usage *rawUsage `json:"usage"`
}

var (
	litUsage = []byte(`"usage"`)
	litData  = []byte("data:")
	litDone  = []byte("[DONE]")
)

// ParseResponse 从一个完整 JSON 响应体中提取 usage。
// 第二个返回值表示「后端是否真的返回了 usage」——false 时 Usage 为零值，不应计入。
func ParseResponse(body []byte) (Usage, bool) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Usage{}, false
	}
	return fromRaw(env.Usage), true
}

// ParseStreamChunk 从 SSE 的 data 载荷（"data: " 之后的 JSON）中提取 usage。
//
// 先做一次字节包含检查再决定是否 Unmarshal：流式响应里绝大多数 chunk 是纯文本增量，
// 无条件对每个 chunk 做 JSON 解析是白白烧 CPU（长流可达数千 chunk）。
// 仅当载荷里出现 "usage" 字面量时才真正解析——正常使用下只有末尾那一两个 chunk 会命中。
func ParseStreamChunk(payload []byte) (Usage, bool) {
	if !bytes.Contains(payload, litUsage) {
		return Usage{}, false
	}
	return ParseResponse(payload)
}

func fromRaw(r *rawUsage) Usage {
	var u Usage
	if r.PromptTokens != nil {
		u.PromptTokens = *r.PromptTokens
	}
	if r.CompletionTokens != nil {
		u.CompletionTokens = *r.CompletionTokens
	}
	if r.PromptTokensDetails != nil && r.PromptTokensDetails.CachedTokens != nil {
		u.CachedTokens = *r.PromptTokensDetails.CachedTokens
		u.HasCached = true
	}
	return u
}

// maxPendingLine SSE 未完成行的缓存上限。
// 正常情况下一次 Feed 后残留的只是半行（几十字节）；若后端吐出超长单行或压根不发换行，
// 不设上限会让这块缓冲无界增长。超过上限直接丢弃，代价仅是这条流统计不到 usage。
const maxPendingLine = 1 << 16 // 64KB

// StreamScanner 在流式转发过程中逐行扫描 SSE，抓取末尾的 usage chunk。
//
// 不能直接按 TCP 读块解析：一个 SSE 行可能被任意一次 Read 切成两半。
// 所以这里只保留「未完成的尾部行」，与下一次读到的内容拼接后再切分。
type StreamScanner struct {
	pending []byte
	usage   Usage
	found   bool
}

// Feed 喂入本次从后端读到的原始字节（未做任何改写）。
func (s *StreamScanner) Feed(chunk []byte) {
	s.pending = append(s.pending, chunk...)

	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		line := s.pending[:i]
		s.pending = s.pending[i+1:]
		s.consumeLine(bytes.TrimRight(line, "\r"))
	}

	if len(s.pending) > maxPendingLine {
		s.pending = s.pending[:0]
	}
}

// Usage 返回扫描到的用量。流尚未结束或后端没给 usage 时返回 false。
func (s *StreamScanner) Usage() (Usage, bool) {
	if !s.found {
		return Usage{}, false
	}
	return s.usage, true
}

func (s *StreamScanner) consumeLine(line []byte) {
	if !bytes.HasPrefix(line, litData) {
		return
	}
	payload := bytes.TrimSpace(line[len(litData):])
	if len(payload) == 0 || bytes.Equal(payload, litDone) {
		return
	}
	if u, ok := ParseStreamChunk(payload); ok {
		s.usage = u
		s.found = true
	}
}

// CappedBuffer 带上限的累积缓冲。
//
// 用于非流式响应：转发的同时旁路累积一份用于解析 usage。
// 超过上限后进入丢弃模式（不再保留内容），但 Write 仍吞掉全部字节并返回 len(p)，
// 保证 TeeReader 的语义不被破坏——否则转发会静默少写字节。
type CappedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

// NewCappedBuffer 创建上限为 limit 字节的累积缓冲。
func NewCappedBuffer(limit int) *CappedBuffer {
	return &CappedBuffer{limit: limit}
}

// Write 实现 io.Writer。
func (c *CappedBuffer) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil // 已超限：丢弃内容但必须照常「吃掉」全部字节
	}
	if c.buf.Len()+len(p) > c.limit {
		c.over = true
		c.buf.Reset()
		return len(p), nil
	}
	return c.buf.Write(p)
}

// Bytes 返回累积的内容。超限时返回空——调用方应先看 Truncated 判断原因，
// 否则「内容为空」和「被截断」无法区分，会把超限误记成「后端没返回 usage」。
func (c *CappedBuffer) Bytes() []byte {
	if c.over {
		return nil
	}
	return c.buf.Bytes()
}

// Truncated 是否因超过上限而丢弃了内容。
func (c *CappedBuffer) Truncated() bool { return c.over }

// EnsureStreamOptions 给流式请求补上 stream_options.include_usage。
//
// 为什么必须补：OpenAI 兼容协议下，**流式响应默认不返回 usage**——
// 实测 Ollama 23 个 chunk 里 0 个含 usage；只有显式传 stream_options.include_usage=true，
// 后端才会在末尾追加一个带 usage 的 chunk。网关若不主动注入，流式请求的成本就永远看不见，
// 而生产环境里流式恰恰是主要流量。
//
// 客户端已经自己指定 stream_options 时原样返回：那是调用方的显式意图，不能被覆盖。
// 任何解析/序列化失败也一律返回原始 body——计量功能绝不能改变转发行为。
func EnsureStreamOptions(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, exists := m["stream_options"]; exists {
		return body
	}

	// 用 RawMessage 保留其它字段的原始字节：若走 map[string]interface{} 再 Marshal，
	// 浮点数精度、数字格式（1 vs 1.0）、键顺序都可能被改写，
	// 对某些做请求体签名/校验的上游是致命的。
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)

	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
