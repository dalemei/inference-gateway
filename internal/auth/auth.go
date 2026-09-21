// Package auth 提供网关入口治理：API Key 鉴权 + 按 Key 的令牌桶限流。
//
// 定位：网关此前只做「透明转发」——谁在调、调了多少、能不能调，全无管控。
// 这一层补上后，网关才从「四层代理」升级为「控制平面」。
//
// 设计约束：保持零第三方依赖。令牌桶自己实现（见 limiter.go），
// 不引入 golang.org/x/time/rate——本项目 README 的立身之本是「零依赖单二进制」，
// 为一个 60 行的数据结构破坏它不划算。
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dalemei/inference-gateway/internal/config"
)

// 鉴权失败原因，用于指标标签与日志，便于区分「没带 Key」和「带了但不对」——
// 前者通常是客户端漏配，后者更可能是 Key 泄露或轮换未同步，处置方式完全不同。
const (
	ReasonMissing = "missing_key" // 请求未携带 API Key
	ReasonInvalid = "invalid_key" // 携带了 Key 但未匹配任何已配置的 Key
)

// APIKey 一个已通过鉴权的调用方身份
type APIKey struct {
	// Name 配置里给 Key 起的标识，用于日志与指标标签（不参与鉴权）
	Name string
	// limiter 该 Key 专属的令牌桶；nil 表示不限流（只鉴权）
	limiter *Limiter
}

// Allow 扣减一个令牌。返回是否放行；被拒时同时返回建议的重试等待时长。
func (k *APIKey) Allow(now time.Time) (allowed bool, retryAfter time.Duration) {
	if k.limiter == nil {
		return true, 0
	}
	return k.limiter.Allow(now)
}

// LimitDesc 该 Key 的限流配置描述（启动日志用，不含任何密钥 material）
func (k *APIKey) LimitDesc() string {
	if k.limiter == nil {
		return "不限流"
	}
	return fmt.Sprintf("%.4g req/s, 突发 %.4g", k.limiter.rate, k.limiter.burst)
}

// Authenticator API Key 鉴权器。只读，可并发使用（初始化后无任何写操作）。
type Authenticator struct {
	// header 自定义取 Key 的请求头名。为空时走默认双通道（见 extract）。
	header string
	// keys sha256 摘要（小写 hex）→ Key 信息。内存里不保留明文 Key。
	keys map[string]*APIKey
	// ordered 按配置顺序保存，仅用于日志/描述输出（map 遍历顺序随机，日志会跳动）
	ordered []*APIKey
}

// Describe 返回每个 Key 的一行描述，供启动日志打印。不含任何密钥 material。
func (a *Authenticator) Describe() []string {
	out := make([]string, 0, len(a.ordered))
	for _, k := range a.ordered {
		out = append(out, fmt.Sprintf("%s — %s", k.Name, k.LimitDesc()))
	}
	return out
}

// New 从配置构造鉴权器。
//
// 配置里 key（明文）与 key_hash 二选一：填明文时在此处立即换算成摘要，
// 之后明文即被丢弃，进程内存与转储里都只剩摘要。
func New(cfg config.AuthConfig) (*Authenticator, error) {
	if len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("未配置任何 API Key")
	}

	keys := make(map[string]*APIKey, len(cfg.Keys))
	ordered := make([]*APIKey, 0, len(cfg.Keys))
	for i, kc := range cfg.Keys {
		hash := strings.ToLower(strings.TrimSpace(kc.KeyHash))
		if hash == "" {
			if strings.TrimSpace(kc.Key) == "" {
				return nil, fmt.Errorf("auth.keys[%d] (%s) key 与 key_hash 均为空", i, kc.Name)
			}
			hash = HashKey(kc.Key)
		}
		if len(hash) != 64 {
			return nil, fmt.Errorf("auth.keys[%d] (%s) key_hash 必须是 64 位 sha256 十六进制串，实际 %d 位",
				i, kc.Name, len(hash))
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf("auth.keys[%d] (%s) key_hash 不是合法十六进制: %w", i, kc.Name, err)
		}

		apiKey := &APIKey{
			Name:    kc.Name,
			limiter: NewLimiter(kc.RateLimit, kc.Burst),
		}
		keys[hash] = apiKey
		ordered = append(ordered, apiKey)
	}

	return &Authenticator{
		header:  strings.TrimSpace(cfg.Header),
		keys:    keys,
		ordered: ordered,
	}, nil
}

// Authenticate 校验请求的 API Key。
// 返回匹配的 Key（失败时为 nil）与失败原因（成功时为空串）。
func (a *Authenticator) Authenticate(r *http.Request) (*APIKey, string) {
	presented := a.extract(r)
	if presented == "" {
		return nil, ReasonMissing
	}

	// 先算摘要再比较：把「比较长度不同的字符串」这个可变因素消掉，
	// 之后每次比较都是等长的 64 字节。
	sum := HashKey(presented)

	// 恒定时间比较，避免通过响应耗时逐字节爆破 Key。
	// 遍历全部条目而不是命中即返回：否则「第几个 Key 命中」会泄露匹配位置。
	matched := (*APIKey)(nil)
	for hash, key := range a.keys {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(sum)) == 1 {
			matched = key
		}
	}
	if matched == nil {
		return nil, ReasonInvalid
	}
	return matched, ""
}

// extract 从请求头中取出客户端提供的 Key。
//
// 默认（未配置 header）同时接受两种业界惯例：
//
//	Authorization: Bearer <key>   ← OpenAI SDK / 绝大多数客户端
//	X-API-Key: <key>              ← 部分内部系统与网关（Kong、One API 等）
//
// 两种都支持是为了让调用方不必为接网关改客户端。若显式配了 header，
// 则只认那一个头（Authorization 仍按 Bearer 解析）。
func (a *Authenticator) extract(r *http.Request) string {
	if a.header != "" {
		v := r.Header.Get(a.header)
		if strings.EqualFold(a.header, "Authorization") {
			return trimBearer(v)
		}
		return strings.TrimSpace(v)
	}

	// Bearer 优先：同时带了两个头且不一致时，以更明确的 Authorization 为准
	if v := trimBearer(r.Header.Get("Authorization")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	return ""
}

// trimBearer 从 "Bearer xxx" 中取出 xxx。HTTP 认证 scheme 大小写不敏感（RFC 7235），
// 故前缀判断忽略大小写；无 Bearer 前缀时原样返回（有些客户端直接把裸 Key 塞进 Authorization）。
func trimBearer(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

// HashKey 计算 Key 的 sha256 摘要（小写十六进制，64 字符）。
// 导出以便运维用 `gateway --hash-key` 之类的方式生成配置用的 key_hash。
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
