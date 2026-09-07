# Inference Gateway · 通用推理控制平面

> 一个轻量的**通用推理控制平面**：在客户端与各类推理后端（vLLM / Ollama / 云端 LLM / 任意 OpenAI 兼容服务）之间架一层透明代理，提供多后端负载均衡、按场景路由、SSE 流式透传、失败自动重试、透明编码修复，以及 Prometheus 可观测指标。一次部署，即可把任意上层应用（不限运维、不限场景）接入统一的推理底座。

---

## 为什么需要这个网关

直接连 vLLM 在开发环境没问题，但上生产时会遇到三个麻烦：

1. **单点故障** — vLLM 进程挂了，客户端直接 502，没有容错
2. **无法扩容** — 加 GPU 节点后，客户端不知道该发给谁
3. **看不见** — 多少请求、多少延迟、哪个后端慢了，全是黑盒

这个网关用 ~500 行 Go 代码解决这三个问题。部署在你和 vLLM 之间，对客户端完全透明。

---

## 架构

```
                          ┌──────────────────────────────────┐
                          │        Go 推理网关 :8080          │
  客户端                  │                                  │
  curl /                  │  /v1/chat/completions ──→ 后端1  │
  Python SDK  ──────────→│                     ──→ 后端2  │────→ vLLM:8000
  你的应用                │                     ──→ 后端3  │
                          │                                  │
                          │  /health     网关自检            │
                          │  /metrics    Prometheus 指标     │
                          │  /backends   后端状态 JSON       │
                          └──────────────────────────────────┘
```

---

## 功能

| 功能 | 说明 |
|------|------|
| 多后端轮询 | round-robin 负载均衡，自动跳过不健康节点 |
| 健康检查 | 独立 goroutine 定期探测 /health，状态变化即时日志 |
| 失败重试 | 可配置重试次数，自动排除已失败后端 |
| SSE 流式代理 | `stream: true` 请求逐块转发，支持 Flusher 降级 |
| 编码自动修复 | 检测 GBK → 自动转 UTF-8（解决 Windows 终端乱码） |
| Prometheus 指标 | 10 个指标，纯 stdlib 实现，零依赖 |
| 结构化日志 | 请求 ID 追踪，`[请求 #N]` 格式贯穿全链路 |
| 优雅关闭 | SIGINT/SIGTERM 触发，停止健康检查再退出 |

---

## 快速开始

### 前置条件

- Go 1.21+
- 一个运行中的 vLLM 实例（或任何 OpenAI 兼容的推理服务）

### 1. 克隆 & 编译

```bash
git clone https://github.com/你的用户名/inference-gateway.git
cd inference-gateway
go build -o inference-gateway .
```

### 2. 配置后端

编辑 `config.yaml`，把 `backends` 下的 URL 改成你的 vLLM 地址：

```yaml
backends:
  - name: "my-vllm"
    url: "http://你的vLLM地址:8000"
    health_check_interval: "10s"
```

> 如果是 AutoDL 等云 GPU，推荐用 SSH 端口转发：  
> `ssh -L 8000:localhost:8000 -p <SSH端口> root@<AutoDL_IP> -N`  
> 然后 URL 填 `http://localhost:8000`

### 3. 启动网关

```bash
./inference-gateway --config config.yaml
```

### 4. 测试

```bash
# 非流式请求
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen2.5-7B-Instruct","messages":[{"role":"user","content":"Hello"}],"max_tokens":50}'

# 流式请求
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen2.5-7B-Instruct","messages":[{"role":"user","content":"讲个笑话"}],"stream":true}'

# 查看后端状态
curl http://localhost:8080/backends | python -m json.tool

# 查看指标
curl http://localhost:8080/metrics
```

---

## 配置说明

```yaml
gateway:
  port: 8080          # 网关监听端口
  timeout: "120s"     # 后端请求超时（vLLM 推理较慢，建议设大一些）
  max_retries: 1      # 失败重试次数（0=不重试）

backends:
  - name: "vllm-node-1"              # 标识名，出现在日志和指标中
    url: "http://192.168.1.101:8000" # vLLM 地址
    health_check_interval: "10s"     # 健康检查间隔

  - name: "vllm-node-2"
    url: "http://192.168.1.102:8000"
    health_check_interval: "10s"
```

> 多后端时网关自动 round-robin 轮询。一个后端挂了，请求自动路由到其他健康节点。

---

## API 端点

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/chat/completions` | POST | OpenAI 兼容的推理代理 |
| `/health` | GET | 网关自身健康状态 |
| `/metrics` | GET | Prometheus 格式指标 |
| `/backends` | GET | 所有后端详情（名称/地址/健康/延迟） |
| `/*` | 任意 | 通用代理，转发到 vLLM 对应路径 |

---

## Prometheus 指标

将 `localhost:8080` 加入 Prometheus scrape config 即可采集：

| 指标 | 类型 | 说明 |
|------|------|------|
| `inference_gateway_uptime_seconds` | gauge | 运行时长 |
| `inference_gateway_requests_total` | counter | 请求总数（按后端+状态码） |
| `inference_gateway_errors_total` | counter | 错误数（按类型） |
| `inference_gateway_retries_total` | counter | 重试次数 |
| `inference_gateway_sse_connections_active` | gauge | 当前活跃 SSE 连接 |
| `inference_gateway_sse_connections_total` | counter | SSE 连接历史总数 |
| `inference_gateway_backend_health` | gauge | 后端健康（1=健康, 0=故障） |
| `inference_gateway_backend_latency_seconds` | gauge | 健康检查延迟 |

---

## 项目结构

```
inference-gateway/
├── main.go          # 入口：flag 解析 → 配置 → 后端池 → 路由 → 启动
├── config.go        # 配置结构体 + YAML 加载（gopkg.in/yaml.v3）
├── backend.go       # 后端结构体 + 健康检查 goroutine + 后端池
├── proxy.go         # 核心代理逻辑（路由、编码修复、非流式/SSE 分发）
├── metrics.go       # Prometheus 指标（纯 stdlib，零外部依赖）
├── config.yaml      # 示例配置文件
├── go.mod           # Go module 定义（仅依赖 yaml.v3 + x/text）
└── go.sum
```

**阅读顺序建议**：`main.go` → `config.go` → `backend.go` → `proxy.go` → `metrics.go`

---

## 设计决策

### 为什么在网关层做 GBK→UTF-8 转换？

Windows 终端（CMD/PowerShell）默认编码是 GBK。当用户用 `curl` 发中文 prompt 时，JSON body 实际是 GBK 编码，但 HTTP/JSON 规范要求 UTF-8。vLLM 严格校验 UTF-8，直接返回 400。

两个选择：让每个客户端改编码 → 不现实；网关透明转换 → 一次搞定。`proxy.go` 中的 `ensureUTF8()` 函数用 `utf8.Valid()` 快速检测 + `golang.org/x/text` 做 GBK→UTF-8 转码，对客户端完全透明。

### 为什么流式请求不用带超时的 Client？

SSE 流式响应可能持续数分钟（长文本生成），如果设置了 `http.Client.Timeout`，到达超时时间后连接被强制关闭，客户端收到的回复会截断。流式处理时给 `streamClient` 设 `Timeout: 0`，复用 `p.client.Transport` 的连接池即可。

### 为什么不支持流式请求的重试？

SSE 已经开始向客户端发送数据（HTTP 200 已返回），此时重试意味着：要么重复发送已输出的 token，要么中断当前流换后端重新开始。前者破坏语义，后者客户端体验差。v1 版本对流式请求不做重试，失败直接返回错误。

---

## License

MIT
