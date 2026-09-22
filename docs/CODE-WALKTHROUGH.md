# inference-gateway 代码导览（Review 专用）

> 配套文档：`dev/regression.py`（自动化功能回归）、根目录 `inference-gateway-代码review-2026-09-21.md`（缺陷报告）
> 生成时间：2026-09-21

## 0. 怎么用这份文档

这份文档**不复述代码**，而是告诉你每个文件的「该看哪段、问自己什么问题、已知雷在哪」。

建议流程：**先读这一节 → 打开对应文件 → 带着自检问题读 → 跑 `dev/regression.py` 验证你的理解 → 记录疑问**。

---

## 1. 仓库全貌

业务代码 **2596 行**，测试 **640 行**。分布极不均衡：

| 文件 | 行数 | 职责 | 测试覆盖 | 建议投入 |
|---|---:|---|---|---:|
| `internal/proxy/proxy.go` | **720** | 代理核心：路由分发、重试、流式、守卫 | **0** ⚠️ | 最多 |
| `internal/metrics/metrics.go` | 419 | Prometheus 指标（纯文本输出） | 0 ⚠️ | 中 |
| `cmd/gateway/main.go` | 242 | 启动、装配、优雅退出 | 0 | 少 |
| `internal/backend/backend.go` | 236 | 后端实例 + 健康检查 + 池轮询 | 0 ⚠️ | 中 |
| `internal/config/config.go` | 255 | 配置加载与校验 | 103 行 ✅ | 少 |
| `internal/usage/usage.go` | 224 | token 计量解析 | 253 行 ✅ | 中 |
| `internal/auth/auth.go` | 184 | Key 鉴权 | 284 行 ✅ | 中 |
| `internal/auth/middleware.go` | 129 | HTTP 中间件 + Key 透传 | 含在 auth_test ✅ | 中 |
| `internal/router/router.go` | 106 | model → pool 映射 | 0 ⚠️ | 少 |
| `internal/auth/limiter.go` | 81 | 自研令牌桶 | 含在 auth_test ✅ | 少 |
| **测试合计** | **640** | auth 284 / usage 253 / config 103 | | |

**一眼能看出的结论**：最大的文件（proxy.go，占业务代码 28%）**零测试**，次大的 metrics.go 也零测试。这两处正是 F1/F2/F3 三个缺陷的所在地 —— 不是巧合。

---

## 2. 五天阅读路线

| 天 | 主题 | 读什么 | 配套验证 | 已知缺陷 |
|---|---|---|---|---|
| **D1** | 骨架与配置 | `main.go` → `config.go` | 启动、配置边界、fail-closed | — |
| **D2** | 后端池与路由 | `backend.go` → `router.go` | 轮询、健康摘除、model 路由 | **F1** 轮询退化 |
| **D3** | 代理核心（非流式） | `proxy.go` L36–377 | 重试语义、413、延迟指标 | **F2** 延迟失真 |
| **D4** | 流式与计量 | `proxy.go` L378–641 + `usage.go` | SSE、超时守卫、token 计量 | **F3** gzip 失效 |
| **D5** | 治理层与收口 | `auth/*` → `metrics.go` | 鉴权、限流、归因、指标合法性 | 中低项 |

每天结束前跑一次 `python dev/regression.py`，目标是**当天的领域从红色变绿色**。

---

## 3. 分层依赖图

```
                        cmd/gateway/main.go
                    （装配：配置→池→路由→代理→中间件）
                                 │
        ┌────────────┬───────────┼────────────┬─────────────┐
        │            │           │            │             │
     config      backend      router       proxy          auth
   （配置结构）  （健康检查） （model路由）  （核心转发）  （鉴权限流）
        │            │           │            │             │
        │            └───────────┘            │             │
        │                  │                  │             │
        │                  │          ┌───────┴──────┐      │
        │                  │          │              │      │
        │                  │       usage         metrics ◄──┘
        │                  │     （token 解析）  （指标输出）
        │                  │          │              │
        └──────────────────┴──────────┴──────────────┘
                    全部仅依赖 stdlib + yaml.v3

请求路径：
HTTP → [auth.Middleware] → proxy.ServeHTTP → MaxBytesReader → ReadAll
     → ensureUTF8 → detectStreaming → extractModel
     → Route(model) → pool.Next() ──┬─→ handleRequest（非流式）
                                    └─→ handleStreaming（流式）→ streamWatchdog
```

**依赖是单向的**：`auth` 依赖 `metrics`，反向不成立。这条边界要守住 —— 未来加平台化外壳时，外壳依赖内核，内核不能知道外壳存在。

---

## 4. 逐文件导览

### D1 · cmd/gateway/main.go（242 行）

**一句话**：把配置文件里的东西变成活的对象，然后串起来。

| 段落 | 行号 | 看什么 |
|---|---|---|
| flag 解析 | ~L21 | `--config` / `--debug` |
| 配置加载 | ~L40 | 失败即退出，无兜底默认值 |
| 时长解析 | — | `max_body_size` 等解析失败时 **WARN 回退**，不退出 |
| 池与路由装配 | ~L80 | backends → pool → RegisterPool → MapModel |
| **鉴权装配** | ~L130 | `auth.New` 失败即退出（fail-closed） |
| Server 配置 | ~L105 | ⚠️ `WriteTimeout: timeout + 10s` —— 见 D4 |
| 路由注册 | ~L200 | `/health` `/metrics` `/backends` **默认不鉴权**（K8s probe 需要） |

**自检问题**：
1. `/metrics` 是否鉴权由 `protect_metrics` 控制 —— 默认是什么？生产暴露有什么风险？
2. 优雅退出有没有做？`SIGTERM` 时正在进行的流式请求会怎样？
3. `igw.exe` 二进制 10MB 在根目录，`.gitignore` 已排除 —— 确认一下没被提交（`git ls-files` 已验证干净）。

---

### D1 · internal/config/config.go（255 行）

**一句话**：YAML → 结构体，外加安全默认值填充。

| 关键项 | 位置 | 要点 |
|---|---|---|
| `GatewayConfig` | L72 | 6 个控制项：port/timeout/max_retries/max_body_size/stream_idle_timeout/stream_max_duration |
| `AuthConfig` | L25 | enabled/header/keys[] |
| `KeyConfig` | L45 | name/key_hash/rate_limit/burst |
| **`validate()`** | L177 | ⚠️ **只查重名，未查重复 key_hash** |
| `ParseSize()` | L214 | 自实现字节量解析（`"16MB"`/`"1.5G"`），stdlib 没有对应函数 |

**已知缺陷（中低）**：两个 Key 配了相同 `key_hash` → 后加载的静默覆盖前一个的限流参数。**重构验证**：`config_test.go` 里有 fail-closed 用例，但没覆盖这条。

**自检问题**：
1. 为什么 `max_body_size` 缺省要填 16MB 而不是"不限"？（答案：Go 的 `net/http` 默认无上限，任何人能 POST 2GB 打满内存）
2. `auth.enabled=true` 但一个 Key 都没配 → 启动失败。这个 fail-closed 设计对不对？有没有更好的降级方式？

---

### D2 · internal/backend/backend.go（236 行）⚠️ **F1 所在地**

**一句话**：管单个后端的生死，以及一群后端轮着来。

| 函数 | 行号 | 要点 |
|---|---|---|
| `NewBackend` | L32 | ⚠️ 注意健康检查路径各家不同（vLLM `/health`、Ollama `/`） |
| `check()` | L94 | 主动探活，更新 `healthy` 标志 |
| **`Next()`** | L153 | ⚠️ L166 `p.next = (idx+1) % len` **推进游标** |
| **`NextExcluding()`** | L198 | ⚠️ L211 / L221 **也推进同一个游标** |

#### F1 缺陷推演（两个后端，slow 排第一）

```
初始 next=0
请求1  Next()        → idx=0 slow，next=1   ┐
       NextExcluding → idx=1 good，next=0   ┘ next 绕回原点！
请求2  Next()        → idx=0 slow，next=1   ┐
       NextExcluding → idx=1 good，next=0   ┘
请求3  ...           → 又是 slow
```

**实测 4/4 全部先命中同一个慢后端。** 后果：某节点"劣化但健康检查仍通过"（vLLM 并发打满返 429）时，**100% 流量都先打它** —— 轮询本应分摊压力，现在反而全压在坏节点上。

**修复方向**：`NextExcluding` 是"本次请求的补偿行为"，不应影响全局游标位置 → 用局部游标，不写 `p.next`。修复后首次尝试应为 slow/good/slow/good 的 50/50 分布。

**自检问题**：
1. `check()` 的健康判定依据是什么？后端返回 500 会被摘除吗？（注意：健康检查与业务请求是两条独立链路）
2. 所有后端都不健康时，`Next()` 返回 nil → 走 `no_healthy_backend`。这条路径有指标吗？
3. `StopAll()` 用的是 `RLock` 而非 `Lock`（L231）—— 合理吗？

---

### D2 · internal/router/router.go（106 行）

**一句话**：`model 名 → 池名` 的一层 map 查找。

| 函数 | 行号 | 要点 |
|---|---|---|
| `RegisterPool` | L28 | 池不存在时自动创建 |
| `MapModel` | L38 | ⚠️ 重复映射会不会报错？ |
| **`Route`** | L51 | 查不到落 `defaultPool`；默认池也没注册则返回 nil |

**这是项目的差异化卖点**：Nginx 不解析 JSON body，做不到按 model 路由（那篇文章明说要上 Lua）。用 Go 解析 body 做路由 = "为什么推理网关不能只用 Nginx" 的硬论据。

**自检问题**：
1. `default_pool` 没配且没注册时会怎样？返回 nil 之前有没有兜底？
2. model 名大小写敏感吗？是否做了 trim？

---

### D3 · internal/proxy/proxy.go L36–377（非流式路径）⚠️ **F2 所在地**

**一句话**：把一个 HTTP 请求转发给选中的后端，失败就换一个再试。

| 函数 | 行号 | 要点 |
|---|---|---|
| `isInferencePath` | L36 | 路径白名单 |
| `ServeHTTP` | L109 | ⚠️ `MaxBytesReader`（P0 加的）→ `ReadAll` → `ensureUTF8` |
| `ensureUTF8` | L174 | GBK→UTF-8 修复（Windows 客户端问题） |
| `detectStreaming` | L192 | 从 body 里找 `stream:true` |
| `extractModel` | L204 | 从 body 里找 model 字段 |
| `isRetryableStatus` | L227 | 429 / 502 / 503 / 504 |
| **`handleRequest`** | L242 | ⚠️ 重试循环主体 |
| **`copyHeaders`** | L696 | ⚠️ **F3 所在地**（见 D4） |

#### F2 缺陷：延迟指标双重失真

```go
for attempt := 0; attempt <= p.maxRetries; attempt++ {
    ...
    start := time.Now()          // ← L290：在循环内部，每次 attempt 重置！
    resp, err := p.client.Do(proxyReq)
    duration := time.Since(start)
```

`start` 应该写在循环**外面**（衡量"客户端感知的总耗时"）。加上 `RecordRequest` 只在成功路径调用，导致：

| 真相 | 指标显示的 |
|---|---|
| 客户端每次等 **3.02 秒** | `backend_latency_seconds{backend="good"} 0.001055` |
| 降级中的 `slow-503` | **在延迟指标里根本不存在** |

**低估约 2900 倍**。这比没有指标更危险 —— 后端劣化时看板反而显示"1ms，极健康"，告警永远不响。

**自检问题**：
1. 重试时请求体 `body` 被重复使用（`bytes.NewReader(body)`），如果后端消耗了 reader 会怎样？`ContentLength` 显式设置了，但 body 是每次新建 reader —— 安全吗？
2. `tried` 集合在 `maxRetries=0` 时的行为对不对？
3. 首次成功时 `duration` 是否包含了重试的等待时间？（答案：不包含 —— 这就是 bug）

---

### D4 · internal/proxy/proxy.go L378–641（流式路径）

**一句话**：把 SSE 字节流一块块转发给客户端，同时盯着别让连接挂死。

| 函数 | 行号 | 要点 |
|---|---|---|
| **`handleStreaming`** | L378 | ⚠️ 开头就用 `ResponseController` 清写 deadline（P0 修的 bug） |
| **`streamWatchdog`** | L590 | 空闲超时 + 可选总时长，触发时取消 ctx |
| `HealthHandler` | L642 | `/health` |
| `BackendsHandler` | L665 | `/backends` |
| `MetricsHandler` | L689 | `/metrics` |

#### F11 缺陷：流式请求遇 5xx 不换节点

```go
backend := pool.Next()                 // ← L403：只选一次，没有重试循环
...
if resp.StatusCode != http.StatusOK {
    w.WriteHeader(resp.StatusCode)     // ← L475：直接把 503 交给客户端
    return                              // ← L479：return，不换节点
}
```

对比非流式 `handleRequest`：后者有完整的 `for attempt` 循环 + `NextExcluding`，**流式路径完全没有**。

后果：池里只要有一个"健康检查通过但业务层面返 5xx"的节点（vLLM 并发打满的典型表现），流式请求就有 **1/N 的概率直接失败**，而非流式会自动换节点成功。实测 H1 用例：6 次流式 → 3 次 503。

**这个缺陷修起来是安全的**：失败点在 `w.WriteHeader`（L500）**之前**，客户端还没收到任何 token，换节点重发不会产生重复内容。这正是那篇 Nginx 文章讨论的边界 —— **响应体写出之前可重试，之后不可**。修法：给流式补一个对称的、仅在「响应头写出之前」生效的重试循环。

---

**关键历史**：`main.go` 的 `WriteTimeout: timeout+10s` 会在 70 秒硬切长 SSE（Go 的 `WriteTimeout` 在请求头读完时打一次 deadline，全程不重置）。P0 用 `ResponseController.SetWriteDeadline(time.Time{})` 修掉。

**⚠️ 连带约束**：P1 加的鉴权中间件要包装 `ResponseWriter`，这个包装**必须实现 `Flush()` 和 `Unwrap()`**，否则 `NewResponseController` 穿不透到真实连接 → 上面那个修复失效。`auth_test.go` 里有一对对照单测锁死这条。

**自检问题**：
1. 被守卫掐断时会补发 `event: error` —— 客户端能收到吗？为什么不能在掐断时直接改状态码？
2. `idle.Reset()` 前为什么必须先 `Stop()` 并排空 channel？（答：否则定时器残留的到期值会立刻触发下一次 select，把正常流误杀）

---

### D4 · internal/proxy/proxy.go L696 —— copyHeaders ⚠️ **F3 所在地**

```go
for key, values := range src.Header {
    if strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
        continue
    }
    for _, v := range values {
        dst.Header.Add(key, v)   // ← Accept-Encoding 被原样转发
    }
}
```

**Go 的 `http.Transport` 只在「自己添加」Accept-Encoding 时才自动解压**。转发客户端的头 → Transport 不解压 → `TeeReader` 抓到的是 gzip 字节 → JSON 解析必败。

| 客户端 | 网关 response | 网关侧计量 |
|---|---|---|
| 无 `Accept-Encoding` | 无（Transport 自动解压） | ✅ 正常 |
| `gzip, deflate` ← **httpx/requests 默认** | gzip | ❌ `+0`，`usage_missing +1` |
| `gzip` ← curl `--compressed` | gzip | ❌ `+0`，`usage_missing +1` |

**转发完全正确、客户端毫无感知，只有计量悄悄归零。** 而 Python 官方 OpenAI SDK 基于 httpx，默认就带这个头。

**修复方向**：转发时剔除 `Accept-Encoding`（让 Transport 用 gzip 请求并自动解压），计量侧拿到明文；响应侧再决定是否压缩返回。

---

### D4 · internal/usage/usage.go（224 行）

**一句话**：从后端响应里抠出 token 数。

| 函数 | 行号 | 要点 |
|---|---|---|
| `ParseResponse` | L64 | 非流式：整段 JSON |
| `ParseStreamChunk` | L77 | 流式单个 chunk |
| **`StreamScanner`** | L108 | ⚠️ 逐行扫 SSE，**拼接未完成行** |
| `CappedBuffer` | L160 | 8MB 上限保护 |
| **`EnsureStreamOptions`** | L205 | ⚠️ 注入 `include_usage` —— **会改写键顺序**（见下） |

**核心机制**：流式请求**默认不带 usage**（实测 Ollama 23 个 chunk 里 0 个含 usage）。网关自己注入 `stream_options: {include_usage: true}`，末尾那个 `choices: []` + usage 的 chunk 才能被扫到。这是「推理网关 vs 日志分析工具」的分水岭 —— LiteLLM 只能等客户端给，你能主动要。

**已知缺陷（中）**：`EnsureStreamOptions` 走 `map[string]any` 再 Marshal → **键顺序变成字典序**（实测 `{"model","messages",...}` → `{"max_tokens","messages","model",...}`），与它自己的注释「避免重写整个 body 导致字段顺序变化」直接矛盾。实践中无害（JSON 对象无序），但注释在撒谎。

**自检问题**：
1. 为什么 `cached` 序列按「字段是否存在」而非「值 > 0」输出？（答：区分"真没命中"与"后端无此能力"，后者要查 vLLM 的 `--enable-prompt-tokens-details`）
2. SSE 行被一次 TCP Read 切成两半怎么办？→ `pending` 残留拼接，`usage_test.go` 有对照单测。

---

### D5 · internal/auth/auth.go（184 行）

**一句话**：比对 sha256 摘要，给出"这个 Key 是谁"。

| 函数 | 行号 | 要点 |
|---|---|---|
| `New` | L77 | 启动时把配置里的 Key 换算成摘要，明文不驻留内存 |
| `Authenticate` | L117 | 返回 `(*APIKey, reason)` |
| `extract` | L150 | `Bearer` 与 `X-API-Key` 双通道 |
| `HashKey` | L181 | 给用户生成 `key_hash` 用 |

**自检问题**：
1. 为什么用 `ConstantTimeCompare` 而不是 `==`？（时序攻击）
2. 明文 Key 只在配置里出现在内存还是？（启动时换算，之后只有摘要）

---

### D5 · internal/auth/limiter.go（81 行）

自研令牌桶。**为什么不用 `golang.org/x/time/rate`**：
1. 保持零依赖（go.mod 只有 yaml.v3 + x/text）
2. `Allow(now)` 收外部时间戳 → 能用**假时钟做确定性单测**；`x/time/rate` 依赖真实时钟，只能 sleep

**已知缺陷（低）**：`NewLimiter` 里 `var max *time.Timer` 遮蔽了 Go 内置 `max`（Go 1.21+ 内置函数）。不影响正确性，但 `go vet` 之外的静态分析可能会抱怨。

---

### D5 · internal/auth/middleware.go（129 行）

| 关键 | 行号 | 要点 |
|---|---|---|
| **`statusRecorder`** | L86 | ⚠️ **必须**实现 `Flush()` + `Unwrap()`，否则毁 SSE |
| **Key 名传递** | L22 | 走 **context**，不走请求头（请求头会被转发给后端 = 泄露内部标识） |
| 429 响应 | ~L60 | ⚠️ 必须带 `Retry-After`，否则客户端盲重试会把限流放大成重试风暴 |

---

### D5 · internal/metrics/metrics.go（419 行）

**一句话**：把内存里的计数器拼成 Prometheus 文本格式。

| 关注点 | 说明 |
|---|---|
| **17 个指标** | requests / errors / retries / auth_failures / key_requests / key_throttled / prompt_tokens / completion_tokens / cached_tokens / usage_missing / sse_connections / backend_health / backend_latency ... |
| **排序** | `sortedKeysOf()` —— map 遍历随机，不排序会让 `/metrics` 行序跳动 |
| **label 未转义** ⚠️ | model 名带 `"` 时输出 `model="evil"quote"` → **整次 scrape 失败** |
| **零值基线** | 错误类型预先占位，避免"没发生过的错误类型不出现在指标里" |

**已知缺陷（中）**：label 未转义。机制已实证；真实触发需后端接受含引号的 model 名（vLLM/Ollama 会返 404），故标「触发路径待验证」。

**自检问题**：
1. 为什么 token 指标单独建而**不给 `requests_total` 加 key label**？（答：后者已是 model×backend×status_code 三维，再乘 key 会让序列数成倍膨胀；"哪个后端处理"和"哪个 Key 用了"是正交关注点）
2. `usage_missing` 为什么必须有？（答：否则 token 总量下降会被误读成"成本降了"，真相可能是后端不再返回 usage 或流式被大量掐断）

---

## 5. 已知缺陷位置索引

| ID | 严重度 | 位置 | 一句话 | 计划修复日 |
|---|---|---|---|---|
| **F1** | 高 | `backend.go` L166 / L211 / L221 | 双重推进游标 → 轮询退化 | D2 |
| **F2** | 高 | `proxy.go` L290 | `start` 在重试循环内 → 延迟低估 2900 倍 | D3 |
| **F3** | 高 | `proxy.go` copyHeaders L696 | 转发 AE → gzip 计量静默失效 | D4 |
| **F11** | 高 | `proxy.go` `handleStreaming` L403 / L466 | 流式只选一次节点，遇 5xx 不重试 | D4 |
| M1 | 中 | `usage.go` L205 | 键顺序被改成字典序，与注释矛盾 | D5 |
| M2 | 中 | `metrics.go` WriteMetrics | label 未转义 | D5 |
| M3 | 中低 | `config.go` L177 | 重复 key_hash 静默覆盖 | D5 |
| L1–L3 | 低 | proxy.go / limiter.go | 注释与代码矛盾、遮蔽内置 max、gofmt | D5 |

---

## 6. 全局自检清单（读完后抢答）

1. **一次请求从进到出经过哪 10 个步骤？** 能不看代码说出来。
2. **流式和非流式在代码里是两套逻辑还是一套？** 分叉点在哪一行？
3. **模型名是从哪来的？** 如果 body 里没有 model 字段会怎样？
4. **Key 名是怎么从中间件传到 proxy 的？** 为什么不能用请求头？
5. **后端挂了多久会被摘除？** 摘除后多久恢复？
6. **三个配置项 `max_body_size` / `stream_idle_timeout` / `stream_max_duration` 各自的默认值与失效后果是什么？**
7. **如果让你删掉一半代码，你会删什么？**

答不上 1–3 说明主链路没通，重读 D3/D4。
