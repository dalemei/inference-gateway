# GitHub 日更优化路线图

> 目标：持续向本仓库灌入**有价值的 commit**，保持 GitHub 日更 streak。
> 原则：每天 1 个干净 commit，改动小、可编译、可测试。每完成一天在下方表格勾掉。
> 当前进度：**Day 1–3 已完成**（见文末 commit 记录）。

## 进度图例
- ✅ 已完成
- ⬜ 待做
- 🚧 进行中

## 14 天基础路线图

| Day | 状态 | 标题（commit 风格） | 改什么 | 文件 | 难度 |
|-----|------|-------------------|--------|------|------|
| 1 | ✅ | fix: 重试计数器未累加 | 重试循环补 `recordRetry()` | proxy.go | ⭐ |
| 2 | ✅ | fix: 暴露请求级延迟指标 | 新增 `backend_request_latency_seconds` | metrics.go | ⭐ |
| 3 | ✅ | refactor: 移除 SSE body HEX dump | 删/加开关控制调试日志（性能+隐私） | proxy.go L224-232 | ⭐ |
| 4 | ✅ | fix: 优雅关闭改用 Shutdown(ctx) | 流式连接不再被瞬间切断 | main.go L79-86 | ⭐⭐ |
| 5 | ⬜ | test: detectStreaming / ensureUTF8 单测 | 新建 `proxy_test.go` | 新测试文件 | ⭐⭐ |
| 6 | ⬜ | test: BackendPool round-robin 单测 | 全不健康→nil、排除集合逻辑 | 新测试文件 | ⭐⭐ |
| 7 | ⬜ | feat: 新增 inflight 请求数 gauge | ServeHTTP 起止各 ±1（atomic） | proxy.go / metrics.go | ⭐ |
| 8 | ⬜ | fix: Backend.Latency 改为 atomic.Int64 | 修 data race（并发读） | backend.go | ⭐⭐ |
| 9 | ⬜ | perf: 健康检查独立短超时 | 不再复用网关 60s 超时 | backend.go NewBackend | ⭐⭐ |
| 10 | ⬜ | feat: 请求耗时均值指标 | 复用 requestLatency 做 sum/count | metrics.go | ⭐ |
| 11 | ⬜ | test: httptest 假后端集成测试 | 验证转发+重试全链路 | 新测试文件 | ⭐⭐⭐ |
| 12 | ⬜ | fix: 转发时剥离 Accept-Encoding | 避免双重压缩 bug | proxy.go copyHeaders | ⭐⭐ |
| 13 | ⬜ | feat: 编译版本号 + /health 上报 | `go build -ldflags` 注入 | main.go / metrics.go | ⭐ |
| 14 | ⬜ | docs: README 指标数对齐 + Makefile | 文档与代码一致 | README.md / Makefile | ⭐ |

## 进阶功能（路线 B 后期，不必日更）
- 加权 / 最少连接负载均衡（替代纯 round-robin）
- 流式请求断点续传重试（客户端 `id` 去重，已输出 token 不再重发）
- 配置热加载（监听 SIGUSR1 重建 backend 池）
- 令牌桶限流

## 执行约定
- **测试零依赖**：Go 自带 `testing` + `net/http/httptest`，不引第三方。
- **每次改动后必跑**：`go build ./...` 与 `go vet ./...`，均通过再提交。
- **提交后**：`git push` 到远程以保持 streak（先 `git remote -v` 确认远程已配）。
- **一日一 commit**：即使一天能做完多项，也拆成独立 commit，保持节奏清晰。

## 协作分层：学什么 / 委托什么

> 原则：**"为什么这么设计"由人定，"怎么写"交给 AI。** 目标是把人训练成架构师，而非提示词工程师。

### 你该掌握的（设计层 / 必须懂）
- 整体架构：网关在客户端与 vLLM 之间的位置、为什么需要这一层
- 负载均衡为什么用 round-robin + `atomic.Bool` 健康位（热路径无锁读）
- `NextExcluding` 重试排除集：绝不重试到刚失败的节点
- **SSE 为什么不能重试、流式 client 为什么必须 `Timeout:0`**（语义正确性优先）
- 入口 `ensureUTF8`（GBK→UTF8）：替 Windows 客户端背锅的透明修正
- 监控指标怎么设计才有用（对照本仓库已踩的埋点废点）
- 演进判断：什么时候该加限流 / 熔断 / 加权（架构取舍）

### 可以委托 AI 的（实现层）
- 具体 HTTP 转发代码、backend.go 的 boilerplate
- `ensureUTF8` 里 `transform` 标准库调用等细节
- metrics.go 的 Prometheus 文本拼接格式
- 测试的具体断言写法、Makefile / ldflags 注入等机械活

### 对人的硬要求（不可委托）
- **博客亲自写**：用 AI 走读当底稿，但用自己的话重构、补一手踩坑体感（影响力靠真实体感）
- **每个 PR 花 5 分钟看 diff**：尤其 Day 5 之后的测试、Day 8 的 data race 修复——review 是架构师护城河
- **设计权衡自己拍板**：模棱两可处 AI 会自选，出事背锅的是人

### 网关最该吃透的 5 个"为什么"（懂了即"拥有"代码）
1. `atomic.Bool` 健康位 → 热路径无锁读
2. `NextExcluding` 重试排除集 → 绝不重试到刚挂的节点
3. SSE 不重试 + 流式 client `Timeout:0` → 语义正确性优先
4. 显式 `Content-Length` + 剥 `Host` → 躲 chunked 解析坑
5. 入口 `ensureUTF8` → 替 Windows GBK 客户端背锅

## 已完成 commit 记录
- `9f7bdfa` fix: 重试计数器未累加，补充 recordRetry() 调用  (Day 1)
- `f2cad48` fix: 暴露请求级延迟指标                  (Day 2)
- `2168897` refactor: 移除 SSE body HEX dump，调试日志加 -debug 开关 (Day 3)
