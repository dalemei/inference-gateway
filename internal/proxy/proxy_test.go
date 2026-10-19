package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"github.com/dalemei/inference-gateway/internal/backend"
	"github.com/dalemei/inference-gateway/internal/router"
)

// newFakeBackend 构造一个指向 httptest 服务、但不启动健康检查的后端。
// 健康状态直接注入：健康检查 goroutine 会真的去探测 URL，而测试里路由该落到谁
// 是我们自己说了算的，不能被探测结果干扰。
func newFakeBackend(name, url string, healthy bool) *backend.Backend {
	b := backend.NewBackend(name, url, "/health",
		time.Second, time.Second, 5*time.Second)
	b.Healthy.Store(healthy)
	return b
}

// newTestProxy 构造一个默认池的 Proxy，返回池以便注入假后端。
func newTestProxy(t *testing.T, opt Options) (*Proxy, *backend.BackendPool) {
	t.Helper()
	r := router.NewModelRouter("default")
	pool := r.RegisterPool("default")
	return NewProxy(r, opt), pool
}

// ============================================================================
// 纯函数组：无 IO，毫秒级，先跑这层能挡住绝大多数逻辑回归
// ============================================================================

func TestIsInferencePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/chat/completions", true},
		{"/v1/completions", true},
		{"/v1/embeddings", true},
		// 元数据接口没有 usage 是「本来如此」，不该被记成 usage 缺失污染指标
		{"/v1/models", false},
		{"/health", false},
		// 是后缀匹配而非包含匹配：子路径不算推理端点
		{"/v1/chat/completions/extra", false},
	}
	for _, c := range cases {
		if got := isInferencePath(c.path); got != c.want {
			t.Errorf("isInferencePath(%q) = %v，期望 %v", c.path, got, c.want)
		}
	}
}

// TestIsRetryableStatus 是「后端 5xx 不重试」缺陷的回归测试。
//
// 修复前只有连接错误才重试，后端返回 503 时网关直接把它透传给了客户端 ——
// 而 503（模型加载中/过载）恰恰是推理集群最常见的瞬时故障，本可以在网关层吸收掉。
//
// 500 刻意不重试：它多半是请求自身触发的（prompt 超长、参数非法），换节点也白搭。
// 4xx 同理。这条边界必须钉住，否则后来者会顺手把 500 加进去。
func TestIsRetryableStatus(t *testing.T) {
	for _, code := range []int{
		http.StatusTooManyRequests,    // 429
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout,     // 504
	} {
		if !isRetryableStatus(code) {
			t.Errorf("%d 应可重试（后端过载/加载中），实际判为不可重试", code)
		}
	}

	for _, code := range []int{
		http.StatusBadRequest,            // 400
		http.StatusUnauthorized,          // 401
		http.StatusNotFound,              // 404
		http.StatusRequestEntityTooLarge, // 413
		http.StatusInternalServerError,   // 500：请求自身问题，换节点无用
		http.StatusOK,                    // 200
	} {
		if isRetryableStatus(code) {
			t.Errorf("%d 不应重试，实际判为可重试", code)
		}
	}
}

func TestDetectStreaming(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"流式", `{"model":"m","stream":true}`, true},
		{"非流式", `{"model":"m","stream":false}`, false},
		{"未声明", `{"model":"m"}`, false},
		// 非法 JSON 不能被当成流式：宁可走非流式让后端报错，也不要猜
		{"非法 JSON", `not-json`, false},
	}
	for _, c := range cases {
		if got := detectStreaming([]byte(c.body)); got != c.want {
			t.Errorf("%s: detectStreaming = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestExtractModel(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"正常", `{"model":"qwen2.5:1.5b"}`, "qwen2.5:1.5b"},
		{"缺字段", `{"stream":true}`, ""},
		{"非法 JSON", `{{{`, ""},
	}
	for _, c := range cases {
		if got := extractModel([]byte(c.body)); got != c.want {
			t.Errorf("%s: extractModel = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// encodeGBK 用真实 GBK 编码器生成字节，模拟 Windows 终端发出来的中文。
func encodeGBK(t *testing.T, s string) []byte {
	t.Helper()
	out, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(s))
	if err != nil {
		t.Fatalf("GBK 编码失败: %v", err)
	}
	return out
}

func TestEnsureUTF8ConvertsGBK(t *testing.T) {
	const want = "你好，世界"
	gbk := encodeGBK(t, want)
	if utf8.Valid(gbk) {
		t.Fatal("测试前提不成立：GBK 字节不应是有效 UTF-8")
	}

	out, converted := ensureUTF8(gbk)
	if !converted {
		t.Fatal("GBK 内容应被转换，实际未转换")
	}
	if string(out) != want {
		t.Fatalf("转换结果 = %q，期望 %q", string(out), want)
	}
	if !utf8.Valid(out) {
		t.Fatal("转换结果必须是有效 UTF-8")
	}
}

func TestEnsureUTF8LeavesValidUTF8Untouched(t *testing.T) {
	in := []byte(`{"model":"qwen","prompt":"你好"}`) // 本身就是 UTF-8

	out, converted := ensureUTF8(in)
	if converted {
		t.Fatal("已是 UTF-8 的内容不应被标记转换")
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("内容被改动: %q", out)
	}
}

// TestEnsureUTF8KeepsJSONParsable 覆盖真实场景：字段名是 ASCII、中文值是 GBK，
// 整体不是合法 UTF-8，转换后必须仍然是能被后端解析的合法 JSON。
// 这个用例的价值在于守住「转换不能破坏结构」——只转中文值、不碰 JSON 语法字符。
func TestEnsureUTF8KeepsJSONParsable(t *testing.T) {
	gbkName := encodeGBK(t, "你好")
	body := []byte(`{"model":"qwen","messages":[{"role":"user","content":"` + string(gbkName) + `"}]}`)

	out, converted := ensureUTF8(body)
	if !converted {
		t.Fatal("含 GBK 中文的请求体应被转换")
	}
	if !json.Valid(out) {
		t.Fatalf("转换后不再是合法 JSON: %s", out)
	}

	var parsed struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("转换后无法解析: %v", err)
	}
	if len(parsed.Messages) != 1 || parsed.Messages[0].Content != "你好" {
		t.Fatalf("解析出的中文 = %q，期望 %q", parsed.Messages[0].Content, "你好")
	}
}

// TestCopyHeadersExcludesHostAndContentLength 覆盖转发头清洗。
// Content-Length 必须排除：网关会按（可能被编码转换/inject 改写过的）body 重算，
// 沿用客户端那个旧值会让后端收到长度不符的请求而解析失败。
func TestCopyHeadersExcludesHostAndContentLength(t *testing.T) {
	src := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	src.Header.Set("Host", "gateway.local")
	src.Header.Set("Content-Length", "2")
	src.Header.Set("Authorization", "Bearer sk-test")

	dst := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	copyHeaders(dst, src)

	if dst.Header.Get("Host") != "" {
		t.Error("Host 不应被复制")
	}
	if dst.Header.Get("Content-Length") != "" {
		t.Error("Content-Length 不应被复制，应由网关按实际 body 重算")
	}
	if dst.Header.Get("Authorization") != "Bearer sk-test" {
		t.Error("Authorization 应被透传，否则后端鉴权会失败")
	}
}

func TestTriedKeys(t *testing.T) {
	if got := triedKeys(map[string]bool{}); got != "" {
		t.Errorf("空集合应返回空串，实际 %q", got)
	}
	got := triedKeys(map[string]bool{"a": true, "b": true})
	// map 遍历顺序不定，按集合比较
	for _, want := range []string{"a", "b"} {
		if !strings.Contains(got, want) {
			t.Errorf("triedKeys = %q，缺少 %s", got, want)
		}
	}
}

// ============================================================================
// 行为组：用 httptest 假后端验证转发、重试、限流
// ============================================================================

// TestHandleRequestSwitchesBackendOnRetryableStatus 是重试链路的核心回归：
// 一个节点返回 503，网关必须换到另一个节点把请求救回来，而不是把 503 透传给客户端。
//
// 发两次请求是为了覆盖两种首选分支：round-robin 下第一次首选 A、第二次首选 B，
// 两种情况最终结果都必须是 200 —— 否则「只有命中坏节点才重试」的偶然性会掩盖缺陷。
func TestHandleRequestSwitchesBackendOnRetryableStatus(t *testing.T) {
	var hits = map[string]int{}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits["bad"]++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits["good"]++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer good.Close()

	p, pool := newTestProxy(t, Options{Timeout: 2 * time.Second, MaxRetries: 1})
	pool.Add(newFakeBackend("bad", bad.URL, true))
	pool.Add(newFakeBackend("good", good.URL, true))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m"}`))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求 = %d，期望 200（重试应把请求救回来），body=%q",
				i+1, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Fatalf("第 %d 次响应体 = %q，应来自健康节点", i+1, rec.Body.String())
		}
	}

	if hits["bad"] == 0 {
		t.Fatal("故障节点一次都没被命中，测试用例没有真正走到重试分支")
	}
	if hits["good"] == 0 {
		t.Fatal("健康节点一次都没被命中，重试没有生效")
	}
}

// TestHandleRequestPassesThroughLastStatusAfterRetriesExhausted 钉住重试耗尽后的状态码语义。
//
// 这里期望的是**后端原始的 503**，不是 502。这是刻意的行为，理由：
// 重试耗尽意味着「试过 N 个节点，后端亲口说它不可用」，把这个 503 原样回给客户端，
// 客户端才知道下游在过载 —— 换成 502 会把「后端不可用」和「网关路由出问题」混为一谈。
// Envoy/Nginx 同样是透传最后的响应。
//
// 注意连接错误耗尽走的是另一条路径，返回 502（因为没有后端响应可供透传）。
// 两条路径状态码不同是有意为之，不是笔误 —— 若将来要统一，改这里而不是改 handleRequest。
func TestHandleRequestPassesThroughLastStatusAfterRetriesExhausted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "real")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer up.Close()

	p, pool := newTestProxy(t, Options{Timeout: 2 * time.Second, MaxRetries: 2})
	pool.Add(newFakeBackend("a", up.URL, true))
	pool.Add(newFakeBackend("b", up.URL, true))

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("重试耗尽后应透传后端原始 503，实际 %d", rec.Code)
	}
	// 响应头也必须透传：网关不该在错误路径上悄悄丢掉后端给的信息
	if got := rec.Header().Get("X-Upstream"); got != "real" {
		t.Errorf("后端响应头未透传，X-Upstream = %q", got)
	}
}

// TestHandleRequestNoHealthyBackendReturns503：池内后端全不健康时，
// Next() 会直接返回 nil，请求根本不该发出去。
func TestHandleRequestNoHealthyBackendReturns503(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应向不健康的后端发起请求")
	}))
	defer up.Close()

	p, pool := newTestProxy(t, Options{Timeout: 2 * time.Second, MaxRetries: 1})
	pool.Add(newFakeBackend("down", up.URL, false))

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无健康后端时应返回 503，实际 %d", rec.Code)
	}
}

// TestRequestBodyTooLargeReturns413 是大请求体闸门。
// Go 默认不限制 body 大小，没有这道闸任何人都能 POST 一个 GB 级 body 打满网关内存。
// 超限时必须 413，且不把超限内容读进内存。
func TestRequestBodyTooLargeReturns413(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("超限请求不应被转发到后端")
	}))
	defer up.Close()

	p, pool := newTestProxy(t, Options{
		Timeout:      2 * time.Second,
		MaxBodyBytes: 128,
	})
	pool.Add(newFakeBackend("a", up.URL, true))

	body := strings.Repeat("x", 1024)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限时本应 413，实际 %d", rec.Code)
	}
}

// TestHandleStreamingPropagatesChunks 覆盖 SSE 逐块转发。
// 关键不只是「内容到了」，还有 Flush —— 不 Flush 的话客户端会一直等到连接结束
// 才收到数据，SSE 就退化成了普通 HTTP，ttft（首 token 延迟）指标也会失真。
func TestHandleStreamingPropagatesChunks(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":%d}]}\n\n", i)
			f.Flush()
		}
	}))
	defer up.Close()

	p, pool := newTestProxy(t, Options{Timeout: 2 * time.Second})
	pool.Add(newFakeBackend("sse", up.URL, true))

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("流式响应状态 = %d，期望 200", rec.Code)
	}
	for i := 0; i < 3; i++ {
		want := fmt.Sprintf(`{"choices":[{"index":%d}]}`, i)
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("缺少第 %d 个 chunk：%q", i, rec.Body.String())
		}
	}
	if !rec.Flushed {
		t.Error("流式响应必须逐块 Flush，否则客户端收不到增量数据")
	}
}

// TestStreamIdleTimeoutTerminatesStuckStream 覆盖流式空闲守卫。
//
// 后端「吐了一半卡死」是 SSE 最典型的故障：连接还活着，但再没有数据。
// 没有守卫的话连接会永久挂起，网关的并发连接数被慢慢吃光。
//
// 掐断后必须补发 SSE error 事件 —— 200 已发出、状态码改不了，
// 不补发的话客户端只收到一段没头没尾的截断流，无法区分「网关超时」和「流正常结束」。
func TestStreamIdleTimeoutTerminatesStuckStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // 卡住，直到客户端断开
	}))
	defer up.Close()

	p, pool := newTestProxy(t, Options{
		Timeout:           2 * time.Second,
		StreamIdleTimeout: 150 * time.Millisecond,
	})
	pool.Add(newFakeBackend("stuck", up.URL, true))

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("守卫未在 5s 内终止卡死的流 —— 空闲超时失效")
	}

	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("流被掐断后应补发 SSE error 事件，实际 body: %q", rec.Body.String())
	}
}

// TestHealthHandlerDegradesWhenAllDown 覆盖网关自身健康语义：
// 有节点但全不健康 = degraded（503）；没有任何节点时也不该谎报 healthy。
func TestHealthHandlerDegradesWhenAllDown(t *testing.T) {
	p, pool := newTestProxy(t, Options{})
	pool.Add(newFakeBackend("a", "http://127.0.0.1:1", false))

	rec := httptest.NewRecorder()
	p.HealthHandler(rec, httptest.NewRequest("GET", "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("全部后端不健康时应返回 503，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "degraded") {
		t.Fatalf("响应体应标记 degraded，实际 %q", rec.Body.String())
	}
}

func TestHealthHandlerHealthy(t *testing.T) {
	p, pool := newTestProxy(t, Options{})
	pool.Add(newFakeBackend("a", "http://127.0.0.1:1", true))

	rec := httptest.NewRecorder()
	p.HealthHandler(rec, httptest.NewRequest("GET", "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("有健康节点时应返回 200，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"healthy_backends":1`) {
		t.Fatalf("响应体应报告健康节点数，实际 %q", rec.Body.String())
	}
}
