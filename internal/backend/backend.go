package backend

import (
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Backend 表示一个 vLLM 推理后端实例
type Backend struct {
	Name    string        // 标识名，如 "vllm-4080"
	URL     string        // 后端地址，如 "http://10.0.0.5:8000"
	Healthy atomic.Bool   // 当前健康状态（原子操作，无锁读）
	Latency time.Duration // 最近一次健康检查延迟（纳秒）

	healthInterval time.Duration
	client         *http.Client
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

// NewBackend 创建一个后端实例
func NewBackend(name, url string, healthInterval, requestTimeout time.Duration) *Backend {
	return &Backend{
		Name:           name,
		URL:            url,
		Healthy:        atomic.Bool{},
		healthInterval: healthInterval,
		client: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  false,
			},
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
	resp, err := b.client.Get(b.URL + "/health")
	duration := time.Since(start)

	if err != nil {
		if b.Healthy.Swap(false) {
			log.Printf("[健康检查] %s (%s) → 不健康: %v", b.Name, b.URL, err)
		}
		return
	}
	resp.Body.Close()

	b.Latency = duration
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
