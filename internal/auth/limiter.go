package auth

import (
	"math"
	"sync"
	"time"
)

// Limiter 令牌桶限流器（每 API Key 一个实例）。
//
// 为什么是自己实现而不用 golang.org/x/time/rate？
// 本项目 README 的立身之本是「零依赖单二进制」，为引入一个约 60 行的数据结构
// 而新增外部模块不划算；且自研版可以接受外部传入的时间戳，
// 让限流行为能用假时钟做确定性单测（x/time/rate 依赖真实时钟，只能靠 sleep 测，慢且不稳）。
//
// 语义：桶容量 burst，按 rate 个/秒匀速填充；请求到来时取走一个令牌，
// 取不到即拒绝。突发流量可一次性打空桶，之后被压到 rate 的稳态速率。
type Limiter struct {
	rate  float64 // 令牌填充速率（个/秒）
	burst float64 // 桶容量

	mu         sync.Mutex
	tokens     float64
	last       time.Time
	initialized bool
}

// NewLimiter 创建限流器。rate <= 0 表示不限流，返回 nil（调用方按 nil 判断）。
// burst <= 0 时自动取 max(1, rate)：给一拍突发额度，
// 因为推理请求天然是突发性的（用户点一次发一批），纯 QPS 硬限会把正常交互误杀。
func NewLimiter(rate float64, burst int) *Limiter {
	if rate <= 0 {
		return nil
	}
	b := float64(burst)
	if b < 1 {
		b = math.Max(1, math.Ceil(rate))
	}
	return &Limiter{rate: rate, burst: b, tokens: b}
}

// Allow 尝试取走一个令牌。
//
// now 由调用方传入而非内部取 time.Now()：
// 一是可在单测里注入假时钟做确定性断言，二是批量场景可复用同一次取时结果。
//
// 返回是否放行；被拒时 retryAfter 给出「大约多久后能再试」，供 Retry-After 响应头使用。
func (l *Limiter) Allow(now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 首次调用以传入时间为基准，避免零值与 now 相差数十年导致令牌瞬间充满
	// （虽会被 burst 截断而不出错，但语义不干净，且会掩盖时钟异常）。
	if !l.initialized {
		l.last = now
		l.initialized = true
	}

	elapsed := now.Sub(l.last).Seconds()
	if elapsed > 0 {
		l.tokens += elapsed * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}
	// elapsed <= 0（时钟回拨或并发下的时间倒流）：不填充也不移动基准，
	// 保守处理——宁可少放几个请求，也不要因时钟问题瞬间放行一大波。

	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}

	// 还差 (1 - tokens) 个令牌，按 rate 填充需要多久
	wait := (1 - l.tokens) / l.rate
	if wait < 0 || math.IsNaN(wait) || math.IsInf(wait, 0) {
		wait = 0
	}
	return false, time.Duration(wait * float64(time.Second))
}
