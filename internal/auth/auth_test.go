package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dalemei/inference-gateway/internal/config"
)

// sha256("team-a-secret") 的前置计算值，用于锁定 HashKey 实现不被无意改动
const knownHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" // sha256("test")

func TestHashKey(t *testing.T) {
	if got := HashKey("test"); got != knownHash {
		t.Fatalf("HashKey(\"test\") = %s, 期望 %s", got, knownHash)
	}
}

func TestAuthAcceptsBothPlainAndHashedKey(t *testing.T) {
	cfg := config.AuthConfig{
		Enabled: true,
		Keys: []config.KeyConfig{
			{Name: "by-plain", Key: "test"},
			{Name: "by-hash", KeyHash: knownHash},
		},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	// 明文与摘要两种写法都应能验过同一个 Key
	for _, name := range []string{"by-plain", "by-hash"} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer test")
		k, reason := a.Authenticate(req)
		if k == nil {
			t.Fatalf("用 %s 配置时，正确的 Key 被拒: %s", name, reason)
		}
	}
}

func TestAuthExtraction(t *testing.T) {
	a, _ := New(config.AuthConfig{Keys: []config.KeyConfig{{Name: "k", Key: "test"}}})

	cases := []struct {
		name, header, value, want string
	}{
		{"标准 Bearer", "Authorization", "Bearer test", ""},
		{"Bearer 小写 scheme", "Authorization", "bearer test", ""},
		{"X-API-Key", "X-API-Key", "test", ""},
		{"错误 Key", "Authorization", "Bearer wrong", ReasonInvalid},
		{"无 Key", "", "", ReasonMissing},
		{"Authorization 非 Bearer 前缀", "Authorization", "test", ""},
	}

	for _, c := range cases {
		req := httptest.NewRequest("POST", "/v1/x", nil)
		if c.header != "" {
			req.Header.Set(c.header, c.value)
		}
		k, reason := a.Authenticate(req)
		if c.want == "" {
			if k == nil {
				t.Errorf("%s: 应当通过，实际被拒(%s)", c.name, reason)
			}
		} else if reason != c.want {
			t.Errorf("%s: 期望原因 %q，实际 %q", c.name, c.want, reason)
		}
	}
}

func TestCustomHeader(t *testing.T) {
	a, _ := New(config.AuthConfig{
		Header: "X-Auth-Token",
		Keys:   []config.KeyConfig{{Name: "k", Key: "test"}},
	})
	// 配了自定义头后，Authorization 不应再生效
	req := httptest.NewRequest("POST", "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer test")
	if k, _ := a.Authenticate(req); k != nil {
		t.Error("配置了自定义 header 后，Authorization 仍被接受（应当只认 X-Auth-Token）")
	}

	req2 := httptest.NewRequest("POST", "/v1/x", nil)
	req2.Header.Set("X-Auth-Token", "test")
	if k, reason := a.Authenticate(req2); k == nil {
		t.Errorf("X-Auth-Token 应当被接受，实际被拒: %s", reason)
	}
}

// TestLimiterBurstThenSteadyRate 用假时钟做确定性验证，不依赖 sleep。
func TestLimiterBurstThenSteadyRate(t *testing.T) {
	l := NewLimiter(2, 2) // 2 个/秒，桶容量 2
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 突发：桶里有 2 个令牌，前两次立即放行
	for i := 0; i < 2; i++ {
		ok, _ := l.Allow(now)
		if !ok {
			t.Fatalf("第 %d 次（突发额度内）应放行", i+1)
		}
	}
	// 第三次：桶已空
	if ok, retry := l.Allow(now); ok {
		t.Fatal("桶空后仍放行")
	} else if retry <= 0 {
		t.Errorf("被拒时应给出正的 Retry-After，实际 %v", retry)
	} else if retry > time.Second {
		t.Errorf("rate=2/s 时补一个令牌约需 0.5s，实际建议 %v（计算偏差过大）", retry)
	}

	// 推进 0.5s → 恰好补满 1 个令牌
	if ok, _ := l.Allow(now.Add(500 * time.Millisecond)); !ok {
		t.Error("推进 0.5s 后应能放行 1 个（rate=2/s）")
	}
	if ok, _ := l.Allow(now.Add(500 * time.Millisecond)); ok {
		t.Error("同一时刻不应放行第二个（令牌已取空）")
	}

	// 长时间不请求 → 令牌回补但不超过桶容量（否则会攒出无限突发）
	for i := 0; i < 10; i++ {
		l.Allow(now.Add(time.Duration(100+i) * time.Second))
	}
	allowed := 0
	for i := 0; i < 20; i++ {
		if ok, _ := l.Allow(now.Add(200 * time.Second)); ok {
			allowed++
		}
	}
	if allowed > 2 {
		t.Errorf("长时间空闲后突发额度应被桶容量 2 封顶，实际放行 %d 次", allowed)
	}
}

func TestZeroRateLimitMeansNoLimit(t *testing.T) {
	if got := NewLimiter(0, 0); got != nil {
		t.Error("rate=0 应返回 nil（表示不限流）")
	}
	k := &APIKey{Name: "x"}
	now := time.Now()
	for i := 0; i < 100; i++ {
		if ok, _ := k.Allow(now); !ok {
			t.Fatalf("未配置限流的 Key 第 %d 次被拒", i+1)
		}
	}
}

func TestMiddlewareStatusCodes(t *testing.T) {
	a, _ := New(config.AuthConfig{Keys: []config.KeyConfig{
		{Name: "fast", Key: "test", RateLimit: 0},
		{Name: "slow", Key: "slow-key", RateLimit: 1, Burst: 1},
	}})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	h := a.Middleware(next)

	// 1) 无 Key → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无 Key 应返回 401，实际 %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 应带 WWW-Authenticate 头（RFC 7235）")
	}

	// 2) 正确 Key → 200
	req := httptest.NewRequest("POST", "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("正确 Key 应放行，实际 %d", rec.Code)
	}

	// 3) 限流 Key：第一个 200，第二个 429 且带 Retry-After
	req1 := httptest.NewRequest("POST", "/v1/x", nil)
	req1.Header.Set("Authorization", "Bearer slow-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req1)
	if rec.Code != http.StatusOK {
		t.Errorf("限流 Key 首个请求应放行，实际 %d", rec.Code)
	}

	req2 := httptest.NewRequest("POST", "/v1/x", nil)
	req2.Header.Set("Authorization", "Bearer slow-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req2)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("超出突发额度应返回 429，实际 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 必须带 Retry-After，否则客户端只能盲重试，会把限流放大成重试风暴")
	}
}

// flusherSpy 记录 Flush 是否被调用，用于验证中间件没有吃掉 http.Flusher 接口。
type flusherSpy struct {
	header  http.Header
	body    []byte
	status  int
	flushes int
}

func (f *flusherSpy) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}
func (f *flusherSpy) Write(b []byte) (int, error) { f.body = append(f.body, b...); return len(b), nil }
func (f *flusherSpy) WriteHeader(code int)        { f.status = code }
func (f *flusherSpy) Flush()                      { f.flushes++ }

func TestStatusRecorderKeepsFlusher(t *testing.T) {
	spy := &flusherSpy{}
	rec := &statusRecorder{ResponseWriter: spy, status: http.StatusOK}

	// SSE 依赖 http.Flusher：若包装后该接口丢失，proxy 会降级成非流式，流式能力静默失效
	if _, ok := interface{}(rec).(http.Flusher); !ok {
		t.Fatal("statusRecorder 未实现 http.Flusher —— 开启鉴权后 SSE 会静默降级为非流式")
	}
	rec.WriteHeader(http.StatusOK)
	rec.Write([]byte("data: x\n\n"))
	rec.Flush()
	if spy.flushes != 1 {
		t.Errorf("Flush 未透传到下层，实际调用 %d 次", spy.flushes)
	}
	if rec.status != http.StatusOK {
		t.Errorf("状态码记录错误: %d", rec.status)
	}
}

// TestResponseControllerPenetratesRecorder 验证最隐蔽的一处回归风险：
// proxy 用 http.NewResponseController(w).SetWriteDeadline(time.Time{}) 清除
// Server.WriteTimeout 打在连接上的写 deadline（否则长 SSE 会被硬掐断）。
// ResponseController 只认底层真实连接，靠 ResponseWriter.Unwrap() 逐层穿透；
// 一旦中间件包装后没有实现 Unwrap，鉴权开启就会让这个保护失效。
func TestResponseControllerPenetratesRecorder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		rc := http.NewResponseController(wrapped)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			t.Errorf("包装后 SetWriteDeadline 失败（%v）—— 说明 Unwrap 未生效，"+
				"开启鉴权后长 SSE 会被 Server.WriteTimeout 硬掐断", err)
		}
		wrapped.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
}

// noUnwrapRecorder 与 statusRecorder 相同，唯独不实现 Unwrap——作为对照组。
type noUnwrapRecorder struct{ http.ResponseWriter }

// TestResponseControllerFailsWithoutUnwrap 对照组：
// 证明上一个测试不是空转（若去掉 Unwrap 也照样通过，说明那条断言毫无价值）。
// 只有「有 Unwrap 通过、无 Unwrap 失败」这一对结果同时成立，才能确认穿透机制真实存在。
func TestResponseControllerFailsWithoutUnwrap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(&noUnwrapRecorder{ResponseWriter: w})
		if err := rc.SetWriteDeadline(time.Time{}); err == nil {
			t.Error("对照组失效：未实现 Unwrap 的包装器也能设置写 deadline，" +
				"说明穿透与 Unwrap 无关，上一个测试是空断言")
		}
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
}
