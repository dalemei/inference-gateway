package backend

import (
	"sync"
	"testing"
	"time"
)

// newTestBackend 构造一个不启动健康检查的后端，健康状态直接注入。
// URL 指向一个必然连不上的地址也没关系 —— 这里从不发起真实探测。
func newTestBackend(name string, healthy bool) *Backend {
	b := NewBackend(name, "http://127.0.0.1:1", "/health",
		time.Second, time.Second, time.Second)
	b.Healthy.Store(healthy)
	return b
}

func TestNextRoundRobin(t *testing.T) {
	p := NewBackendPool()
	p.Add(newTestBackend("a", true))
	p.Add(newTestBackend("b", true))

	want := []string{"a", "b", "a", "b", "a", "b"}
	for i, w := range want {
		got := p.Next()
		if got == nil {
			t.Fatalf("第 %d 次返回 nil，期望 %s", i+1, w)
		}
		if got.Name != w {
			t.Fatalf("第 %d 次 = %s，期望 %s", i+1, got.Name, w)
		}
	}
}

// TestNextExcludingDoesNotAdvanceCursor 是 F1 的回归测试。
//
// 修复前：Next() 把游标 0→1，重试的 NextExcluding() 又 1→0，两者相抵，
// 每轮净移动为 0，于是从第二次请求起首次尝试永远落在同一个后端上，
// 轮询退化为固定节点。修复后应为严格交替。
func TestNextExcludingDoesNotAdvanceCursor(t *testing.T) {
	p := NewBackendPool()
	p.Add(newTestBackend("a", true))
	p.Add(newTestBackend("slow", true))

	var firsts []string
	for i := 0; i < 6; i++ {
		b := p.Next()
		if b == nil {
			t.Fatalf("第 %d 次 Next() 返回 nil", i+1)
		}
		firsts = append(firsts, b.Name)
		// 模拟真实链路：slow 总是失败，失败后重试换节点
		if b.Name == "slow" {
			retry := p.NextExcluding(map[string]bool{"slow": true})
			if retry == nil {
				t.Fatalf("第 %d 次重试无可用后端", i+1)
			}
			if retry.Name == "slow" {
				t.Fatalf("第 %d 次重试仍命中 slow，排除集未生效", i+1)
			}
		}
	}

	want := []string{"a", "slow", "a", "slow", "a", "slow"}
	if len(firsts) != len(want) {
		t.Fatalf("序列长度 %d，期望 %d：%v", len(firsts), len(want), firsts)
	}
	for i := range want {
		if firsts[i] != want[i] {
			t.Fatalf("首次尝试序列 = %v，期望 %v —— 轮询已退化为固定节点",
				firsts, want)
		}
	}
}

func TestNextSkipsUnhealthy(t *testing.T) {
	p := NewBackendPool()
	p.Add(newTestBackend("down", false))
	p.Add(newTestBackend("up", true))

	for i := 0; i < 4; i++ {
		got := p.Next()
		if got == nil {
			t.Fatalf("第 %d 次返回 nil，池内明明有健康后端", i+1)
		}
		if got.Name != "up" {
			t.Fatalf("第 %d 次 = %s，应跳过不健康的 down", i+1, got.Name)
		}
	}
}

func TestNextAllUnhealthyReturnsNil(t *testing.T) {
	p := NewBackendPool()
	p.Add(newTestBackend("a", false))
	p.Add(newTestBackend("b", false))

	if got := p.Next(); got != nil {
		t.Fatalf("全部不健康时应返回 nil，实际 %s", got.Name)
	}
	if got := p.NextExcluding(map[string]bool{}); got != nil {
		t.Fatalf("全部不健康时 NextExcluding 应返回 nil，实际 %s", got.Name)
	}
}

func TestNextExcludingAvoidsExcluded(t *testing.T) {
	p := NewBackendPool()
	p.Add(newTestBackend("a", true))
	p.Add(newTestBackend("b", true))

	// 连续排除同一个后端，必须每次都绕开它
	for i := 0; i < 4; i++ {
		got := p.NextExcluding(map[string]bool{"a": true})
		if got == nil {
			t.Fatalf("第 %d 次返回 nil", i+1)
		}
		if got.Name == "a" {
			t.Fatalf("第 %d 次仍返回被排除的 a", i+1)
		}
	}
}

// TestNextExcludingFallsBackWhenAllExcluded 覆盖兜底分支：
// 排除集覆盖全部后端时，宁可重试已失败的，也不能返回 nil 让请求直接失败。
func TestNextExcludingFallsBackWhenAllExcluded(t *testing.T) {
	p := NewBackendPool()
	p.Add(newTestBackend("a", true))
	p.Add(newTestBackend("b", true))

	got := p.NextExcluding(map[string]bool{"a": true, "b": true})
	if got == nil {
		t.Fatal("所有后端都被排除时应回退返回任意健康后端，实际 nil")
	}
}

func TestNextEmptyPool(t *testing.T) {
	p := NewBackendPool()
	if got := p.Next(); got != nil {
		t.Fatalf("空池应返回 nil，实际 %v", got)
	}
	if got := p.NextExcluding(map[string]bool{"x": true}); got != nil {
		t.Fatalf("空池 NextExcluding 应返回 nil，实际 %v", got)
	}
	if p.HealthyCount() != 0 {
		t.Fatalf("空池 HealthyCount = %d，期望 0", p.HealthyCount())
	}
}

// TestNextConcurrent 并发下不能 data race，且负载应大致均匀。
// 用 `go test -race` 跑才有意义。
func TestNextConcurrent(t *testing.T) {
	p := NewBackendPool()
	p.Add(newTestBackend("a", true))
	p.Add(newTestBackend("b", true))

	const goroutines, perGoroutine = 8, 50
	var mu sync.Mutex
	hit := map[string]int{}
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := map[string]int{}
			for i := 0; i < perGoroutine; i++ {
				b := p.Next()
				if b == nil {
					t.Error("并发下 Next() 返回 nil")
					return
				}
				local[b.Name]++
			}
			mu.Lock()
			for k, v := range local {
				hit[k] += v
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	total := goroutines * perGoroutine
	for _, name := range []string{"a", "b"} {
		if hit[name] == 0 {
			t.Fatalf("%s 一次都没被选中，轮询在并发下失效：%v", name, hit)
		}
		// 严格交替下应接近 50%，给并发调度留 20% 容差
		if diff := hit[name] - total/2; diff < 0 {
			if -diff > total/5 {
				t.Fatalf("%s 命中 %d/%d，偏离 50%% 超过容差", name, hit[name], total)
			}
		} else if diff > total/5 {
			t.Fatalf("%s 命中 %d/%d，偏离 50%% 超过容差", name, hit[name], total)
		}
	}
}
