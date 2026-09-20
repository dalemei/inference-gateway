package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/dalemei/inference-gateway/internal/router"
)

// requestIDCounter 自增请求 ID 计数器
var requestIDCounter atomic.Uint64

// errStreamTimeout 流式超时守卫触发时写入 context 的 cause。
// 用 cause 而不是直接判断 ctx.Err()，是为了把「网关主动掐断」和「客户端断开连接」
// 区分开——两者都会让 ctx 进入 canceled 状态，但后续处理不同（前者要补发 SSE error 事件）。
var errStreamTimeout = errors.New("stream timeout guard triggered")

// Proxy 推理请求代理，负责接收客户端请求并转发到健康后端
type Proxy struct {
	router            *router.ModelRouter
	timeout           time.Duration
	maxRetries        int
	maxBodyBytes      int64         // 请求体上限，0 = 不限制
	streamIdleTimeout time.Duration // 流式空闲超时，0 = 不启用
	streamMaxDuration time.Duration // 单条流总时长上限，0 = 不限制
	debug             bool          // 调试日志开关：开启时打印请求头与 body（含敏感信息）
	client            *http.Client  // 共享 HTTP 客户端（连接池复用）
}

// Options 创建 Proxy 的配置项。
// 参数超过三个后用结构体传参：既避免长参数列表难以核对顺序，
// 也让后续新增（鉴权 Key、限流等）不必再改调用点签名。
type Options struct {
	Timeout           time.Duration // 非流式请求的后端超时
	MaxRetries        int           // 失败重试次数（0=不重试）
	MaxBodyBytes      int64         // 请求体上限，0 = 不限制
	StreamIdleTimeout time.Duration // 流式空闲超时，0 = 不启用
	StreamMaxDuration time.Duration // 单条流总时长上限，0 = 不限制
	Debug             bool
}

// NewProxy 创建代理实例
func NewProxy(r *router.ModelRouter, opt Options) *Proxy {
	return &Proxy{
		router:            r,
		timeout:           opt.Timeout,
		maxRetries:        opt.MaxRetries,
		maxBodyBytes:      opt.MaxBodyBytes,
		streamIdleTimeout: opt.StreamIdleTimeout,
		streamMaxDuration: opt.StreamMaxDuration,
		debug:             opt.Debug,
		client: &http.Client{
			Timeout: opt.Timeout,
			Transport: &http.Transport{
				MaxIdleConns: 100,
				// MaxIdleConnsPerHost 默认是 2，对网关是致命的：
				// 网关通常只连少数几个后端，并发一上来就会把空闲连接挤掉，
				// 导致每请求重新建 TCP（+TLS），把「连接建立开销」误算成「网关转发开销」。
				// 必须与并发量同量级，否则压测数据不可信。
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  false,
			},
		},
	}
}

// ServeHTTP 入口：检测 streaming / 非 streaming，走不同代理路径
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDCounter.Add(1)

	// 读请求体（带大小上限）
	//
	// Go 的 net/http 默认不限制请求体大小，任何人都能 POST 一个 GB 级 body 把网关内存打满。
	// 这里用 http.MaxBytesReader 设一道闸：超限后 Read 返回 *http.MaxBytesError，
	// 且后续读取一律失败，不会把超限内容读进内存。默认 16MB（长上下文够用，见 config.go）。
	var bodyReader io.Reader = r.Body
	if p.maxBodyBytes > 0 {
		bodyReader = http.MaxBytesReader(w, r.Body, p.maxBodyBytes)
	}

	body, err := io.ReadAll(bodyReader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			log.Printf("[请求 #%d] 请求体超过上限（限制 %d 字节）", reqID, p.maxBodyBytes)
			metrics.RecordGatewayError("request_body_too_large")
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body too large: limit is %d bytes", p.maxBodyBytes))
			return
		}
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
	// 解析 model 字段，用于按 model 路由到对应后端池
	model := extractModel(body)
	log.Printf("[请求 #%d] 流式检测: %v, model: %q, body 长度: %d 字节", reqID, isStreaming, model, len(body))
	if isStreaming {
		p.handleStreaming(w, r, body, reqID, model)
	} else {
		p.handleRequest(w, r, body, reqID, model)
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

// extractModel 解析请求体中的 model 字段，用于按 model 路由到对应后端池
// 无法解析或字段缺失时返回空串，交由 router 落到默认池
func extractModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return req.Model
}

// isRetryableStatus 判断后端响应码是否值得「换一个节点」重试。
//
// 判定原则：只重试「后端自身过载或临时不可用」，不重试 4xx（请求本身有问题，重试无意义）。
// 对推理服务而言这几类最常见：
//
//	429 Too Many Requests — 后端限流（vLLM 并发打满时返回）
//	502 Bad Gateway       — 后端上游异常
//	503 Service Unavailable — 过载 / 模型仍在加载，换节点大概率能成
//	504 Gateway Timeout   — 后端超时
//
// 说明：这里刻意不重试 500。500 在通用网关（Envoy/Nginx 默认）里同样不重试，
// 因为它更可能是请求本身触发的错误（如 prompt 超长、参数非法），换节点也白搭。
// 若你的推理集群 500 多为 GPU OOM，可在此加入 http.StatusInternalServerError。
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,  // 429
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	}
	return false
}

// ============================================================================
// 非流式请求：带重试的代理转发
// ============================================================================

func (p *Proxy) handleRequest(w http.ResponseWriter, r *http.Request, body []byte, reqID uint64, model string) {
	// 按 model 路由到对应后端池（未匹配则落默认池）
	pool := p.router.Route(model)
	if pool == nil {
		metrics.RecordGatewayError("no_healthy_backend")
		writeError(w, http.StatusServiceUnavailable, "no backend pool available for this model")
		return
	}

	var lastErr error
	tried := make(map[string]bool)

	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		// 选择后端（重试时排除已失败的）
		var backend *backend.Backend
		if attempt == 0 {
			backend = pool.Next()
		} else {
			backend = pool.NextExcluding(tried)
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

		// 后端返回「可重试」状态码 → 换一个节点重试。
		// 必须在写响应头之前判断：一旦 WriteHeader 响应即已提交，无法回头。
		// 循环内不能用 defer 兜底关闭（defer 累积到函数结束才执行，会泄漏连接），
		// 故此处显式 Close；后续 defer 的重复 Close 对 http.Body 是幂等的，无副作用。
		if attempt < p.maxRetries && isRetryableStatus(resp.StatusCode) {
			resp.Body.Close()
			metrics.RecordRetry()
			// 必须留痕：否则这类「被重试挽救的后端故障」在指标里完全隐形，
			// 运维看 /metrics 会以为一切正常，错过后端正在劣化的信号。
			metrics.RecordGatewayError("retryable_status")
			log.Printf("[请求 #%d] %s → %s 返回 %d（可重试），换节点 (attempt %d/%d)",
				reqID, r.URL.Path, backend.Name, resp.StatusCode, attempt+1, p.maxRetries+1)
			continue
		}

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
		metrics.RecordRequest(model, backend.Name, resp.StatusCode, duration, written)

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

func (p *Proxy) handleStreaming(w http.ResponseWriter, r *http.Request, body []byte, reqID uint64, model string) {
	// ===== 关键：清掉 Server.WriteTimeout 打在连接上的写 deadline =====
	//
	// Go 的 http.Server.WriteTimeout 是在「请求头读完时」给连接打一次写 deadline，
	// 整个请求期间不重置（见 net/http/server.go readRequest 里的 defer SetWriteDeadline）。
	// 对 SSE 这种分钟级长连接，它等同于到点硬掐：客户端收到 unexpected EOF 而不是完整流。
	//
	// 实测（WriteTimeout=3s 的 server 推 6 秒流）：客户端只收到前 3 秒的 24 字节后报
	// unexpected EOF，应有 48 字节；用 ResponseController 清掉 deadline 后 48 字节完整收到。
	//
	// 因此流式请求必须自己接管超时——由下面的空闲守卫负责，而不是 Server.WriteTimeout。
	// httptest.ResponseRecorder 等不支持 SetWriteDeadline 的 Writer 会返回 ErrNotSupported，
	// 属预期行为（测试环境没有真实连接），不刷日志。
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		log.Printf("[SSE #%d] 清除写超时失败，长流可能被 Server.WriteTimeout 提前掐断: %v", reqID, err)
	}

	// 按 model 路由到对应后端池（未匹配则落默认池）
	pool := p.router.Route(model)
	if pool == nil {
		metrics.RecordGatewayError("no_healthy_backend")
		writeError(w, http.StatusServiceUnavailable, "no backend pool available for this model")
		return
	}
	backend := pool.Next()
	if backend == nil {
		metrics.RecordGatewayError("no_healthy_backend")
		writeError(w, http.StatusServiceUnavailable, "no healthy backends available")
		return
	}

	targetURL := strings.TrimRight(backend.URL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	// 流式超时守卫用的 context：上游 client.Timeout 必须是 0（否则长生成被截断），
	// 但那会让「后端卡死」的连接永久挂起。这里用可取消的 ctx 让阻塞中的 Read 能被打断。
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)

	// 只有配置了超时才起守卫 goroutine，避免给正常请求增加无谓的调度开销
	var progress chan struct{}
	if p.streamIdleTimeout > 0 || p.streamMaxDuration > 0 {
		progress = make(chan struct{}, 1)
		go p.streamWatchdog(reqID, ctx, cancel, progress)
	}

	proxyReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(body))
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
		metrics.RecordRequest(model, backend.Name, resp.StatusCode, time.Since(start), int64(len(errBody)))
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
		metrics.RecordRequest(model, backend.Name, resp.StatusCode, time.Since(start), written)
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

			// 通知守卫：本轮有数据流动，重置空闲计时器（非阻塞，漏一次通知无副作用）
			if progress != nil {
				select {
				case progress <- struct{}{}:
				default:
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("[SSE #%d] 读取后端响应出错: %v (已发送 %d bytes)", reqID, readErr, totalBytes)
			}
			break
		}
	}

	// 被守卫主动掐断：此时 200 已发出，无法再改状态码。
	// 按 SSE 语义补一个 error 事件再关闭，让客户端（OpenAI SDK 等）能区分
	// 「网关主动超时」与「流正常结束」，而不是收到一个没头没尾的截断流。
	if errors.Is(context.Cause(ctx), errStreamTimeout) {
		log.Printf("[SSE #%d] 流被超时守卫终止 (已发送 %d bytes)", reqID, totalBytes)
		fmt.Fprintf(w, "event: error\ndata: {\"error\":{\"message\":\"upstream timeout, stream terminated by gateway\",\"type\":\"gateway_timeout\"}}\n\n")
		flusher.Flush()
	}

	metrics.RecordSSEConnectionClosed()
	metrics.RecordRequest(model, backend.Name, resp.StatusCode, time.Since(start), totalBytes)
	log.Printf("[SSE #%d] %s → %s %d (%.1fs, %d bytes)",
		reqID, r.URL.Path, backend.Name, resp.StatusCode,
		time.Since(start).Seconds(), totalBytes)
}

// streamWatchdog 流式超时守卫。
//
// 为什么不用 http.Client 的 ResponseHeaderTimeout 代替？
// ResponseHeaderTimeout 只覆盖「等首字节」这一段，管不了「吐了一半卡死」；
// 空闲超时从发请求起算、每次收到数据就重置，两种故障一并覆盖，只需一个配置项。
//
// progress 每来一次信号表示「刚有一批数据流动」，重置空闲计时器；
// 空闲或总时长超时时，用 cancel 打断阻塞中的上游 Read（体现在 ctx 的 cause 上）。
func (p *Proxy) streamWatchdog(reqID uint64, ctx context.Context, cancel context.CancelCauseFunc, progress <-chan struct{}) {
	var idle *time.Timer
	var idleC <-chan time.Time
	if p.streamIdleTimeout > 0 {
		idle = time.NewTimer(p.streamIdleTimeout)
		idleC = idle.C
		defer idle.Stop()
	}

	var max *time.Timer
	var maxC <-chan time.Time
	if p.streamMaxDuration > 0 {
		max = time.NewTimer(p.streamMaxDuration)
		maxC = max.C
		defer max.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return // 流已结束（正常结束或客户端断开），守卫退出
		case <-idleC:
			log.Printf("[SSE #%d] 空闲 %v 内未收到后端数据，主动断开", reqID, p.streamIdleTimeout)
			metrics.RecordGatewayError("stream_idle_timeout")
			cancel(errStreamTimeout)
			return
		case <-maxC:
			log.Printf("[SSE #%d] 单条流超过总时长上限 %v，主动断开", reqID, p.streamMaxDuration)
			metrics.RecordGatewayError("stream_max_duration")
			cancel(errStreamTimeout)
			return
		case <-progress:
			if idle != nil {
				// Stop 返回 false 说明定时器已触发且值未被取走，必须先排空 channel 再 Reset，
				// 否则残留的到期值会立刻触发下一次 select，导致误杀。
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(p.streamIdleTimeout)
			}
		}
	}
}

// ============================================================================
// 管理端点
// ============================================================================

// HealthHandler 网关自身健康检查
func (p *Proxy) HealthHandler(w http.ResponseWriter, r *http.Request) {
	healthy := p.router.HealthyCount()
	total := p.router.TotalCount()

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
	backends := p.router.AllBackends()

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
			Latency: float64(b.Latency.Load()) / 1e6, // 纳秒 → 毫秒
		})
	}
	json.NewEncoder(w).Encode(result)
}

// MetricsHandler Prometheus 格式指标输出
func (p *Proxy) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	metrics.WriteMetrics(w, p.router.Pools())
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
