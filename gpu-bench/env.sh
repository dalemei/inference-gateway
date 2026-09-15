# 实测环境变量：改模型 / 副本数 / 端口，只动这个文件
# 用法：source env.sh

# ---- 模型 ----
# 默认 AWQ 7B × 2 副本：权重 ~5.5G，各占 42% 显存（32G 卡），余量给 KV cache
# 想省下载时间就切成小模型：MODEL_NAME=Qwen/Qwen2.5-1.5B-Instruct  GPU_UTIL=0.15  MAX_LEN=2048
MODEL_REPO="${MODEL_REPO:-Qwen/Qwen2.5-7B-Instruct-AWQ}"
SERVED_NAME="${SERVED_NAME:-qwen2.5-7b}"
MODEL_DIR="${MODEL_DIR:-/root/autodl-tmp/models/$(basename $MODEL_REPO)}"

# ---- 副本 ----
REPLICAS="${REPLICAS:-2}"
BASE_PORT="${BASE_PORT:-8000}"          # 8000, 8001, ...
GPU_UTIL="${GPU_UTIL:-0.42}"            # 每个实例占用总显存比例
MAX_LEN="${MAX_LEN:-4096}"

# ---- 网关 ----
GW_PORT="${GW_PORT:-8080}"
GW_RETRIES="${GW_RETRIES:-1}"
HC_INTERVAL="${HC_INTERVAL:-5s}"        # 健康检查间隔（实验 3 会量化「间隔 vs 故障窗口」）
HC_TIMEOUT="${HC_TIMEOUT:-5s}"

# ---- 路径 ----
WORKDIR="${WORKDIR:-/root/autodl-tmp/ig-bench}"
REPO_DIR="${REPO_DIR:-/root/autodl-tmp/inference-gateway}"
RESULT_DIR="${RESULT_DIR:-$WORKDIR/results}"

# ---- 下载源（国内优先）----
export HF_ENDPOINT="${HF_ENDPOINT:-https://hf-mirror.com}"
export VLLM_WORKER_MULTIPROC_METHOD=spawn

mkdir -p "$WORKDIR/logs" "$RESULT_DIR"
