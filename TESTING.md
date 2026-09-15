# 推理网关 · 理解手册与测试清单（TESTING.md）

> 本文件目的：**帮助作者在合并（commit/push）前吃透架构细节，并提供可操作的验证步骤**。
> 设计原则（作者约定）：实现可由 AI 完成，但合并前作者必须亲自理解全链路并实测通过。
> 本文件为本地文档，**不要求随每次提交更新**，仅在架构变化时同步修订。

---

## 0. 提交闸门（何时才能 push）

满足以下两条才允许合并到 GitHub：

1. **作者已理解整个架构**：能独立讲清每个包职责、请求数据流、失败/重试路径。
2. **vLLM 实测通过**：真实后端转发、model 路由、指标、韧性全部验证绿。

当前状态（**2026-09-15 更新**）：**两条闸门均已满足，可以合并。**

- 真实后端实测**已于本机 Ollama 完成**（无需 GPU，零成本），四项全绿 —— 详见 §3.9。
- 过程中实测发现并修复了 3 个真实缺陷（健康检查路径写死 / 后端 5xx 不重试 / 新错误类型指标被静默丢弃），详见 §3.10。
- 待合并内容：`3a21171`（model 路由，尚未 push）+ 本次 6 个文件的修复 + `TESTING.md` + `config.ollama.yaml`。

> 说明：原定"vLLM GPU 实测"不再是阻塞项——Ollama 同为 OpenAI 兼容协议，
> 已能覆盖转发/路由/指标/韧性四条验证路径。vLLM 实测可留作后续独立一次
> （顺带做 AWQ/FP8 对比，产出更有内容）。

---

## 1. 架构走读（理解用）

### 1.1 包职责表

| 包 / 文件 | 职责 | 关键导出 |
|-----------|------|----------|
| `cmd/gateway/main.go` | 入口：flag、加载配置、按 `pool` 分组建多池、注册 model→池映射、起健康检查、组装 Proxy、mux 路由、优雅关闭 | `main()` |
| `internal/config` | 解析 `config.yaml`：`Gateway` / `Backends[]`(含 `Pool`) / `DefaultPool` / `Models[]`(Name→Pool) | `LoadConfig` |
| `internal/backend` | 单后端（atomic 健康/延迟）+ 后端池（round-robin、排除重试、健康检查 ticker） | `BackendPool` `Next` `NextExcluding` `StartHealthCheck` `StopAll` |
| `internal/router` | 持有 `map[pool]*BackendPool` + `map[model]pool`，`Route(model)` 解析到池 | `ModelRouter` `RegisterPool` `MapModel` `Route` `Pools` |
| `internal/proxy` | 代理核心：读 body → `detectStreaming`+`extractModel` → `router.Route(model)` → `Next/NextExcluding` → SSE 或非流式转发 | `Proxy` `ServeHTTP` `HealthHandler` `MetricsHandler` `BackendsHandler` |
| `internal/metrics` | 计数器 + Prometheus 输出；`RecordRequest` 带 `model` 标签（用量归因雏形） | `RecordRequest(model,...)` `RecordGatewayError` `WriteMetrics` |

### 1.2 请求数据流（主线）

```
HTTP 请求
  → ServeHTTP
      → 读 body（缓存，便于重试重放）
      → isStreaming = detectStreaming(body)        # 解析 JSON 的 stream 字段
      → model      = extractModel(body)            # 解析 JSON 的 model 字段
  → 若流式: handleStreaming(pool.Next())
      否则:   handleRequest(重试循环)
                 attempt 0:        pool.Next()
                 重试 attempt>0:   pool.NextExcluding(tried)   # 排除已失败节点
                 pool==nil → RecordGatewayError("no_healthy_backend")
                             → 503 "no healthy backends available"
                 构造上游请求 → client.Do → 记录指标 RecordRequest(model, backend, ...)
                 非 200 且可重试 → 加入 tried，进入下一轮
  → 失败耗尽 → 503
```

### 1.3 model 路由逻辑（`router.Route`）

```
Route(model):
  if model 在 modelMap 中:        返回对应池 (ok=true)
  else if defaultPool 存在:       返回 default 池 (ok=true)   ← 向后兼容：裸请求落默认
  else:                           返回 (nil, false)           ← 503 "no backend pool available for this model"
```

### 1.4 失败 / 重试路径要点

- **无健康后端**：`Next`/`NextExcluding` 返回 nil → 503「no healthy backends available」。
- **单后端非 200**（2026-09-15 修正）：**只有可重试状态码才换节点重试** ——
  `429 / 502 / 503 / 504`（见 `isRetryableStatus`）。网络层错误（`client.Do` 返回 err）
  同样触发重试。其余 4xx/5xx 直接透传，因为重试无意义。
  > 修复前这里只有网络错误才重试，后端返回 500/503 一律透传——对推理集群是致命的
  > （vLLM 过载返回 503、GPU OOM 返回 500，恰恰最需要换节点）。
- **流式不重试**：SSE 已开始推送就无法重放，设计上 `handleStreaming` 只取一次 `Next()`，首节点失败直接 503（这是有意为之，非缺陷）。
- **指标兜底**：`RecordRequest` 的 `model` 为空时归一化为 `"unknown"`，避免 Prometheus 标签出现空值。

### 1.5 关键设计取舍（作者拍板）

| 取舍点 | 决策 | 理由 |
|--------|------|------|
| model→池 匹配 | 精确匹配 | 简单、可预测、覆盖 99% 场景 |
| 未匹配 model | 落 default_pool | 向后兼容今天所有"裸请求" |
| 目录结构 | `cmd/` + `internal/` | Go 服务通用惯例，第一眼专业；`internal/` 防外部误 import |
| 流式重试 | 不重试 | SSE 无法安全重放 |

---

## 2. 本地快速验证（不依赖 GPU，已通过）

> 仅验证编译与路由代码路径正确，不代表真实后端转发。

```bash
# 离线编译（依赖已在本地 cache，避免网络/代理干扰）
GOPROXY=off GOSUMDB=off go vet ./...
GOPROXY=off GOSUMDB=off go build ./...

# 冒烟（无真实后端，预期 degraded；验证路由组装 + model 提取）
go build -o ./igw_smoke ./cmd/gateway
./igw_smoke --config config.yaml &
sleep 4
curl -s --noproxy '*' http://localhost:8080/health
curl -s --noproxy '*' http://localhost:8080/backends
curl -s --noproxy '*' -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen2.5-7B","messages":[{"role":"user","content":"hi"}]}'
kill %1; rm -f ./igw_smoke
```

判定：返回「no healthy backends available」即证明 `Route()` 解析到了池（非 nil）；若返回「no backend pool available for this model」才是路由失败。

---

## 3. vLLM 实测清单（依赖 GPU，09-08 执行）

### 3.1 起 GPU + vLLM（AutoDL 4080S）

```bash
# 开机后进入容器，确认 vLLM 已起（Qwen2.5-7B，监听 8000）
curl -s http://localhost:8000/v1/models
# 预期返回含 Qwen2.5-7B 的 JSON
```

### 3.2 配置 config.yaml

把后端 url 指向真实地址；可加第二个池做 model 路由验证：

```yaml
gateway:
  port: 8080
  max_retries: 2
default_pool: default
backends:
  - name: "vllm-4080"
    url: "http://<GPU内网IP>:8000"   # ← 改为真实 vLLM 地址
    pool: default
    health_check_interval: "10s"
  - name: "ollama-local"             # 可选：第二个池，验证 model 路由
    url: "http://localhost:11434"
    pool: local
    health_check_interval: "10s"
models:
  - name: "Qwen2.5-7B"
    pool: default
  - name: "llama3-8b"
    pool: local
```

### 3.3 启动网关 + 健康端点

```bash
go build -o ./igw ./cmd/gateway
./igw --config config.yaml > /tmp/igw.log 2>&1 &

curl -s --noproxy '*' http://localhost:8080/health      # 预期 healthy
curl -s --noproxy '*' http://localhost:8080/backends    # 列出各池及节点状态
```

### 3.4 转发验证（流式 / 非流式各一次）

```bash
# 非流式
curl -s --noproxy '*' -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen2.5-7B","messages":[{"role":"user","content":"你好"}],"stream":false}'

# 流式（应看到 SSE 逐块输出）
curl -s --noproxy '*' -N -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen2.5-7B","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

### 3.5 model 路由验证（多池）

发不同 model，确认落到不同后端池（看 `/backends` 的 pool 归属与 `/health` 日志）：

```bash
curl -s --noproxy '*' -X POST http://localhost:8080/v1/chat/completions \
  -d '{"model":"llama3-8b","messages":[{"role":"user","content":"hi"}]}'  # 应走 local 池
```

### 3.6 指标验证（model 标签）

```bash
curl -s --noproxy '*' http://localhost:8080/metrics | grep inference_gateway_requests_total
# 预期含 model="Qwen2.5-7B" 之类标签
```

### 3.7 韧性验证（排除重试）

1. 关掉其中一个后端（或改 url 为不可达）。
2. 发请求，观察日志：应命中 `NextExcluding` 跳过故障节点，落到健康节点；`inference_gateway_retries_total` 增加。

### 3.8 全绿 → 合并

满足 0 节两条闸门后，由作者在本地执行：

```bash
cd "C:/Users/Admin/Desktop/ai-infra-lab/inference-gateway"
git add -A
git commit -m "feat: 按 model 路由到不同后端池 + 指标加 model 标签（实测通过）"
git push origin main   # 代理死了先关掉再推
```

---

## 3.9 本机 Ollama 实测记录（2026-09-15，已通过）

> 用 `config.ollama.yaml` + 本机 Ollama（`qwen2.5:1.5b`）跑通，全程零 GPU 成本。
> 另起一个"健康检查 200、推理返回 503"的假后端（`127.0.0.1:9999`）验证韧性。

| # | 验证项 | 命令/方式 | 实测结果 |
|---|--------|-----------|----------|
| 1 | 后端健康判定 | `curl :8080/health` | `{"healthy_backends":2,"status":"healthy"}` ✅ |
| 2 | 非流式转发 | POST `/v1/chat/completions` | 200，真实回复 + `total_tokens:51` ✅ |
| 3 | 流式 SSE | 同上 + `"stream":true` | 逐块 `data:` 输出，收尾 `data: [DONE]` ✅ |
| 4 | model 路由 | 请求带 `model: qwen2.5:1.5b` | 落到 `default` 池（非 nil 即路由成功）✅ |
| 5 | 指标带 model 标签 | `curl :8080/metrics` | `requests_total{model="qwen2.5:1.5b",backend="ollama-main",status_code="200"}` ✅ |
| 6 | **韧性（换节点重试）** | 假后端返 503 | 日志 `→ fake-broken 返回 503（可重试），换节点 (attempt 1/2)` → `→ ollama-main 200 (attempt 2/2)`；客户端侧 4/4 返回 200 ✅ |
| 7 | SSE 不重试（设计验证） | 流式命中假后端 | 直接失败、无 `[DONE]`，符合 §1.4 设计 ✅ |
| 8 | 错误留痕 | `metrics` | `errors_total{type="retryable_status"} 2` ✅ |

### 3.10 实测发现并修复的 3 个缺陷（2026-09-15）

| # | 缺陷 | 现象 | 根因 | 修复 |
|---|------|------|------|------|
| 1 | **健康检查路径写死 `/health`** | Ollama 永远 unhealthy，实测起不来 | `check()` 硬编码 `URL + "/health"`；各家引擎约定不一：vLLM/TGI=`/health`、Ollama=`/`、SGLang=`/health_generate` | 新增 `health_check_path` 配置项，默认 `/health` |
| 2 | **后端 5xx 不重试** | 后端返 503 直接透传客户端，日志永远 `attempt 1/2` | 只要 `client.Do` 无 err 就无条件写响应 return，未判断状态码 | 新增 `isRetryableStatus`（429/502/503/504），在写响应头**之前**判断并换节点；重试前显式 `resp.Body.Close()` 避免连接泄漏 |
| 3 | **新错误类型指标被静默丢弃** | `errors_total` 只有固定 3 个 type，新增类型累加进 map 却永不输出 | `WriteMetrics` 里硬编码了类型列表 | 已知类型恒输出（保证告警标签稳定）+ 运行期新类型按字典序补输出 |

附带修正：`go.mod` 中 `golang.org/x/text` 被误标 `// indirect`（实际直接引用），`go build` 已自动纠正。

---

## 3.11 AutoDL GPU 实测（脚本已就绪，待执行）

> 定位：**不是重复验证功能**，而是产出 Only-GPU 能拿到的量化数据（网关开销 / 并发曲线 / 故障窗口），
> 这是博客与 README 的核心资产。方案见 `gpu-bench/README.md`。

| 实验 | 内容 | 产出 |
|------|------|------|
| 1 | 直连 vs 经网关，c=8 流式 64 请求 | 网关引入的 TTFT / E2E / QPS 开销百分比 |
| 2 | 并发 1/4/8/16/32 曲线对比 | README 可引用表格 |
| 3 | 压测中 kill 一个 vLLM 副本 | 健康摘除时延、故障窗口失败率、自动加回 |

**先写脚本再开机**（¥1.68/h）：`gpu-bench/` 下 `1_start_vllm.sh` → `2_start_gateway.sh` → `3_compare.sh` → `4_failover.sh`。
已在本地交叉编译好 `igw-linux-amd64`（`GOOS=linux GOARCH=amd64`），上传即可，无需在 GPU 机上装 Go。

**开机前先做**：把本地 3 个 commit 提交掉（见 ROADMAP「待提交」），避免实测后代码再堆积。

---

## 3.12 开 GPU 前的代码走读：6 个优化点（2026-09-15 晚）

完整走读 `proxy.go` / `backend.go` / `router.go` / `metrics.go` 后的问题清单，按「是否污染压测数据」排序。

### 已修（3 个，本机 Ollama 回归通过）

| # | 问题 | 为什么必须修 |
|---|------|-------------|
| 1 | `MaxIdleConnsPerHost` 未设（**Go 默认 = 2**） | 网关只连少数后端，并发一超就把空闲连接挤掉 → 每请求重建 TCP。**不修的话，实验 1 测出的"网关开销"其实是"建连开销"，数据不可信** |
| 2 | `Backend.Latency` 是普通 `time.Duration` | 健康检查 goroutine 写、`/metrics`+`/backends` 读 → data race。`-race` 必挂（ROADMAP Day 8 已列未做）。已改 `atomic.Int64`（存纳秒） |
| 3 | 健康检查只 `Close()` 不读完 body | 连接无法归还连接池，5s 一次 × 长时间 = TIME_WAIT 堆积。已改为 `io.Copy(io.Discard, io.LimitReader(body, 64KB))` 后再 Close |

### 待作者拍板（3 个，涉及设计取舍）

| # | 问题 | 选项 |
|---|------|------|
| 4 | `WriteTimeout: timeout+10s` 会硬切长 SSE | A. SSE 单独不受 WriteTimeout 限制 B. 独立配置项 C. 不动 |
| 5 | 流式 client `Timeout:0` 无任何兜底，后端挂起则连接永不释放 | 加 `ResponseHeaderTimeout`（只等首字节，不影响 SSE 语义） |
| 6 | **model 名直接做指标标签**（用户可控输入）→ 基数炸弹 | A. 白名单：配置里声明的 model 用原名，其余归 `other` B. 上限截断 C. 不动 |

### 观察项（暂不改）

- `Next()` 用写锁：热路径串行，但与项目宣称的"无锁读"有出入；32 并发下 mutex 竞争未必是瓶颈，等实测数据说话。
- `io.ReadAll` 无 body 上限、缺 `X-Forwarded-For` / `X-Request-Id`、重试无退避：属工程完备性，可后续补。

---

## 4. 备注

- 编译产物（`igw` / `igw_smoke` / `*.exe`）已被 `.gitignore` 忽略，不会进版本库。
- 改含中文注释的 Go 文件后，务必做一次 UTF-8 解码校验，避免编辑器写出 GBK/乱码。
- 本目录结构：`cmd/gateway/main.go` + `internal/{config,backend,metrics,proxy,router}/*.go`。
