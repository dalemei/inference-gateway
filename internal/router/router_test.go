package router

import (
	"testing"
	"time"

	"github.com/dalemei/inference-gateway/internal/backend"
)

// newTestBackend 构造不启动健康检查的后端，健康状态直接注入。
// URL 指向必然连不上的地址也无妨 —— 这里从不发起真实探测。
func newTestBackend(name string, healthy bool) *backend.Backend {
	b := backend.NewBackend(name, "http://127.0.0.1:1", "/health",
		time.Second, time.Second, time.Second)
	b.Healthy.Store(healthy)
	return b
}

func TestRouteUsesMappedPool(t *testing.T) {
	r := NewModelRouter("default")
	def := r.RegisterPool("default")
	ollama := r.RegisterPool("ollama")

	if err := r.MapModel("qwen2.5:1.5b", "ollama"); err != nil {
		t.Fatalf("MapModel 失败: %v", err)
	}

	got := r.Route("qwen2.5:1.5b")
	if got != ollama {
		t.Fatal("已映射的 model 应路由到指定池，实际落到了别的池")
	}
	if got == def {
		t.Fatal("已映射的 model 不应再回退到默认池")
	}
}

func TestRouteFallsBackToDefaultPool(t *testing.T) {
	r := NewModelRouter("default")
	def := r.RegisterPool("default")
	r.RegisterPool("ollama") // 存在但未被任何 model 映射

	if got := r.Route("unknown-model"); got != def {
		t.Fatal("未映射的 model 应回退到默认池")
	}
}

// TestRouteReturnsNilWhenDefaultPoolNotRegistered 是网关返 503 那个分支的前提。
//
// 配置里写了 defaultPool 名字、却从没 RegisterPool 过（配置与代码不同步的典型后果），
// r.pools[name] 会拿到 map 的零值 nil。Route 必须如实返回 nil，
// 让 Proxy 走 writeError(503) —— 若这里返回一个空池，请求会被转发到「零个后端」，
// 症状变成更难排查的空指针或莫名 502。
func TestRouteReturnsNilWhenDefaultPoolNotRegistered(t *testing.T) {
	r := NewModelRouter("ghost") // 声明了默认池名，但从未注册

	if got := r.Route("any-model"); got != nil {
		t.Fatalf("默认池未注册时 Route 应返回 nil，实际返回了非 nil 池")
	}
}

// TestRegisterPoolIsIdempotent 覆盖 RegisterPool 的复用语义。
// 若重复注册返回新池，先前加进去的后端就凭空消失了 ——
// 这种丢失在配置加载阶段很隐蔽（不报错、只是后端数为 0）。
func TestRegisterPoolIsIdempotent(t *testing.T) {
	r := NewModelRouter("default")

	p1 := r.RegisterPool("default")
	p1.Add(newTestBackend("a", true))

	p2 := r.RegisterPool("default")
	if p1 != p2 {
		t.Fatal("重复注册同名池应返回同一个池实例，实际返回了新池（会丢后端）")
	}
	if n := len(p2.Backends()); n != 1 {
		t.Fatalf("复用池内应仍保留先前添加的后端，实际 %d 个", n)
	}
}

func TestMapModelRejectsUnknownPool(t *testing.T) {
	r := NewModelRouter("default")
	r.RegisterPool("default")

	if err := r.MapModel("qwen", "ghost"); err == nil {
		t.Fatal("映射到不存在的池应当报错，实际静默成功（配置错误会被掩盖）")
	}
}

func TestCountsAcrossPools(t *testing.T) {
	r := NewModelRouter("default")
	def := r.RegisterPool("default")
	def.Add(newTestBackend("d-up", true))
	def.Add(newTestBackend("d-down", false))

	ollama := r.RegisterPool("ollama")
	ollama.Add(newTestBackend("o-up", true))

	if got := r.HealthyCount(); got != 2 {
		t.Errorf("HealthyCount = %d，期望 2（跨池统计，不健康的不计）", got)
	}
	if got := r.TotalCount(); got != 3 {
		t.Errorf("TotalCount = %d，期望 3（含不健康的）", got)
	}
}

func TestAllBackendsSpansPools(t *testing.T) {
	r := NewModelRouter("default")
	r.RegisterPool("default").Add(newTestBackend("d1", true))
	r.RegisterPool("ollama").Add(newTestBackend("o1", true))

	got := r.AllBackends()
	if len(got) != 2 {
		t.Fatalf("AllBackends = %d 个，期望 2（应跨越所有池）", len(got))
	}

	names := map[string]bool{}
	for _, b := range got {
		names[b.Name] = true
	}
	for _, want := range []string{"d1", "o1"} {
		if !names[want] {
			t.Errorf("AllBackends 缺少 %s", want)
		}
	}
}

// TestPoolsSnapshotIsDetached 覆盖快照语义：Pools() 返回的是切片副本，
// 调用方拿到的列表不应被后续的 RegisterPool 改动。
// /metrics 与 /backends 就是基于这个快照渲染的，遍历途中被改动会直接 panic。
func TestPoolsSnapshotIsDetached(t *testing.T) {
	r := NewModelRouter("default")
	r.RegisterPool("default")

	snap := r.Pools()
	if len(snap) != 1 {
		t.Fatalf("前置条件：快照应有 1 个池，实际 %d", len(snap))
	}

	r.RegisterPool("second")
	if len(snap) != 1 {
		t.Fatalf("快照被后续注册污染：%d 个（应为 1）", len(snap))
	}
	if len(r.Pools()) != 2 {
		t.Fatalf("注册后 Pools() 应为 2 个，实际 %d", len(r.Pools()))
	}
}

// TestRouteConcurrent 并发下路由读取不能 panic / 不能 data race。
// 写操作（注册池、映射）与读操作（Route）同时发生时，靠的是 ModelRouter 的读写锁。
// 需要 `go test -race` 才有完整意义（本机需 gcc，见 README/记忆）。
func TestRouteConcurrent(t *testing.T) {
	r := NewModelRouter("default")
	r.RegisterPool("default").Add(newTestBackend("d1", true))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = r.Route("whatever")
			_ = r.HealthyCount()
			_ = r.Pools()
		}
	}()

	for i := 0; i < 50; i++ {
		p := r.RegisterPool("pool-" + string(rune('a'+i%5)))
		p.Add(newTestBackend("b"+string(rune('a'+i%5)), true))
	}

	<-done
	if r.TotalCount() == 0 {
		t.Fatal("并发注册后 TotalCount 为 0，后端丢失")
	}
}
