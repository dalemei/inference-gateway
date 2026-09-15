package backend

import (
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Backend 表示一个推理后端实例（vLLM / Ollama / TGI / SGLang 等）
type Backend struct {
	Name    string      // 标识名，如 "vllm-4080"
	URL     string      // 后端地址，如 "http://10.0.0.5:8000"
	Healthy atomic.Bool // 当前健康状态（原子操作，无锁读）
	// Latency 最近一次健康检查延迟（纳秒）。
	// 必须是原子类型：健康检查 goroutine 写、/metrics 与 /backends 读，
	// 并发访问下普通 time.Duration 是 data race（go test -race 会直接报错）。
	Latency atomic.Int64

	healthPath     string        // 健康检查路径，各家引擎约定不一，故可配置
	healthInterval time.Duration
	client         *http.Client // 转发请求用，超时 = 网关请求超时
	healthClient   *http.Client // 健康探测用，独立短超时
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

// NewBackend 创建一个后端实例
// healthPath 为空时回退为 "/health"；healthTimeout 为空（<=0）时回退为 3s
func NewBackend(name, url, healthPath string, healthInterval, healthTimeout, requestTimeout time.Duration) *Backend {
	if healthPath == "" {
		healthPath = "/health"
	}
	if healthTimeout <= 0 {
		healthTimeout = 3 * time.Second
	}
	newTransport := func() *http.Transport {
		return &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10, // 默认是 2，网关场景必须调大（理由见 proxy.go）
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  false,
		}
	}
	return &Backend{
		Name:           name,
		URL:            url,
		Healthy:        atomic.Bool{},
		healthPath:     healthPath,
		healthInterval: healthInterval,
		client: &http.Client{
			Timeout:   requestTimeout,
			Transport: newTransport(),
		},
		// 健康探测必须用独立短超时：若复用网关请求超时（可达 120s），
		// 一个卡死的后端会让探测 goroutine 长期挂起，健康状态无法及时翻转。
		healthClient: &http.Client{
			Timeout:   healthTimeout,
			Transport: newTransport(),
		},
		stopCh: make(chan struct{}),
	}
}

// StartHealthCheck 启动后台健康检查 goroutine
func (b *Backend) StartHealthCheck() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		// 启动时立即检查一次
		b.check()
		ticker := time.NewTicker(b.healthInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.check()
			case <-b.stopCh:
				return
			}
		}
	}()
}

// Stop 停止健康检查
func (b *Backend) Stop() {
	close(b.stopCh)
	b.wg.Wait()
}

// check 执行一次健康探测
func (b *Backend) check() {
	start := time.Now()
	resp, err := b.healthClient.Get(b.URL + b.healthPath)
	duration := time.Since(start)

	if err != nil {
		if b.Healthy.Swap(false) {
			log.Printf("[健康检查] %s (%s) → 不健康: %v", b.Name, b.URL, err)
		}
		return
	}
	// 必须先读完 body 再 Close：只 Close 不读，连接无法归还连接池，
	// 每次探测都要新建 TCP，长期运行会积累大量 TIME_WAIT 连接。
	// 用 LimitReader 兜底：异常后端可能返回巨大响应体，不能无上限读。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()

	b.Latency.Store(int64(duration))
	if resp.StatusCode == http.StatusOK {
		if !b.Healthy.Swap(true) {
			log.Printf("[健康检查] %s (%s) → 恢复健康 (延迟: %v)", b.Name, b.URL, duration)
		}
	} else {
		if b.Healthy.Swap(false) {
			log.Printf("[健康检查] %s (%s) → 不健康 (HTTP %d)", b.Name, b.URL, resp.StatusCode)
		}
	}
}

// IsHealthy 返回当前健康状态
func (b *Backend) IsHealthy() bool {
	return b.Healthy.Load()
}

// ============================================================================
// BackendPool — 后端池，管理多后端 + 轮询负载均衡
// ============================================================================

// BackendPool 后端实例池
type BackendPool struct {
	mu       sync.RWMutex
	backends []*Backend
	next     int // round-robin 索引
}

// NewBackendPool 创建后端池
func NewBackendPool() *BackendPool {
	return &BackendPool{}
}

// Add 添加一个后端到池中
func (p *BackendPool) Add(b *Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = append(p.backends, b)
}

// Next 按 round-robin 返回下一个健康的后端，跳过不健康的
// 返回 nil 表示所有后端都不健康
func (p *BackendPool) Next() *Backend {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.backends) == 0 {
		return nil
	}

	// 从当前位置开始遍历一圈，找第一个健康的后端
	for i := 0; i < len(p.backends); i++ {
		idx := (p.next + i) % len(p.backends)
		b := p.backends[idx]
		if b.IsHealthy() {
			p.next = (idx + 1) % len(p.backends) // 下次从下一个开始
			return b
		}
	}

	return nil // 所有后端都不健康
}

// Backends 返回所有后端（只读快照）
func (p *BackendPool) Backends() []*Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*Backend, len(p.backends))
	copy(result, p.backends)
	return result
}

// HealthyCount 返回健康后端数量
func (p *BackendPool) HealthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, b := range p.backends {
		if b.IsHealthy() {
			count++
		}
	}
	return count
}

// NextExcluding 返回下一个健康的后端，跳过 excluded 集合里的后端
// excluded 是后端 Name 的集合，用于重试时避免再次命中已失败的后端
func (p *BackendPool) NextExcluding(excluded map[string]bool) *Backend {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.backends) == 0 {
		return nil
	}

	// 遍历两圈：第一圈排除 excluded，第二圈找不到时放宽松（只找任意健康的）
	for i := 0; i < len(p.backends); i++ {
		idx := (p.next + i) % len(p.backends)
		b := p.backends[idx]
		if b.IsHealthy() && !excluded[b.Name] {
			p.next = (idx + 1) % len(p.backends)
			return b
		}
	}

	// 如果排除后找不到，退一步：返回任意健康后端（即使被排除过）
	for i := 0; i < len(p.backends); i++ {
		idx := (p.next + i) % len(p.backends)
		b := p.backends[idx]
		if b.IsHealthy() {
			p.next = (idx + 1) % len(p.backends)
			return b
		}
	}

	return nil
}

// StopAll 停止所有后端的健康检查
func (p *BackendPool) StopAll() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, b := range p.backends {
		b.Stop()
	}
}
