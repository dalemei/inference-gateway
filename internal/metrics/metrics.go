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

// split2 按第一个 "|" 拆成两段；无分隔符时第二段返回 "unknown"
func split2(s string) (string, string) {
	i := strings.Index(s, "|")
	if i < 0 {
		return s, "unknown"
	}
	return s[:i], s[i+1:]
}
