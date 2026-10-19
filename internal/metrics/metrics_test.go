package metrics

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dalemei/inference-gateway/internal/backend"
	"github.com/dalemei/inference-gateway/internal/usage"
)

// ============================================================================
// 测试夹具
// ============================================================================

// resetForTest 清空包级指标状态。
//
// 全部指标都挂在包级全局变量上，用例之间会互相累加。只靠「增量断言」是撑不住的：
// 本文件最核心的几类断言（某些类型必须输出 0 基线、某些序列必须**不**出现）
// 根本没法用增量表达 —— 已在上一用例里被累加过的状态会让它们永远假通过。
func resetForTest(t *testing.T) {
	t.Helper()
	metricsMu.Lock()
	defer metricsMu.Unlock()

	requestCount = make(map[string]int64)
	requestLatency = make(map[string]float64)
	errorCount = make(map[string]int64)
	authFailureCount = make(map[string]int64)
	keyRequestCount = make(map[string]int64)
	keyThrottledCount = make(map[string]int64)
	tokenCount = make(map[string]int64)
	usageMissing = make(map[string]int64)

	totalRequests.Store(0)
	totalErrors.Store(0)
	sseConnectionsActive.Store(0)
	sseConnectionsTotal.Store(0)
	retryCount.Store(0)
}

func newTestBackend(name string, healthy bool) *backend.Backend {
	b := backend.NewBackend(name, "http://127.0.0.1:1", "/health",
		time.Second, time.Second, time.Second)
	b.Healthy.Store(healthy)
	return b
}

// scrape 抓一次 /metrics 文本。
func scrape(t *testing.T, pools ...*backend.BackendPool) string {
	t.Helper()
	var buf bytes.Buffer
	WriteMetrics(&buf, pools)
	return buf.String()
}

// stripUptime 去掉 uptime 行 —— 它每次抓取都在变，会干扰"输出是否稳定"的比较。
func stripUptime(out string) string {
	kept := make([]string, 0)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "inference_gateway_uptime_seconds") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func mustContain(t *testing.T, out, want string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("输出中缺少这一行：\n  期望: %s\n  实际输出:\n%s", want, out)
}

// ============================================================================
// 基线留痕：为 0 也必须输出，否则「从未发生」与「埋点失效」长得一模一样
// ============================================================================

func TestKnownErrorTypesExposedEvenWhenZero(t *testing.T) {
	resetForTest(t)
	out := scrape(t)

	// 这 7 个是告警规则会直接引用的标签组合。若只在非零时输出，
	// 规则里的 absent() 判断就会在「刚启动还没出过错」时误报。
	for _, typ := range []string{
		"no_healthy_backend", "backend_unreachable", "all_retries_failed",
		"retryable_status", "request_body_too_large",
		"stream_idle_timeout", "stream_max_duration",
	} {
		mustContain(t, out, fmt.Sprintf(`inference_gateway_errors_total{type="%s"} 0`, typ))
	}
}

// TestRuntimeErrorTypeStillExposed 是「埋点废点」缺陷的回归测试。
//
// 修复前 errors_total 只写死输出已知类型，运行期新增的类型被累加进 map 却永不输出 ——
// 等于埋了一个看不见的点：故障发生了、计数在涨、面板上却是空的。
// 这条用例用一个刻意不在已知列表里的类型兜住它。
func TestRuntimeErrorTypeStillExposed(t *testing.T) {
	resetForTest(t)
	RecordGatewayError("brand_new_error_type")

	out := scrape(t)
	mustContain(t, out, `inference_gateway_errors_total{type="brand_new_error_type"} 1`)
	mustContain(t, out, `inference_gateway_errors_total{type="all_retries_failed"} 0`)
}

func TestAuthFailureAndUsageMissingBaselines(t *testing.T) {
	resetForTest(t)
	out := scrape(t)

	mustContain(t, out, `inference_gateway_auth_failures_total{reason="missing_key"} 0`)
	mustContain(t, out, `inference_gateway_auth_failures_total{reason="invalid_key"} 0`)
	// usage_missing 是 token 成本看板的分母健康度，三个原因同样要固定出基线
	mustContain(t, out, `inference_gateway_usage_missing_total{reason="no_usage_field"} 0`)
	mustContain(t, out, `inference_gateway_usage_missing_total{reason="response_too_large"} 0`)
	mustContain(t, out, `inference_gateway_usage_missing_total{reason="stream_aborted"} 0`)
}

// ============================================================================
// 输出稳定性：排障时要能「这次比上次多了哪一行」
// ============================================================================

// TestOutputStableAcrossScrapes：Go 的 map 遍历顺序是随机的，
// 若不排序，人工 diff 两次抓取结果会彻底失效（Prometheus 不在乎顺序，人在乎）。
func TestOutputStableAcrossScrapes(t *testing.T) {
	resetForTest(t)
	RecordRequest("qwen", "vllm-1", 200, time.Millisecond, 100)
	RecordRequest("qwen", "vllm-2", 503, time.Millisecond, 100)
	RecordGatewayError("backend_unreachable")
	RecordGatewayError("zzz_custom")

	first := stripUptime(scrape(t))
	for i := 0; i < 5; i++ {
		if got := stripUptime(scrape(t)); got != first {
			t.Fatalf("第 %d 次抓取与首次不一致 —— 指标行顺序未固定，人工 diff 会失效", i+2)
		}
	}
}

// ============================================================================
// 计数器累加与维度切分
// ============================================================================

// TestRequestMetricsAreSortedByDimension 是「两族指标漏排序」缺陷的回归测试。
//
// 实测（抓 30 次比对行号）发现 inference_gateway_requests_total 与
// inference_gateway_backend_request_latency_seconds 直接 range map 输出，
// 顺序逐次随机 —— 而 sortedKeysOf 的注释明明写着「排障时『这次比上次多了哪一行』
// 是关键信息，故统一排序」。这两族恰恰是序列数最多的一族
// （model × backend × status_code），乱序对人工 diff 的影响最大。
//
// 该缺陷是概率性复现的：单跑一个包可能碰巧通过，全量跑才暴露，
// 所以这条用例必须显式多次抓取并对齐，不能只抓一次。
func TestRequestMetricsAreSortedByDimension(t *testing.T) {
	resetForTest(t)
	// 刻意乱序写入，排序正确性不该依赖写入顺序
	RecordRequest("qwen", "vllm-2", 200, time.Millisecond, 10)
	RecordRequest("qwen", "vllm-1", 200, time.Millisecond, 10)
	RecordRequest("qwen", "vllm-3", 200, time.Millisecond, 10)

	var wantRequests, wantLatency []string
	for i := 0; i < 20; i++ {
		out := scrape(t)
		gotRequests := linesWithPrefix(out, "inference_gateway_requests_total{")
		gotLatency := linesWithPrefix(out, "inference_gateway_backend_request_latency_seconds{")

		if len(gotRequests) != 3 {
			t.Fatalf("requests_total 应有 3 行，实际 %d", len(gotRequests))
		}
		if i == 0 {
			wantRequests, wantLatency = gotRequests, gotLatency
			continue
		}
		if !equalStrings(gotRequests, wantRequests) {
			t.Fatalf("第 %d 次抓取的 requests_total 行序与首次不同：\n%v\n%v",
				i+1, gotRequests, wantRequests)
		}
		if !equalStrings(gotLatency, wantLatency) {
			t.Fatalf("第 %d 次抓取的 latency 行序与首次不同：\n%v\n%v",
				i+1, gotLatency, wantLatency)
		}
	}

	// 不止要稳定，还必须是字典序（这样人眼定位某个 backend 的位置是固定的）
	if !sort.StringsAreSorted(wantRequests) {
		t.Errorf("requests_total 未按字典序输出：%v", wantRequests)
	}
	if !sort.StringsAreSorted(wantLatency) {
		t.Errorf("backend_request_latency_seconds 未按字典序输出：%v", wantLatency)
	}
}

func linesWithPrefix(out, prefix string) []string {
	got := make([]string, 0)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			got = append(got, line)
		}
	}
	return got
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRecordRequestAggregatesByDimensions(t *testing.T) {
	resetForTest(t)
	RecordRequest("qwen", "vllm-1", 200, time.Millisecond, 100)
	RecordRequest("qwen", "vllm-1", 200, time.Millisecond, 100)
	RecordRequest("qwen", "vllm-2", 500, time.Millisecond, 100)

	out := scrape(t)
	mustContain(t, out, `inference_gateway_requests_total{model="qwen",backend="vllm-1",status_code="200"} 2`)
	mustContain(t, out, `inference_gateway_requests_total{model="qwen",backend="vllm-2",status_code="500"} 1`)
}

// TestRecordTokensRespectsHasCachedFlag 钉住一个容易被"顺手改坏"的设计决策。
//
// cached 序列的输出条件是「后端上报了该字段（HasCached）」，而不是「值 > 0」。
// 若按 > 0 输出，「这次真没命中（0）」与「后端压根没这个能力（也是 0 或缺失）」
// 都会表现为序列不存在 —— 缓存命中率就永远分不清「0%」和「未观测」。
func TestRecordTokensRespectsHasCachedFlag(t *testing.T) {
	resetForTest(t)
	RecordTokens("m", "b", "k", usage.Usage{PromptTokens: 100, CompletionTokens: 50})

	out := scrape(t)
	mustContain(t, out, `inference_gateway_prompt_tokens_total{model="m",backend="b",key="k"} 100`)
	mustContain(t, out, `inference_gateway_completion_tokens_total{model="m",backend="b",key="k"} 50`)
	if strings.Contains(out, `inference_gateway_cached_tokens_total{model="m"`) {
		t.Fatal("HasCached=false 时不应产生 cached 序列，否则 0% 命中与未观测无法区分")
	}

	// 后端上报了、但本次确实一 token 都没命中：序列必须存在且为 0
	resetForTest(t)
	RecordTokens("m", "b", "k", usage.Usage{
		PromptTokens: 100, CompletionTokens: 50, CachedTokens: 0, HasCached: true,
	})
	mustContain(t, scrape(t), `inference_gateway_cached_tokens_total{model="m",backend="b",key="k"} 0`)
}

// TestAnonymousKeyFallback：未启用鉴权时 token 指标的 key 标签用固定字面量
// "anonymous" 而不是空串 —— 空标签在 PromQL 里写起来别扭，且容易被读成「标签没打上」。
func TestAnonymousKeyFallback(t *testing.T) {
	resetForTest(t)
	RecordTokens("m", "b", "", usage.Usage{PromptTokens: 10})

	mustContain(t, scrape(t), `inference_gateway_prompt_tokens_total{model="m",backend="b",key="anonymous"} 10`)
}

func TestUsageMissingAccumulatesByReason(t *testing.T) {
	resetForTest(t)
	RecordUsageMissing("stream_aborted")
	RecordUsageMissing("stream_aborted")
	RecordUsageMissing("no_usage_field")

	out := scrape(t)
	mustContain(t, out, `inference_gateway_usage_missing_total{reason="stream_aborted"} 2`)
	mustContain(t, out, `inference_gateway_usage_missing_total{reason="no_usage_field"} 1`)
}

func TestSSEConnectionGaugeTracksActive(t *testing.T) {
	resetForTest(t)
	RecordSSEConnection()
	RecordSSEConnection()
	RecordSSEConnectionClosed()

	out := scrape(t)
	// active 是 gauge，建立 +1 关闭 -1；total 是 counter，只增不减
	mustContain(t, out, "inference_gateway_sse_connections_active 1")
	mustContain(t, out, "inference_gateway_sse_connections_total 2")
}

func TestBackendHealthGaugeFromPools(t *testing.T) {
	resetForTest(t)
	pool := backend.NewBackendPool()
	pool.Add(newTestBackend("up", true))
	pool.Add(newTestBackend("down", false))

	out := scrape(t, pool)
	mustContain(t, out, `inference_gateway_backend_health{backend="up",url="http://127.0.0.1:1"} 1`)
	mustContain(t, out, `inference_gateway_backend_health{backend="down",url="http://127.0.0.1:1"} 0`)
}
