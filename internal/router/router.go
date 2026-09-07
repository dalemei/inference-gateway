package router

import (
	"fmt"
	"sync"

	"github.com/dalemei/inference-gateway/internal/backend"
)

// ModelRouter routes inference requests to backend pools by model name.
type ModelRouter struct {
	mu          sync.RWMutex
	pools       map[string]*backend.BackendPool // pool name -> pool
	modelToPool map[string]string                // model name -> pool name
	defaultPool string
}

// NewModelRouter creates a router with the given default pool name.
func NewModelRouter(defaultPool string) *ModelRouter {
	return &ModelRouter{
		pools:       make(map[string]*backend.BackendPool),
		modelToPool: make(map[string]string),
		defaultPool: defaultPool,
	}
}

// RegisterPool creates (if absent) and returns the named pool.
func (r *ModelRouter) RegisterPool(name string) *backend.BackendPool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pools[name]; !ok {
		r.pools[name] = backend.NewBackendPool()
	}
	return r.pools[name]
}

// MapModel binds a model name to an existing pool.
func (r *ModelRouter) MapModel(model, pool string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pools[pool]; !ok {
		return fmt.Errorf("model %q maps to unknown pool %q", model, pool)
	}
	r.modelToPool[model] = pool
	return nil
}

// Route returns the pool for the given model. Falls back to the default pool
// when the model is not explicitly mapped. Returns nil only if the default
// pool is also unset (misconfiguration).
func (r *ModelRouter) Route(model string) *backend.BackendPool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if poolName, ok := r.modelToPool[model]; ok {
		return r.pools[poolName]
	}
	if r.defaultPool != "" {
		return r.pools[r.defaultPool]
	}
	return nil
}

// Pools returns all registered pools (snapshot, order undefined).
func (r *ModelRouter) Pools() []*backend.BackendPool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*backend.BackendPool, 0, len(r.pools))
	for _, p := range r.pools {
		out = append(out, p)
	}
	return out
}

// AllBackends returns every backend across all pools.
func (r *ModelRouter) AllBackends() []*backend.Backend {
	out := make([]*backend.Backend, 0)
	for _, p := range r.Pools() {
		out = append(out, p.Backends()...)
	}
	return out
}

// HealthyCount returns the total number of healthy backends.
func (r *ModelRouter) HealthyCount() int {
	c := 0
	for _, p := range r.Pools() {
		c += p.HealthyCount()
	}
	return c
}

// TotalCount returns the total number of backends.
func (r *ModelRouter) TotalCount() int {
	c := 0
	for _, p := range r.Pools() {
		c += len(p.Backends())
	}
	return c
}

// StopAll stops health checks on every pool.
func (r *ModelRouter) StopAll() {
	for _, p := range r.Pools() {
		p.StopAll()
	}
}
