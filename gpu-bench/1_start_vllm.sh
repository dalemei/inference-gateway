#!/usr/bin/env bash
# 起 N 个 vLLM 实例（串行启动，逐个等 ready）
set -u
cd "$(dirname "$0")"
source ./env.sh

# 激活 venv（按实机调整 VENV_PATH）
if [ -n "${VENV_PATH:-}" ]; then
  source "$VENV_PATH"
elif [ -f /root/vllm-env/bin/activate ]; then
  source /root/vllm-env/bin/activate
elif [ -f /root/autodl-tmp/vllm-env/bin/activate ]; then
  source /root/autodl-tmp/vllm-env/bin/activate
fi

echo "=== GPU 状态 ==="
nvidia-smi --query-gpu=name,memory.total,memory.used --format=csv

# ---- 1. 模型就绪 ----
if [ -d "$MODEL_DIR" ] && [ -f "$MODEL_DIR/config.json" ]; then
  echo "[模型] 已存在: $MODEL_DIR"
else
  echo "[模型] 下载 $MODEL_REPO -> $MODEL_DIR"
  mkdir -p "$MODEL_DIR"
  if huggingface-cli download "$MODEL_REPO" --local-dir "$MODEL_DIR"; then
    echo "[模型] HF 镜像下载完成"
  else
    echo "[模型] HF 失败，回退 modelscope"
    pip install -q modelscope 2>/dev/null
    python -c "from modelscope import snapshot_download; snapshot_download('$MODEL_REPO', local_dir='$MODEL_DIR')" \
      || { echo "[FATAL] 模型下载失败，检查网络或手动指定 MODEL_DIR"; exit 1; }
  fi
fi

# ---- 2. 逐个启动副本 ----
wait_ready() {
  local port=$1 i
  for i in $(seq 1 120); do   # 最多等 10 分钟
    if curl -s -m 3 "http://127.0.0.1:$port/health" | grep -q .; then
      echo "[vLLM :$port] ready (等待 $((i*5))s)"
      return 0
    fi
    sleep 5
  done
  echo "[FATAL] :$port 启动超时，日志: $WORKDIR/logs/vllm-$port.log"
  tail -30 "$WORKDIR/logs/vllm-$port.log"
  return 1
}

for ((i=0; i<REPLICAS; i++)); do
  port=$((BASE_PORT + i))
  if curl -s -m 3 "http://127.0.0.1:$port/health" >/dev/null 2>&1; then
    echo "[vLLM :$port] 已在运行，跳过"
    continue
  fi
  echo "[启动] vLLM 实例 :$port  (gpu_util=$GPU_UTIL, max_len=$MAX_LEN)"
  nohup python -m vllm.entrypoints.openai.api_server \
      --model "$MODEL_DIR" \
      --served-model-name "$SERVED_NAME" \
      --port "$port" \
      --gpu-memory-utilization "$GPU_UTIL" \
      --max-model-len "$MAX_LEN" \
      --disable-log-requests \
      > "$WORKDIR/logs/vllm-$port.log" 2>&1 &
  echo $! > "$WORKDIR/vllm-$port.pid"
  wait_ready "$port" || exit 1
done

# ---- 3. 自检 ----
echo "=== 副本状态 ==="
for ((i=0; i<REPLICAS; i++)); do
  port=$((BASE_PORT + i))
  echo -n ":$port -> "
  curl -s -m 3 "http://127.0.0.1:$port/v1/models" | head -c 200; echo
done
echo "=== 显存 ==="
nvidia-smi --query-gpu=memory.used,memory.total --format=csv
