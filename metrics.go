package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

	totalRequests atomic.Int64
	totalErrors   atomic.Int64

	// SSE 连接计数
	sseConnectionsActive atomic.Int64
	sseConnectionsTotal  atomic.Int64

	// 重试计数
	retryCount atomic.Int64
)

// recordRequest 记录一次成功代理请求的指标
func recordRequest(backend string, statusCode int, duration time.Duration, bytesWritten int64) {
	totalRequests.Add(1)

	metricsMu.Lock()
	defer metricsMu.Unlock()

	key := backend + "|" + strconv.Itoa(statusCode)
	requestCount[key]++
	requestLatency[backend] = duration.Seconds()
}

// recordGatewayError 记录网关层错误
func recordGatewayError(errorType string) {
	totalErrors.Add(1)

	metricsMu.Lock()
	defer metricsMu.Unlock()

	errorCount[errorType]++
}

// recordSSEConnection SSE 连接建立
func recordSSEConnection() {
	sseConnectionsActive.Add(1)
	sseConnectionsTotal.Add(1)
}

// recordSSEConnectionClosed SSE 连接关闭
func recordSSEConnectionClosed() {
	sseConnectionsActive.Add(-1)
}

// recordRetry 记录一次重试
func recordRetry() {
	retryCount.Add(1)
}

// writeMetrics 输出 Prometheus 兼容格式的指标
func writeMetrics(w io.Writer, pool *BackendPool) {
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
		parts := strings.SplitN(key, "|", 2)
		backend := parts[0]
		statusCode := "unknown"
		if len(parts) > 1 {
			statusCode = parts[1]
		}
		sb.WriteString(fmt.Sprintf(
			`inference_gateway_requests_total{backend="%s",status_code="%s"} %d`+"\n",
			backend, statusCode, count))
	}
	if len(requestCount) == 0 {
		sb.WriteString("# (尚无请求)\n")
	}

	// 错误计数
	sb.WriteString("\n# HELP inference_gateway_errors_total 网关层错误总数（按类型）\n")
	sb.WriteString("# TYPE inference_gateway_errors_total counter\n")
	for _, t := range []string{"no_healthy_backend", "backend_unreachable", "all_retries_failed"} {
		count := errorCount[t]
		sb.WriteString(fmt.Sprintf(`inference_gateway_errors_total{type="%s"} %d`+"\n", t, count))
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
	backends := pool.Backends()
	for _, b := range backends {
		val := 0
		if b.IsHealthy() {
			val = 1
		}
		sb.WriteString(fmt.Sprintf(
			`inference_gateway_backend_health{backend="%s",url="%s"} %d`+"\n",
			b.Name, b.URL, val))
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
	for _, b := range backends {
		lat := float64(b.Latency) / float64(time.Second)
		sb.WriteString(fmt.Sprintf(
			`inference_gateway_backend_latency_seconds{backend="%s",url="%s"} %.6f`+"\n",
			b.Name, b.URL, lat))
	}

	w.Write([]byte(sb.String()))
}
