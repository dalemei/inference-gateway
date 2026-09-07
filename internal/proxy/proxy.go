package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"github.com/dalemei/inference-gateway/internal/backend"
	"github.com/dalemei/inference-gateway/internal/metrics"
)

// requestIDCounter 自增请求 ID 计数器
var requestIDCounter atomic.Uint64

// Proxy 推理请求代理，负责接收客户端请求并转发到健康后端
type Proxy struct {
	pool       *backend.BackendPool
	timeout    time.Duration
	maxRetries int
	debug      bool          // 调试日志开关：开启时打印请求头与 body（含敏感信息）
	client     *http.Client // 共享 HTTP 客户端（连接池复用）
}

// NewProxy 创建代理实例
func NewProxy(pool *backend.BackendPool, timeout time.Duration, maxRetries int, debug bool) *Proxy {
	return &Proxy{
		pool:       pool,
		timeout:    timeout,
		maxRetries: maxRetries,
		debug:      debug,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  false,
			},
		},
	}
}

// ServeHTTP 入口：检测 streaming / 非 streaming，走不同代理路径
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDCounter.Add(1)

	// 读请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[请求 #%d] 读取请求体失败: %v", reqID, err)
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	// 关闭原始请求体（释放连接资源）
	r.Body.Close()

	// 自动检测并转换编码：Windows 终端发 GBK → 转 UTF-8
	origLen := len(body)
	body, converted := ensureUTF8(body)
	if converted {
		log.Printf("[请求 #%d] 编码转换: GBK → UTF-8 (%d 字节 → %d 字节)", reqID, origLen, len(body))
	}

	// 检测是否为流式请求
	isStreaming := detectStreaming(body)
	log.Printf("[请求 #%d] 流式检测: %v, body 长度: %d 字节", reqID, isStreaming, len(body))
	if isStreaming {
		p.handleStreaming(w, r, body, reqID)
	} else {
		p.handleRequest(w, r, body, reqID)
	}
}

// ensureUTF8 检测并转换 body 编码为 UTF-8。
// Windows 终端（CMD/PowerShell）默认发送 GBK 编码的中文，
// 而 JSON 规范要求 UTF-8，vLLM 也只接受 UTF-8。
// 此函数在网关侧自动修正编码，对客户端透明。
func ensureUTF8(body []byte) ([]byte, bool) {
	if utf8.Valid(body) {
		return body, false // 已经是 UTF-8，无需转换
	}
	// 尝试 GBK → UTF-8
	reader := transform.NewReader(bytes.NewReader(body), simplifiedchinese.GBK.NewDecoder())
	converted, err := io.ReadAll(reader)
	if err != nil {
		return body, false // 转换失败，返回原样（让后端报错）
	}
	// 二次验证：转换结果必须是有效 UTF-8
	if !utf8.Valid(converted) {
		return body, false
	}
	return converted, true
}

// detectStreaming 解析请求体检测 stream 字段
func detectStreaming(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return req.Stream
}

// ============================================================================
// 非流式请求：带重试的代理转发
// ============================================================================

func (p *Proxy) handleRequest(w http.ResponseWriter, r *http.Request, body []byte, reqID uint64) {
	var lastErr error
	tried := make(map[string]bool)

	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		// 选择后端（重试时排除已失败的）
		var backend *backend.Backend
		if attempt == 0 {
			backend = p.pool.Next()
		} else {
			backend = p.pool.NextExcluding(tried)
		}

		if backend == nil {
			metrics.RecordGatewayError("no_healthy_backend")
			log.Printf("[请求 #%d] 无可用健康后端 (已尝试: %v)", reqID, triedKeys(tried))
			writeError(w, http.StatusServiceUnavailable, "no healthy backends available")
			return
		}
		tried[backend.Name] = true

		// 构建目标 URL
		targetURL := strings.TrimRight(backend.URL, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		// 创建代理请求
		proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

	// 复制请求头（排除 Host 和 Content-Length）
	copyHeaders(proxyReq, r)
	// 显式设置 Content-Length，避免 Go HTTP client 使用 chunked 编码导致后端解析异常
	proxyReq.ContentLength = int64(len(body))

	// 发送请求
	start := time.Now()
	resp, err := p.client.Do(proxyReq)
		duration := time.Since(start)

		if err != nil {
			lastErr = err
			log.Printf("[请求 #%d] %s → %s 失败 (attempt %d/%d): %v",
				reqID, r.URL.Path, backend.Name, attempt+1, p.maxRetries+1, err)
			metrics.RecordGatewayError("backend_unreachable")
			// 仅当还有剩余重试次数时才计入重试次数（最后一次失败不再重试）
			if attempt < p.maxRetries {
				metrics.RecordRetry()
			}
			continue // 重试
		}
		defer resp.Body.Close()

		// 复制响应头
		for key, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}

		// 写入状态码和响应体
		w.WriteHeader(resp.StatusCode)
		written, _ := io.Copy(w, resp.Body)

		// 记录指标
		metrics.RecordRequest(backend.Name, resp.StatusCode, duration, written)

		log.Printf("[请求 #%d] %s → %s %d (%.1fms, %d bytes, attempt %d/%d)",
			reqID, r.URL.Path, backend.Name, resp.StatusCode,
			float64(duration.Microseconds())/1000, written,
			attempt+1, p.maxRetries+1)
		return
	}

	// 所有重试均失败
	metrics.RecordGatewayError("all_retries_failed")
	log.Printf("[请求 #%d] 所有重试均失败 (已尝试: %v, 最后错误: %v)", reqID, triedKeys(tried), lastErr)
	writeError(w, http.StatusBadGateway,
		fmt.Sprintf("all %d backends unreachable, last error: %v", len(tried), lastErr))
}

// ============================================================================
// 流式请求（SSE）：逐块转发，不做缓冲
// ============================================================================

func (p *Proxy) handleStreaming(w http.ResponseWriter, r *http.Request, body []byte, reqID uint64) {
	backend := p.pool.Next()
	if backend == nil {
		metrics.RecordGatewayError("no_healthy_backend")
		writeError(w, http.StatusServiceUnavailable, "no healthy backends available")
		return
	}

	targetURL := strings.TrimRight(backend.URL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 复制请求头
	copyHeaders(proxyReq, r)
	// 显式设置 Content-Length，避免 chunked 编码导致 vLLM 解析失败
	proxyReq.ContentLength = int64(len(body))

	// 调试日志：默认关闭。开启（-debug）时打印完整请求头与 body，
	// 会暴露 Authorization 等敏感头及 prompt 原文，仅限排障环境使用。
	if p.debug {
		log.Printf("[SSE #%d] 代理请求 URL: %s", reqID, targetURL)
		log.Printf("[SSE #%d] 代理请求 Headers: %v", reqID, proxyReq.Header)
		log.Printf("[SSE #%d] 代理请求 ContentLength: %d", reqID, proxyReq.ContentLength)
		log.Printf("[SSE #%d] 代理请求 Body HEX: %x", reqID, body)
	} else {
		log.Printf("[SSE #%d] → %s (backend %s)", reqID, targetURL, backend.Name)
	}

	// 流式请求不能用带超时的 client（SSE 可能持续数分钟）
	streamClient := &http.Client{
		Transport: p.client.Transport, // 复用连接池
		Timeout:   0,                   // 无超时
	}

	start := time.Now()
	resp, err := streamClient.Do(proxyReq)
	if err != nil {
		log.Printf("[SSE #%d] %s → %s 连接失败: %v", reqID, r.URL.Path, backend.Name, err)
		metrics.RecordGatewayError("backend_unreachable")
		writeError(w, http.StatusBadGateway, fmt.Sprintf("backend %s unreachable: %v", backend.Name, err))
		return
	}
	defer resp.Body.Close()

	// vLLM 返回了非 200 错误，读取完整响应体辅助排查
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Printf("[SSE #%d] vLLM 错误响应体: %s", reqID, string(errBody))
		for key, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(errBody)
		metrics.RecordRequest(backend.Name, resp.StatusCode, time.Since(start), int64(len(errBody)))
		log.Printf("[SSE #%d] %s → %s 返回错误 %d", reqID, r.URL.Path, backend.Name, resp.StatusCode)
		return
	}

	// 复制响应头
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	// 检查是否支持 Flush（SSE 必须）
	flusher, ok := w.(http.Flusher)
	if !ok {
		// 降级：回退到非流式模式（一次性返回所有内容）
		w.WriteHeader(resp.StatusCode)
		written, _ := io.Copy(w, resp.Body)
		metrics.RecordRequest(backend.Name, resp.StatusCode, time.Since(start), written)
		log.Printf("[SSE #%d] Flusher 不支持，降级为非流式 (%d bytes)", reqID, written)
		return
	}

	w.WriteHeader(resp.StatusCode)
	metrics.RecordSSEConnection()

	var totalBytes int64
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				log.Printf("[SSE #%d] 客户端断开: %v (已发送 %d bytes)", reqID, writeErr, totalBytes)
				break
			}
			flusher.Flush()
			totalBytes += int64(n)
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("[SSE #%d] 读取后端响应出错: %v (已发送 %d bytes)", reqID, readErr, totalBytes)
			}
			break
		}
	}

	metrics.RecordSSEConnectionClosed()
	metrics.RecordRequest(backend.Name, resp.StatusCode, time.Since(start), totalBytes)
	log.Printf("[SSE #%d] %s → %s %d (%.1fs, %d bytes)",
		reqID, r.URL.Path, backend.Name, resp.StatusCode,
		time.Since(start).Seconds(), totalBytes)
}

// ============================================================================
// 管理端点
// ============================================================================

// HealthHandler 网关自身健康检查
func (p *Proxy) HealthHandler(w http.ResponseWriter, r *http.Request) {
	healthy := p.pool.HealthyCount()
	total := len(p.pool.Backends())

	w.Header().Set("Content-Type", "application/json")
	if healthy == 0 && total > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "degraded",
			"healthy_backends": healthy,
			"total_backends":   total,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":           "healthy",
		"healthy_backends": healthy,
		"total_backends":   total,
	})
}

// BackendsHandler 查看后端详情（调试用）
func (p *Proxy) BackendsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	backends := p.pool.Backends()

	type backendStatus struct {
		Name    string  `json:"name"`
		URL     string  `json:"url"`
		Healthy bool    `json:"healthy"`
		Latency float64 `json:"latency_ms"`
	}

	result := make([]backendStatus, 0, len(backends))
	for _, b := range backends {
		result = append(result, backendStatus{
			Name:    b.Name,
			URL:     b.URL,
			Healthy: b.IsHealthy(),
			Latency: float64(b.Latency.Microseconds()) / 1000,
		})
	}
	json.NewEncoder(w).Encode(result)
}

// MetricsHandler Prometheus 格式指标输出
func (p *Proxy) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	metrics.WriteMetrics(w, p.pool)
}

// ========== 工具函数 ==========

// copyHeaders 复制请求头，排除 Host 和 Content-Length（后者由 Go HTTP client 自动计算）
func copyHeaders(dst *http.Request, src *http.Request) {
	for key, values := range src.Header {
		if strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, v := range values {
			dst.Header.Add(key, v)
		}
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// triedKeys 将 map[string]bool 的 key 拼接成逗号分隔字符串（日志用）
func triedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}
