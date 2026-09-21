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
| **请求体上限** | 可配 `max_body_size`（默认 16MB），超限返回 413，防止 GB 级 body 打满内存 |
| **流式超时守卫** | 空闲超时（两次数据块间隔）+ 可选总时长上限，防后端卡死导致连接永久挂起 |
| **API Key 鉴权** | 可选开启；支持 `Authorization: Bearer` 与 `X-API-Key` 两种写法，配置里可填明文或 sha256 摘要 |
| **按 Key 限流** | 每个 Key 独立令牌桶（QPS + 突发额度），超限返回 429 并带 `Retry-After` |
| **用量归因** | 按 Key 统计请求数与状态码，支撑成本分摊与配额审计 |
| Prometheus 指标 | 13 个指标，纯 stdlib 实现，零依赖 |
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
go build -o inference-gateway ./cmd/gateway
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

  # 请求体上限，支持 "16MB" / "512KB" / "1G"；填 "0" = 不限制（不推荐）
  max_body_size: "16MB"
  # 流式空闲超时：两次数据块之间的最大间隔，收到数据即重置；"0" = 不启用
  stream_idle_timeout: "120s"
  # 单条流最长总时长，兜底「极慢但一直在吐」的连接；"0" = 不限制（默认）
  stream_max_duration: "0"

backends:
  - name: "vllm-node-1"               # 标识名，出现在日志和指标中
    url: "http://192.168.1.101:8000"  # 后端地址
    health_check_path: "/health"      # 健康检查路径（默认 /health）
    health_check_timeout: "3s"        # 健康探测独立超时（默认 3s）
    health_check_interval: "10s"      # 健康检查间隔
    pool: "default"                   # 所属后端池

  - name: "vllm-node-2"
    url: "http://192.168.1.102:8000"
    health_check_path: "/health"
    health_check_timeout: "3s"
    health_check_interval: "10s"
    pool: "default"

# 入口治理（可选，默认关闭；完整示例见 config.auth.example.yaml）
auth:
  enabled: false
  # header: "X-API-Key"      # 留空 = 同时接受 Authorization: Bearer <key> 与 X-API-Key: <key>
  # protect_metrics: false   # /metrics 是否也要鉴权（公网暴露时建议 true）
  # keys:
  #   - name: "team-a"       # 标识名，出现在日志与指标标签中（不参与鉴权）
  #     key: "sk-xxxx"       # 明文；启动时换算为 sha256，明文不驻留内存
  #     # key_hash: "..."    # 或直接填 sha256 摘要（配置里不落明文）
  #     rate_limit: 10       # 每秒请求数，0 = 不限流
  #     burst: 20            # 突发容量，0 = 自动取 max(1, rate_limit)
```

> 多后端时网关自动 round-robin 轮询。一个后端挂了，请求自动路由到其他健康节点。

### 入口治理：API Key 鉴权与限流

默认关闭，开启后推理路径（`/` 下所有请求）必须先通过鉴权才进入后端调度：

| 场景 | 响应 |
|------|------|
| 未带 Key | `401` + `WWW-Authenticate: Bearer` |
| Key 不匹配 | `401`，error type 为 `invalid_key` |
| 超出该 Key 的限流额度 | `429` + `Retry-After`（秒） |
| 通过 | 正常转发，并在 `/metrics` 中按 Key 计数 |

两个刻意的设计取舍：

1. **`auth.enabled=true` 但没配任何 Key 时启动失败**，而不是降级为"不鉴权"。配了 `enabled` 说明有管控意图，静默放行等于安全策略被无声绕过。
2. **`/health`、`/backends` 默认不鉴权**：前者供 K8s probe 与负载均衡探活调用，加鉴权会让探活链路配置复杂化。`/metrics` 由 `protect_metrics` 单独控制。

### 健康检查路径：各家引擎约定不同

这是接入非 vLLM 后端时最容易踩的坑——**网关默认探 `/health`，但并不是所有引擎都有这个端点**。探错了端点，网关会一直把活着的后端判定为 unhealthy：

| 推理引擎 | 健康检查路径 | 备注 |
|----------|-------------|------|
| vLLM | `/health` | 默认值，无需配置 |
| TGI (text-generation-inference) | `/health` | 默认值 |
| **Ollama** | `/` | 必须显式配 `health_check_path: "/"` |
| SGLang | `/health_generate` | 会真实跑一次生成，比 `/health` 严格 |

`health_check_timeout` 默认 3s，**故意不复用 `gateway.timeout`**（后者通常设 120s 以适应慢推理）——若复用，一个卡死的后端会让探测 goroutine 长期挂起，健康状态无法及时翻转。

> 另一个常见坑：**`url` 不要带 `/v1` 后缀**。网关转发时是「后端 URL + 原始请求路径」，OpenAI 客户端发来的路径本身已含 `/v1`，写进 url 会拼成 `/v1/v1/chat/completions` 返回 404。

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
| `inference_gateway_errors_total` | counter | 错误数（按类型：`no_healthy_backend` / `backend_unreachable` / `all_retries_failed` / `retryable_status` / `request_body_too_large` / `stream_idle_timeout` / `stream_max_duration`） |
| `inference_gateway_retries_total` | counter | 重试次数 |
| `inference_gateway_sse_connections_active` | gauge | 当前活跃 SSE 连接 |
| `inference_gateway_sse_connections_total` | counter | SSE 连接历史总数 |
| `inference_gateway_backend_health` | gauge | 后端健康（1=健康, 0=故障） |
| `inference_gateway_backend_latency_seconds` | gauge | 健康检查延迟 |
| `inference_gateway_backend_request_latency_seconds` | gauge | 后端最近一次请求延迟 |
| `inference_gateway_auth_failures_total` | counter | 鉴权失败数（按原因：`missing_key` / `invalid_key`） |
| `inference_gateway_key_requests_total` | counter | **按 API Key 的请求数**（key + status_code），用于用量归因与成本分摊 |
| `inference_gateway_key_throttled_total` | counter | 按 API Key 的限流拒绝次数 |

> **为什么用量归因用独立指标，而不是给 `requests_total` 加 key 标签？**
> `requests_total` 已经是 `model × backend × status_code` 的组合，再乘上 key 会让时间序列成倍膨胀。
> 而"哪个后端处理了"与"哪个 Key 用了"是两个正交的关注点，拆成独立指标后靠 PromQL 聚合即可，既控制基数也让语义边界清晰。

---

## 项目结构

```
inference-gateway/
├── cmd/gateway/main.go     # 入口：flag 解析 → 配置 → 后端池 → 路由 → 启动
├── internal/
│   ├── config/config.go    # 配置结构体 + YAML 加载 + 校验（gopkg.in/yaml.v3）
│   ├── auth/               # 入口治理：API Key 鉴权 + 令牌桶限流 + 中间件
│   ├── backend/            # 后端结构体 + 健康检查 goroutine + 后端池
│   ├── proxy/              # 核心代理逻辑（路由、编码修复、非流式/SSE 分发）
│   ├── router/             # 按 model 路由到后端池
│   └── metrics/            # Prometheus 指标（纯 stdlib，零外部依赖）
├── config.yaml             # 示例配置（鉴权默认关闭）
├── config.auth.example.yaml# 入口治理完整示例（可直接运行）
├── config.ollama.yaml      # 本机 Ollama 实测配置
├── go.mod                  # 仅依赖 yaml.v3 + x/text
└── go.sum
```

**阅读顺序建议**：`cmd/gateway/main.go` → `internal/config` → `internal/backend` → `internal/proxy` → `internal/auth` → `internal/metrics`

---

## 设计决策

### 为什么在网关层做 GBK→UTF-8 转换？

Windows 终端（CMD/PowerShell）默认编码是 GBK。当用户用 `curl` 发中文 prompt 时，JSON body 实际是 GBK 编码，但 HTTP/JSON 规范要求 UTF-8。vLLM 严格校验 UTF-8，直接返回 400。

两个选择：让每个客户端改编码 → 不现实；网关透明转换 → 一次搞定。`proxy.go` 中的 `ensureUTF8()` 函数用 `utf8.Valid()` 快速检测 + `golang.org/x/text` 做 GBK→UTF-8 转码，对客户端完全透明。

### 为什么流式请求不用带超时的 Client？

SSE 流式响应可能持续数分钟（长文本生成），如果设置了 `http.Client.Timeout`，到达超时时间后连接被强制关闭，客户端收到的回复会截断。流式处理时给 `streamClient` 设 `Timeout: 0`，复用 `p.client.Transport` 的连接池即可。

### 为什么流式请求必须清掉 Server.WriteTimeout？

`http.Server.WriteTimeout` **不是在每次 Write 时重置，而是在「请求头读完时」给连接打一次写 deadline，整个请求期间不再变动**（Go 源码 `net/http/server.go` 的 `readRequest`：`defer c.rwc.SetWriteDeadline(time.Now().Add(d))`）。

对普通请求这是对的；对 SSE 这种分钟级长连接，它等同于**到点硬掐**。实测：

| 场景（后端每秒 1 个 chunk，共 6 秒） | 客户端实际收到 |
|---|---|
| `WriteTimeout=3s`，未处理 | 3 个 chunk，`unexpected EOF`（24/48 字节） |
| 用 `http.ResponseController.SetWriteDeadline(time.Time{})` 清掉 | 6 个 chunk，正常 EOF（48/48 字节） |

所以 `handleStreaming` 一进来就清掉写 deadline，改由自己的超时守卫接管。只给流式清，非流式仍受 `WriteTimeout` 保护。

### 为什么用「空闲超时」而不是 `ResponseHeaderTimeout`？

`http.Client.ResponseHeaderTimeout` 只覆盖「等首字节」这一段，管不了「吐了一半卡死」——而后者在 GPU OOM / 显存打满时才是常态。空闲超时从发请求起算、每次收到数据就重置，两种故障一并覆盖，只需一个配置项。

超时触发时 200 已经发出、无法再改状态码，网关会补发一个 SSE `event: error` 再关闭，让客户端能区分「被网关掐断」和「流正常结束」。

### 为什么限流自己写，不用 `golang.org/x/time/rate`？

`x/time/rate` 是官方扩展库，质量没问题，但引入它会让 `go.mod` 多一条 `require`。本项目的立身之本是「零依赖单二进制」，为一个约 60 行的数据结构破这个例不划算。

自研版还带来一个实际好处：`Allow(now)` 接收外部传入的时间戳，于是限流行为能用**假时钟做确定性单测**——`x/time/rate` 依赖真实时钟，只能靠 `sleep` 测，慢且不稳定。

### 鉴权中间件最隐蔽的坑：包装 ResponseWriter 会毁掉 SSE

中间件要用 `statusRecorder` 包裹 `ResponseWriter` 才能捕获状态码做用量归因。但这个包装有两个必须实现的接口，漏掉任何一个都会**静默**破坏流式响应：

| 接口 | 漏掉的后果 |
|---|---|
| `Flush()` | 丢失 `http.Flusher` → proxy 走「降级为非流式」分支，响应变成一次性返回，流式彻底失效 |
| `Unwrap()` | `http.NewResponseController(w)` 无法穿透到真实连接 → 上一节「清掉 WriteTimeout」的操作失败，**长 SSE 被硬掐** |

单测里用一对对照用例锁死这个行为：有 `Unwrap` 时 `SetWriteDeadline` 成功，去掉 `Unwrap` 则失败——两个结果同时成立，才能确认穿透机制真实存在，而不是写了个空断言。

端到端也验证过：开启鉴权后，后端持续吐 15 秒（网关 `timeout=2s` → `WriteTimeout=12s`），客户端完整收到 15 个 chunk，未被 12 秒硬切。

### 为什么不支持流式请求的重试？

SSE 已经开始向客户端发送数据（HTTP 200 已返回），此时重试意味着：要么重复发送已输出的 token，要么中断当前流换后端重新开始。前者破坏语义，后者客户端体验差。v1 版本对流式请求不做重试，失败直接返回错误。

---

## License

MIT
