# GitHub 日更优化路线图

> 目标：持续向本仓库灌入**有价值的 commit**，保持 GitHub 日更 streak。
> 原则：每天 1 个干净 commit，改动小、可编译、可测试。每完成一天在下方表格勾掉。
> 当前进度：**Day 1–2 已完成**（见文末 commit 记录）。

## 进度图例
- ✅ 已完成
- ⬜ 待做
- 🚧 进行中

## 14 天基础路线图

| Day | 状态 | 标题（commit 风格） | 改什么 | 文件 | 难度 |
|-----|------|-------------------|--------|------|------|
| 1 | ✅ | fix: 重试计数器未累加 | 重试循环补 `recordRetry()` | proxy.go | ⭐ |
| 2 | ✅ | fix: 暴露请求级延迟指标 | 新增 `backend_request_latency_seconds` | metrics.go | ⭐ |
| 3 | ⬜ | refactor: 移除 SSE body HEX dump | 删/加开关控制调试日志（性能+隐私） | proxy.go L224-228 | ⭐ |
| 4 | ⬜ | fix: 优雅关闭改用 Shutdown(ctx) | 流式连接不再被瞬间切断 | main.go L79-86 | ⭐⭐ |
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

## 已完成 commit 记录
- `9f7bdfa` fix: 重试计数器未累加，补充 recordRetry() 调用  (Day 1)
- `f2cad48` fix: 暴露请求级延迟指标                  (Day 2)
