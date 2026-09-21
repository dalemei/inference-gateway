package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dalemei/inference-gateway/internal/auth"
	"github.com/dalemei/inference-gateway/internal/backend"
	"github.com/dalemei/inference-gateway/internal/config"
	"github.com/dalemei/inference-gateway/internal/proxy"
	"github.com/dalemei/inference-gateway/internal/router"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	debug := flag.Bool("debug", false, "启用调试日志（打印请求头与 body，含敏感信息）")
	flag.Parse()

	// 加载配置
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	if len(cfg.Backends) == 0 {
		log.Fatal("配置文件中至少需要一个后端 (backends)")
	}

	// 解析超时
	timeout, err := time.ParseDuration(cfg.Gateway.Timeout)
	if err != nil {
		log.Printf("[WARN] 无效的超时配置 %q，使用默认 60s", cfg.Gateway.Timeout)
		timeout = 60 * time.Second
	}

	// 解析请求体上限（如 "16MB"；"0" = 不限制）
	maxBodyBytes, err := config.ParseSize(cfg.Gateway.MaxBodySize)
	if err != nil {
		log.Printf("[WARN] 无效的 max_body_size %q，使用默认 16MB: %v", cfg.Gateway.MaxBodySize, err)
		maxBodyBytes = 16 << 20
	}
	if maxBodyBytes == 0 {
		log.Println("[WARN] max_body_size = 0：请求体不做任何限制，异常客户端可打满网关内存")
	}

	// 解析流式空闲超时（"0" = 不启用）
	streamIdle, err := time.ParseDuration(cfg.Gateway.StreamIdleTimeout)
	if err != nil || streamIdle < 0 {
		log.Printf("[WARN] 无效的 stream_idle_timeout %q，使用默认 120s", cfg.Gateway.StreamIdleTimeout)
		streamIdle = 120 * time.Second
	}
	if streamIdle == 0 {
		log.Println("[WARN] stream_idle_timeout = 0：后端卡死时流式连接将永久挂起")
	}

	// 解析单条流总时长上限（"0" = 不限制）
	streamMax, err := time.ParseDuration(cfg.Gateway.StreamMaxDuration)
	if err != nil || streamMax < 0 {
		log.Printf("[WARN] 无效的 stream_max_duration %q，按 0（不限制）处理", cfg.Gateway.StreamMaxDuration)
		streamMax = 0
	}

	// 解析默认池名（未显式配置时回退为 "default"）
	defaultPool := cfg.DefaultPool
	if defaultPool == "" {
		defaultPool = "default"
	}

	// 创建模型路由器（按 model 路由到不同后端池）
	r := router.NewModelRouter(defaultPool)
	// 确保默认池存在
	r.RegisterPool(defaultPool)

	for _, bc := range cfg.Backends {
		poolName := bc.Pool
		if poolName == "" {
			poolName = defaultPool
		}
		pl := r.RegisterPool(poolName)

		interval, err := time.ParseDuration(bc.HealthCheckInterval)
		if err != nil || interval == 0 {
			interval = 10 * time.Second
		}

		healthTimeout, err := time.ParseDuration(bc.HealthCheckTimeout)
		if err != nil || healthTimeout <= 0 {
			healthTimeout = 3 * time.Second
		}

		be := backend.NewBackend(bc.Name, bc.URL, bc.HealthCheckPath, interval, healthTimeout, timeout)
		pl.Add(be)
		be.StartHealthCheck()

		log.Printf("[后端] %s (%s) — 池: %s, 健康检查: %v 间隔 / %v 超时",
			bc.Name, bc.URL, poolName, interval, healthTimeout)
	}

	// 映射模型到后端池（未匹配的 model 自动落入默认池）
	for _, m := range cfg.Models {
		r.RegisterPool(m.Pool) // 确保模型引用的池存在
		if err := r.MapModel(m.Name, m.Pool); err != nil {
			log.Fatalf("模型映射失败: %v", err)
		}
		log.Printf("[模型] %s → 池: %s", m.Name, m.Pool)
	}

	// 等待初始健康检查完成
	log.Println("等待初始健康检查...")
	time.Sleep(2 * time.Second)

	// 创建代理
	gw := proxy.NewProxy(r, proxy.Options{
		Timeout:           timeout,
		MaxRetries:        cfg.Gateway.MaxRetries,
		MaxBodyBytes:      maxBodyBytes,
		StreamIdleTimeout: streamIdle,
		StreamMaxDuration: streamMax,
		Debug:             *debug,
	})

	// ====== 入口治理：API Key 鉴权 + 按 Key 限流 ======
	//
	// 默认关闭，老用户升级后行为不变；开启后才对推理路径生效。
	var authenticator *auth.Authenticator
	if cfg.Auth.Enabled {
		authenticator, err = auth.New(cfg.Auth)
		if err != nil {
			// 启动即失败而不是降级为「不鉴权」：
			// 配了 enabled=true 说明有管控意图，静默放行等于安全策略被无声绕过。
			log.Fatalf("鉴权配置无效: %v", err)
		}
	}

	// ====== 路由注册 ======
	mux := http.NewServeMux()

	// 网关自身端点。/health 与 /backends 默认不鉴权：
	// 前者供 K8s probe / 负载均衡探活调用，加鉴权会让探活链路配置复杂化；
	// 后者属调试端点，内网使用。
	mux.HandleFunc("/health", gw.HealthHandler)
	mux.HandleFunc("/backends", gw.BackendsHandler)

	// /metrics 是否鉴权由 protect_metrics 决定（默认不鉴权，理由见 config.go）
	if authenticator != nil && cfg.Auth.ProtectMetrics {
		mux.Handle("/metrics", authenticator.Middleware(http.HandlerFunc(gw.MetricsHandler)))
	} else {
		mux.HandleFunc("/metrics", gw.MetricsHandler)
	}

	// 所有其他路径 → 鉴权 → 限流 → 代理转发到后端
	if authenticator != nil {
		mux.Handle("/", authenticator.Middleware(gw))
	} else {
		mux.HandleFunc("/", gw.ServeHTTP)
	}

	// ====== 启动服务器 ======
	addr := fmt.Sprintf(":%d", cfg.Gateway.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: timeout + 10*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 优雅关闭：先停健康检查，再给在途请求（含流式连接）宽限期，而非瞬间切断
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("收到信号 %v，正在优雅关闭...", sig)

		// 先停健康检查，避免关闭期间继续探测后端
		r.StopAll()

		// 给在途请求一个宽限期（30s），让非流式请求正常返回、流式连接不再被瞬间切断
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("[WARN] 优雅关闭超时（在途连接未在规定时间内结束），强制退出: %v", err)
		}
		log.Println("网关已停止接收新连接，等待在途请求完成")
	}()

	// 打印启动信息
	healthyCount := r.HealthyCount()
	log.Println("=======================================")
	log.Printf("Go 推理网关 v1.0 已启动")
	log.Printf("监听地址: %s", addr)
	log.Printf("后端数量: %d (健康: %d)", r.TotalCount(), healthyCount)
	for _, b := range r.AllBackends() {
		status := "健康"
		if !b.IsHealthy() {
			status = "不健康"
		}
		log.Printf("  - %s: %s [%s]", b.Name, b.URL, status)
	}
	log.Printf("超时设置: %v", timeout)
	log.Printf("最大重试: %d", cfg.Gateway.MaxRetries)
	if maxBodyBytes > 0 {
		log.Printf("请求体上限: %d 字节 (%s)", maxBodyBytes, cfg.Gateway.MaxBodySize)
	} else {
		log.Printf("请求体上限: 不限制 ⚠")
	}
	if streamIdle > 0 {
		log.Printf("流式空闲超时: %v", streamIdle)
	} else {
		log.Printf("流式空闲超时: 未启用 ⚠")
	}
	if streamMax > 0 {
		log.Printf("单条流总时长上限: %v", streamMax)
	}
	if authenticator != nil {
		log.Printf("鉴权: 已启用")
		for _, line := range authenticator.Describe() {
			log.Printf("  - %s", line)
		}
		log.Printf("  /metrics 端点鉴权: %v", cfg.Auth.ProtectMetrics)
	} else {
		log.Printf("鉴权: 未启用 ⚠ 任何人都能直接调用后端（内网环境可接受，公网请务必开启）")
	}
	log.Printf("调试日志: %v", *debug)
	log.Println("=======================================")

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("服务启动失败: %v", err)
	}

	log.Println("网关已关闭")
}
