package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dalemei/inference-gateway/internal/backend"
	"github.com/dalemei/inference-gateway/internal/usage"
)

// ========== 指标数据结构 ==========

var (
	metricsMu        sync.Mutex
	gatewayStartTime = time.Now()

	// 按后端维度
	requestCount   = make(map[string]int64)   // key: "backend|status_code"
	requestLatency = make(map[string]float64) // key: "backend" → 最近一次延迟（秒）

	// 全局错误计数
	errorCount = make(map[string]int64) // key: error_type

	// 入口治理维度（API Key 鉴权 / 限流 / 用量归因）
	authFailureCount  = make(map[string]int64) // key: 失败原因（missing_key / invalid_key）
	keyRequestCount   = make(map[string]int64) // key: "key名|status_code" → 按 Key 的用量
	keyThrottledCount = make(map[string]int64) // key: key名 → 被限流次数

	// token 用量维度（成本观测 / 按 Key 分账）
	// key: "direction|model|backend|key"，direction ∈ prompt/completion/cached
	tokenCount   = make(map[string]int64)
	usageMissing = make(map[string]int64) // key: 缺失原因

	totalRequests atomic.Int64
	totalErrors   atomic.Int64

	// SSE 连接计数
	sseConnectionsActive atomic.Int64
	sseConnectionsTotal  atomic.Int64

	// 重试计数
	retryCount atomic.Int64
)

// RecordRequest 记录一次成功代理请求的指标（按 model+backend+status 归因）
func RecordRequest(model, backendName string, statusCode int, duration time.Duration, bytesWritten int64) {
	if model == "" {
		model = "unknown"
	}
	totalRequests.Add(1)

	metricsMu.Lock()
	defer metricsMu.Unlock()

	key := model + "|" + backendName + "|" + strconv.Itoa(statusCode)
	requestCount[key]++
	requestLatency[backendName] = duration.Seconds()
}

// recordGatewayError 记录网关层错误
func RecordGatewayError(errorType string) {
	totalErrors.Add(1)

	metricsMu.Lock()
	defer metricsMu.Unlock()

	errorCount[errorType]++
}

// recordSSEConnection SSE 连接建立
func RecordSSEConnection() {
	sseConnectionsActive.Add(1)
	sseConnectionsTotal.Add(1)
}

// recordSSEConnectionClosed SSE 连接关闭
func RecordSSEConnectionClosed() {
	sseConnectionsActive.Add(-1)
}

// recordRetry 记录一次重试
func RecordRetry() {
	retryCount.Add(1)
}

// ========== 入口治理指标（鉴权 / 限流 / 用量归因） ==========

// RecordAuthFailure 记录一次 API Key 鉴权失败
func RecordAuthFailure(reason string) {
	totalErrors.Add(1)

	metricsMu.Lock()
	defer metricsMu.Unlock()

	authFailureCount[reason]++
}

// RecordKeyRequest 记录某个 Key 的一次已完成请求（含状态码），用于用量归因与成本分摊
func RecordKeyRequest(keyName string, statusCode int) {
	metricsMu.Lock()
	defer metricsMu.Unlock()

	keyRequestCount[keyName+"|"+strconv.Itoa(statusCode)]++
}

// RecordKeyThrottled 记录某个 Key 被限流拒绝一次
func RecordKeyThrottled(keyName string) {
	totalErrors.Add(1)

	metricsMu.Lock()
	defer metricsMu.Unlock()

	keyThrottledCount[keyName]++
}

// ========== token 用量指标（成本观测 / 分账） ==========

// AnonymousKey 未启用鉴权（或请求未携带 Key）时 token 指标的 key 标签取值。
// 用一个固定字面量而不是空串：空标签在 PromQL 里写起来别扭（key=""），
// 且容易被误读成「标签没打上」。
const AnonymousKey = "anonymous"

// usageMissingBaseline usage_missing 指标的基线原因枚举。
// 与 errors_total 同样的理由：固定输出 0 基线，否则「从未缺失」和
// 「埋点失效 / 后端集体不返回 usage」在 /metrics 上长得一模一样。
var usageMissingBaseline = []string{
	"no_usage_field",     // 响应 200 但解析不出 usage（后端不支持，或路径不是推理端点）
	"response_too_large", // 响应体超过解析上限，跳过解析
	"stream_aborted",     // 流被超时守卫掐断，末尾 usage chunk 永远等不到
}

// RecordTokens 记录一次请求的 token 用量。
//
// cached 序列的输出条件为什么是「后端上报了该字段」而不是「值 > 0」：
// 按 > 0 输出的话，「这次真没命中（0）」与「后端没这个能力（也是 0 或缺失）」
// 都会表现为序列不存在，缓存命中率无从区分「0%」和「未观测」。
// 按 HasCached 输出后：有序列且为 0 = 确实没命中；无序列 = 后端未上报，
// 需结合 usage_missing 与后端启动参数（vLLM 的 --enable-prompt-tokens-details）判断。
func RecordTokens(model, backendName, keyName string, u usage.Usage) {
	if model == "" {
		model = "unknown"
	}
	if keyName == "" {
		keyName = AnonymousKey
	}

	metricsMu.Lock()
	defer metricsMu.Unlock()

	base := model + "|" + backendName + "|" + keyName
	tokenCount["prompt|"+base] += u.PromptTokens
	tokenCount["completion|"+base] += u.CompletionTokens
	if u.HasCached {
		tokenCount["cached|"+base] += u.CachedTokens
	}
}

// RecordUsageMissing 记录一次「本该有 usage 却没拿到」的请求。
//
// 没有这个指标，token 总量下降会被误读成「成本降低了」，
// 而真实原因可能是后端升级后不再返回 usage、或流式被大量掐断。
// 分母缺失是成本看板最危险的失真，必须单独可见。
func RecordUsageMissing(reason string) {
	metricsMu.Lock()
	defer metricsMu.Unlock()

	usageMissing[reason]++
}

// WriteMetrics 输出 Prometheus 兼容格式的指标（接收多后端池）
func WriteMetrics(w io.Writer, pools []*backend.BackendPool) {
	metricsMu.Lock()
	defer metricsMu.Unlock()

	var sb strings.Builder

	// 运行时长
	sb.WriteString("# HELP inference_gateway_uptime_seconds 网关运行时长（秒）\n")
	sb.WriteString("# TYPE inference_gateway_uptime_seconds gauge\n")
	uptime := time.Since(gatewayStartTime).Seconds()
	sb.WriteString(fmt.Sprintf("inference_gateway_uptime_seconds %.2f\n", uptime))

	// 请求计数（按后端+状态码）
	sb.WriteString("\n# HELP inference_gateway_requests_total 代理请求总数（按后端+状态码）\n")
	sb.WriteString("# TYPE inference_gateway_requests_total counter\n")
	for key, count := range requestCount {
		parts := strings.SplitN(key, "|", 3)
		model := "unknown"
		backendName := "unknown"
		statusCode := "unknown"
		if len(parts) > 0 {
			model = parts[0]
		}
		if len(parts) > 1 {
			backendName = parts[1]
		}
		if len(parts) > 2 {
			statusCode = parts[2]
		}
		sb.WriteString(fmt.Sprintf(
			`inference_gateway_requests_total{model="%s",backend="%s",status_code="%s"} %d`+"\n",
			model, backendName, statusCode, count))
	}
	if len(requestCount) == 0 {
		sb.WriteString("# (尚无请求)\n")
	}

	// 错误计数
	sb.WriteString("\n# HELP inference_gateway_errors_total 网关层错误总数（按类型）\n")
	sb.WriteString("# TYPE inference_gateway_errors_total counter\n")
	// 先输出已知类型（即使为 0 也输出，保证告警规则的标签组合始终存在），
	// 再输出运行期新增的类型。若只写死已知类型，新错误类型会被累加进 map 却
	// 永不输出——等于埋了一个看不见的点（本仓库此前踩过的「埋点废点」）。
	seen := make(map[string]bool, 8)
	for _, t := range []string{
		"no_healthy_backend",
		"backend_unreachable",
		"all_retries_failed",
		"retryable_status",
		"request_body_too_large",
		"stream_idle_timeout",
		"stream_max_duration",
	} {
		count := errorCount[t]
		sb.WriteString(fmt.Sprintf(`inference_gateway_errors_total{type="%s"} %d`+"\n", t, count))
		seen[t] = true
	}
	// 其余运行期出现的类型按字典序输出，保证多次抓取结果稳定可比对
	var extra []string
	for t := range errorCount {
		if !seen[t] {
			extra = append(extra, t)
		}
	}
	sort.Strings(extra)
	for _, t := range extra {
		sb.WriteString(fmt.Sprintf(`inference_gateway_errors_total{type="%s"} %d`+"\n", t, errorCount[t]))
	}

	// ===== 入口治理指标 =====
	//
	// 为什么用独立指标而不是给 requests_total 加 key 标签？
	// requests_total 的维度已经是 model × backend × status_code，再乘上 key 会让时间序列数
	// 成倍膨胀（Prometheus 里每增加一个标签值就是一条新序列）。而「哪个后端处理了」
	// 与「哪个 Key 用了」是两个正交的关注点，拆成独立指标后靠 PromQL 聚合即可，
	// 既控制基数，也让「用量归因」这件事有自己的语义边界。
	//
	// 鉴权失败的原因标签是有限枚举，固定输出 0 基线——
	// 否则「从未发生过」与「埋点失效」在 /metrics 上长得一样，告警规则无从下手。
	sb.WriteString("\n# HELP inference_gateway_auth_failures_total API Key 鉴权失败次数（按原因）\n")
	sb.WriteString("# TYPE inference_gateway_auth_failures_total counter\n")
	for _, t := range []string{"missing_key", "invalid_key"} {
		sb.WriteString(fmt.Sprintf(`inference_gateway_auth_failures_total{reason="%s"} %d`+"\n", t, authFailureCount[t]))
	}
	for _, t := range sortedKeysOf(authFailureCount, "missing_key", "invalid_key") {
		sb.WriteString(fmt.Sprintf(`inference_gateway_auth_failures_total{reason="%s"} %d`+"\n", t, authFailureCount[t]))
	}

	// 按 Key 的用量（仅在启用鉴权且有流量时出现）
	if len(keyRequestCount) > 0 {
		sb.WriteString("\n# HELP inference_gateway_key_requests_total 按 API Key 统计的请求数（用量归因/成本分摊）\n")
		sb.WriteString("# TYPE inference_gateway_key_requests_total counter\n")
		for _, k := range sortedKeysOf(keyRequestCount) {
			name, status := split2(k)
			sb.WriteString(fmt.Sprintf(`inference_gateway_key_requests_total{key="%s",status_code="%s"} %d`+"\n",
				name, status, keyRequestCount[k]))
		}
	}

	if len(keyThrottledCount) > 0 {
		sb.WriteString("\n# HELP inference_gateway_key_throttled_total 按 API Key 统计的限流拒绝次数\n")
		sb.WriteString("# TYPE inference_gateway_key_throttled_total counter\n")
		for _, k := range sortedKeysOf(keyThrottledCount) {
			sb.WriteString(fmt.Sprintf(`inference_gateway_key_throttled_total{key="%s"} %d`+"\n", k, keyThrottledCount[k]))
		}
	}

	// ===== token 用量指标 =====
	//
	// 三个方向拆成三个指标族而不是一个带 direction 标签的指标：
	// 缓存命中率要写成 rate(cached)/rate(prompt)，拆开后两个 rate 各取自
	// 独立序列，语义清晰；合成一个的话每条 PromQL 都要先做 label filter 再聚合，易写错。
	sb.WriteString("\n# HELP inference_gateway_prompt_tokens_total 输入 token 总数（按模型/后端/Key）\n")
	sb.WriteString("# TYPE inference_gateway_prompt_tokens_total counter\n")
	writeTokenFamily(&sb, "prompt", "inference_gateway_prompt_tokens_total")

	sb.WriteString("\n# HELP inference_gateway_completion_tokens_total 输出 token 总数（按模型/后端/Key）\n")
	sb.WriteString("# TYPE inference_gateway_completion_tokens_total counter\n")
	writeTokenFamily(&sb, "completion", "inference_gateway_completion_tokens_total")

	sb.WriteString("\n# HELP inference_gateway_cached_tokens_total 前缀缓存命中的输入 token 数（仅后端上报时输出）\n")
	sb.WriteString("# TYPE inference_gateway_cached_tokens_total counter\n")
	writeTokenFamily(&sb, "cached", "inference_gateway_cached_tokens_total")

	// 用量缺失计数：token 指标的分母健康度。
	// 只在有数据时输出——全 0 会让人误以为「一直没缺失」，而实际上可能只是没流量。
	sb.WriteString("\n# HELP inference_gateway_usage_missing_total 未能取到 usage 的请求数（按原因）\n")
	sb.WriteString("# TYPE inference_gateway_usage_missing_total counter\n")
	for _, r := range usageMissingBaseline {
		sb.WriteString(fmt.Sprintf(`inference_gateway_usage_missing_total{reason="%s"} %d`+"\n", r, usageMissing[r]))
	}
	for _, r := range sortedKeysOf(usageMissing, usageMissingBaseline...) {
		sb.WriteString(fmt.Sprintf(`inference_gateway_usage_missing_total{reason="%s"} %d`+"\n", r, usageMissing[r]))
	}

	// 重试计数
	sb.WriteString("\n# HELP inference_gateway_retries_total 请求重试总次数\n")
	sb.WriteString("# TYPE inference_gateway_retries_total counter\n")
	sb.WriteString(fmt.Sprintf("inference_gateway_retries_total %d\n", retryCount.Load()))

	// SSE 连接
	sb.WriteString("\n# HELP inference_gateway_sse_connections_active 当前活跃 SSE 连接数\n")
	sb.WriteString("# TYPE inference_gateway_sse_connections_active gauge\n")
	sb.WriteString(fmt.Sprintf("inference_gateway_sse_connections_active %d\n", sseConnectionsActive.Load()))

	sb.WriteString("\n# HELP inference_gateway_sse_connections_total SSE 连接历史总数\n")
	sb.WriteString("# TYPE inference_gateway_sse_connections_total counter\n")
	sb.WriteString(fmt.Sprintf("inference_gateway_sse_connections_total %d\n", sseConnectionsTotal.Load()))

	// 后端健康状态
	sb.WriteString("\n# HELP inference_gateway_backend_health 后端健康状态（1=健康, 0=不健康）\n")
	sb.WriteString("# TYPE inference_gateway_backend_health gauge\n")
	for _, pl := range pools {
		for _, b := range pl.Backends() {
			val := 0
			if b.IsHealthy() {
				val = 1
			}
			sb.WriteString(fmt.Sprintf(
				`inference_gateway_backend_health{backend="%s",url="%s"} %d`+"\n",
				b.Name, b.URL, val))
		}
	}

	// 健康检查延迟
	// 请求级延迟（最近一次成功代理的耗时，秒）
	sb.WriteString("\n# HELP inference_gateway_backend_request_latency_seconds 后端最近一次请求延迟（秒）\n")
	sb.WriteString("# TYPE inference_gateway_backend_request_latency_seconds gauge\n")
	for backend, lat := range requestLatency {
		sb.WriteString(fmt.Sprintf(
			`inference_gateway_backend_request_latency_seconds{backend="%s"} %.6f`+"\n",
			backend, lat))
	}

	sb.WriteString("\n# HELP inference_gateway_backend_latency_seconds 健康检查延迟（秒）\n")
	sb.WriteString("# TYPE inference_gateway_backend_latency_seconds gauge\n")
	for _, pl := range pools {
		for _, b := range pl.Backends() {
			lat := float64(b.Latency.Load()) / float64(time.Second)
			sb.WriteString(fmt.Sprintf(
				`inference_gateway_backend_latency_seconds{backend="%s",url="%s"} %.6f`+"\n",
				b.Name, b.URL, lat))
		}
	}

	w.Write([]byte(sb.String()))
}

// sortedKeysOf 返回 map 的键（字典序），exclude 中的键跳过。
//
// Go 的 map 遍历顺序是随机的，直接输出会让每次抓取的指标行顺序都不同。
// 语义上 Prometheus 不在乎顺序，但人工比对与 diff 会彻底失效，
// 排障时「这次比上次多了哪一行」是关键信息，故统一排序。
func sortedKeysOf(m map[string]int64, exclude ...string) []string {
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[e] = true
	}
	out := make([]string, 0, len(m))
	for k := range m {
		if skip[k] {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeTokenFamily 输出某个方向（prompt/completion/cached）的全部 token 序列。
// 调用方必须已持有 metricsMu。
//
// 键格式为 "direction|model|backend|key"；direction 由调用方给定，
// 故先剥掉前缀再按三段拆，避免 model 名里若含 "|" 时把后面字段挤掉。
func writeTokenFamily(sb *strings.Builder, direction, metricName string) {
	prefix := direction + "|"
	n := 0
	for _, k := range sortedKeysOf(tokenCount) {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		parts := strings.SplitN(rest, "|", 3)
		for len(parts) < 3 {
			parts = append(parts, "unknown")
		}
		sb.WriteString(fmt.Sprintf(`%s{model="%s",backend="%s",key="%s"} %d`+"\n",
			metricName, parts[0], parts[1], parts[2], tokenCount[k]))
		n++
	}
	if n == 0 {
		sb.WriteString("# (尚无数据)\n")
	}
}

// split2 按第一个 "|" 拆成两段；无分隔符时第二段返回 "unknown"
func split2(s string) (string, string) {
	i := strings.Index(s, "|")
	if i < 0 {
		return s, "unknown"
	}
	return s[:i], s[i+1:]
}
