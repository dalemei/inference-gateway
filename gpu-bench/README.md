# AutoDL GPU 实测方案（vLLM + 推理网关）

> 原则：**只做本机 Ollama 做不了的实验**。功能验证（转发/路由/指标/韧性）已在本地零成本跑通，
> GPU 机时不用来重复验证，而是用来产出 **Only-GPU 能拿到的量化数据**——这是博客与 README 的核心资产。

## 三个实验（按顺序跑，总耗时预算 ~1.5h ≈ ¥2.5）

| # | 实验 | 回答的问题 | 产出 | 预算 |
|---|------|-----------|------|------|
| 1 | **网关开销** | 多一层转发，到底损耗多少 TTFT / 吞吐？ | 开销百分比，判定网关是不是瓶颈 | 20 min |
| 2 | **并发曲线** | 并发 1→32，直连 vs 网关的 p95 / 吞吐对比 | 可放进 README 的表格 | 30 min |
| 3 | **故障摘除** | 真·副本挂掉（满载 503 / 进程死），网关多久摘除、丢多少请求 | 故障窗口量化，暴露架构演进点 | 30 min |

判定标准（先写下来，避免事后自我说服）：
- 实验 1：网关引入的端到端延迟开销 **< 2%**，且 TTFT 差值 **< 10ms** → 网关不是瓶颈，可以对外宣称。
- 实验 2：并发 ≥ 8 时网关吞吐 **不低于直连的 95%**。
- 实验 3：故障窗口内失败率量化出来，不管数字多难看，**照实记录**——难看的数字才是下一步架构演进的选题。

## 环境前提（AutoDL RTX 4080 SUPER 32GB）

- vLLM 0.26.0，venv `vllm-env`，数据盘 `/root/autodl-tmp`（50G，关机保留）
- 模型默认 `Qwen2.5-7B-Instruct-AWQ`（权重 ~5.5G），**2 副本**，各占 42% 显存
- 网关与 vLLM **同机部署**（localhost），测出的就是纯转发开销，不含网络跳

## 执行顺序

```bash
cd /root/autodl-tmp/inference-gateway/gpu-bench   # 或 git pull 后进入

source env.sh          # 所有变量在这里，改模型/副本数只动这个文件
bash 1_start_vllm.sh   # 下载模型 + 起 2 个 vLLM 实例（首次含下载，约 10-20 min）
bash 2_start_gateway.sh # 编译 + 起网关，确认 healthy_backends=2
bash 3_compare.sh      # 实验 1 + 2，结果写 results/
bash 4_failover.sh     # 实验 3
```

## 成本纪律

- **先跑 3_compare.sh，确认拿到数据再跑 4_failover.sh**（后者会杀进程，可能要重启实例）。
- 全部跑完立刻关机；`results/` 与模型都在 `/root/autodl-tmp`，关机不丢。
- 卡住超过 10 分钟无进展 → 先看 `nvidia-smi` 和 vLLM 日志，不要空烧机时。

## 已知坑

| 坑 | 现象 | 解法 |
|---|------|------|
| 后端 URL 带 `/v1` | 转发 404 `/v1/v1/...` | URL 只填根地址，见 `config.gpu.yaml` 注释 |
| vLLM 健康检查 | 有些版本 `/health` 返回慢 | 用 `health_check_path: /health` + `health_check_timeout: 5s` |
| 两实例同起 | 显存探测打架 | 脚本已串行启动 + 逐个等 ready |
| HF 下载慢 | 卡在 download | 脚本已设 `HF_ENDPOINT=hf-mirror.com`，失败回退 modelscope |
