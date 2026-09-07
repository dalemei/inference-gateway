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

	"github.com/dalemei/inference-gateway/internal/backend"
	"github.com/dalemei/inference-gateway/internal/config"
	"github.com/dalemei/inference-gateway/internal/proxy"
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

	// 创建后端池
	pool := backend.NewBackendPool()
	for _, bc := range cfg.Backends {
		interval, err := time.ParseDuration(bc.HealthCheckInterval)
		if err != nil || interval == 0 {
			interval = 10 * time.Second
		}

		be := backend.NewBackend(bc.Name, bc.URL, interval, timeout)
		pool.Add(be)
		be.StartHealthCheck()

		log.Printf("[后端] %s (%s) — 健康检查间隔: %v", bc.Name, bc.URL, interval)
	}

	// 等待初始健康检查完成
	log.Println("等待初始健康检查...")
	time.Sleep(2 * time.Second)

	// 创建代理
	gw := proxy.NewProxy(pool, timeout, cfg.Gateway.MaxRetries, *debug)

	// ====== 路由注册 ======
	mux := http.NewServeMux()

	// 网关自身端点
	mux.HandleFunc("/health", gw.HealthHandler)
	mux.HandleFunc("/metrics", gw.MetricsHandler)
	mux.HandleFunc("/backends", gw.BackendsHandler)

	// 所有其他路径 → 代理转发到 vLLM
	mux.HandleFunc("/", gw.ServeHTTP)

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
		pool.StopAll()

		// 给在途请求一个宽限期（30s），让非流式请求正常返回、流式连接不再被瞬间切断
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("[WARN] 优雅关闭超时（在途连接未在规定时间内结束），强制退出: %v", err)
		}
		log.Println("网关已停止接收新连接，等待在途请求完成")
	}()

	// 打印启动信息
	healthyCount := pool.HealthyCount()
	log.Println("=======================================")
	log.Printf("Go 推理网关 v1.0 已启动")
	log.Printf("监听地址: %s", addr)
	log.Printf("后端数量: %d (健康: %d)", len(pool.Backends()), healthyCount)
	for _, b := range pool.Backends() {
		status := "健康"
		if !b.IsHealthy() {
			status = "不健康"
		}
		log.Printf("  - %s: %s [%s]", b.Name, b.URL, status)
	}
	log.Printf("超时设置: %v", timeout)
	log.Printf("最大重试: %d", cfg.Gateway.MaxRetries)
	log.Printf("调试日志: %v", *debug)
	log.Println("=======================================")

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("服务启动失败: %v", err)
	}

	log.Println("网关已关闭")
}
