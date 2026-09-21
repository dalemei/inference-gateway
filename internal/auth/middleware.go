package auth

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/dalemei/inference-gateway/internal/metrics"
)

// contextKey API Key 名称在 request context 中的键类型（不可导出，避免外部污染）。
type contextKey struct{}

// WithKeyName 把已通过鉴权的 Key 名写入 context，供下游（proxy）做 token 用量归因。
//
// 为什么走 context 而不是塞一个自定义请求头：
// proxy 的 copyHeaders 会把请求头原样转发给后端，等于把内部标识泄露给上游推理服务；
// context 只在本进程内传递，且随请求生命周期自动回收。
func WithKeyName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, contextKey{}, name)
}

// KeyNameFrom 取出 context 中的 Key 名。未启用鉴权时返回空串（调用方按匿名处理）。
func KeyNameFrom(ctx context.Context) string {
	name, _ := ctx.Value(contextKey{}).(string)
	return name
}

// Middleware 返回「鉴权 → 限流 → 业务」的 HTTP 中间件。
//
// 刻意做成中间件而不是塞进 proxy.ServeHTTP：
// 鉴权是入口治理，转发是后端调度，两者关注点正交。做成独立一层后，
// 关闭鉴权时网关退化为纯转发（零行为差异），也便于后续插入更多入口策略（配额、审计）。
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, reason := a.Authenticate(r)
		if key == nil {
			metrics.RecordAuthFailure(reason)
			log.Printf("[auth] 拒绝 %s %s: %s (UA=%q)", r.Method, r.URL.Path, reason, r.UserAgent())
			writeJSONError(w, http.StatusUnauthorized, map[string]string{
				"error": "unauthorized: missing or invalid API key",
				"type":  reason,
			})
			return
		}

		allowed, retryAfter := key.Allow(time.Now())
		if !allowed {
			metrics.RecordKeyThrottled(key.Name)
			// 429 必须带 Retry-After：否则客户端（含带退避重试的 SDK）只能盲重试，
			// 把一次限流放大成一轮重试风暴——这正是限流最怕引发的次生灾害。
			secs := int(retryAfter.Seconds()) + 1
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			log.Printf("[auth] 限流 key=%s %s %s (retry after %ds)", key.Name, r.Method, r.URL.Path, secs)
			writeJSONError(w, http.StatusTooManyRequests, map[string]string{
				"error":       "rate limit exceeded for this API key",
				"retry_after": strconv.Itoa(secs) + "s",
			})
			return
		}

		// 包裹响应器以捕获状态码：用量归因需要 {key, status_code} 两个维度，
		// 而状态码只有下游写完响应才知道。
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// 把 Key 名带进 context，让 proxy 能把 token 用量归因到具体调用方
		next.ServeHTTP(rec, r.WithContext(WithKeyName(r.Context(), key.Name)))
		metrics.RecordKeyRequest(key.Name, rec.status)
	})
}

// statusRecorder 捕获下游写入的状态码。
//
// ⚠️ 必须实现 Flush 与 Unwrap，否则会静默破坏流式响应：
//   - Flush：SSE 逐块转发依赖 http.Flusher。若包装后丢失该接口，
//     proxy 会走「降级为非流式」分支——响应变成一次性返回，流式彻底失效。
//   - Unwrap：proxy 用 http.NewResponseController(w).SetWriteDeadline() 清除
//     Server.WriteTimeout 打在连接上的写 deadline（否则长 SSE 会被硬掐）。
//     ResponseController 只认底层真实连接，靠 Unwrap 逐层穿透；
//     不实现它，鉴权一开启流式超时保护就失效，且失败得很隐蔽。
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	// 下游只 Write 不显式 WriteHeader 时，Go 会隐式补 200，
	// 这里同步记录，避免归因到错误的 0 值。
	if !s.written {
		s.status = http.StatusOK
		s.written = true
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 暴露底层 ResponseWriter，供 http.NewResponseController 穿透到真实连接。
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func writeJSONError(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		// 告诉客户端期望的认证方式，符合 RFC 7235
		w.Header().Set("WWW-Authenticate", `Bearer realm="inference-gateway"`)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
