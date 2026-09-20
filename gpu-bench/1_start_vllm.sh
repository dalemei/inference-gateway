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

# ---- 0. 校验 vllm 可用 ----
# 早失败比晚失败重要得多：venv 没激活时若直接往下走，会白白等到 10 分钟超时
# 才知道挂了（本机 fixed bug 时踩过：进程秒退但 wait_ready 仍傻等满 120×5s）。
if command -v vllm >/dev/null 2>&1; then
  echo "[vLLM] $(command -v vllm)  $(vllm --version 2>/dev/null | head -1)"
else
  echo "[FATAL] 找不到 vllm 命令 —— venv 未激活或不在预期路径。"
  echo "        任选一种："
  echo "          source <你的venv>/bin/activate && bash $0"
  echo "          VENV_PATH=<你的venv>/bin/activate bash $0"
  echo "        当前 PATH 下无 vllm，已搜过: /root/vllm-env, /root/autodl-tmp/vllm-env"
  exit 1
fi

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
#
# ready 判据：/health 返回 200 **且** /v1/models 能列出模型名。
# 两个都不能省：
#   1) vLLM 的 /health 返回的是**空 body**（Response(status_code=200)），
#      所以绝不能用 `curl | grep -q .` 判断——空 body 会让 grep 永远失败，
#      脚本会空转 120×5s=10 分钟后误报启动超时。必须判 HTTP 状态码。
#   2) 只判 /health 也不够：它在端口 bind 后**立刻**返回 200，此时模型权重
#      可能还在加载。压测若抢在加载完成前发请求，会拿到一批 500/503，
#      把实验数据直接污染掉。/v1/models 能列出模型名才是真的可服务。
ready() {
  local port=$1
  [ "$(curl -s -o /dev/null -w '%{http_code}' -m 3 "http://127.0.0.1:$port/health")" = "200" ] || return 1
  curl -s -m 5 "http://127.0.0.1:$port/v1/models" | grep -q "$SERVED_NAME" || return 1
  return 0
}

wait_ready() {
  local port=$1 i pid pidfile="$WORKDIR/vllm-$port.pid"
  for i in $(seq 1 120); do   # 最多等 10 分钟
    if ready "$port"; then
      echo "[vLLM :$port] ready (等待 $((i*5))s)"
      return 0
    fi
    # 进程已退出却始终没 ready → 立即失败，不要傻等满 120 轮。
    # vLLM 参数写错（如子命令不存在）时会秒退，但端口探测看不出来，
    # 不查 PID 就会白烧 10 分钟机时才发现。
    if [ -f "$pidfile" ]; then
      pid=$(cat "$pidfile" 2>/dev/null || echo "")
      if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
        echo "[FATAL] :$port 进程(pid=$pid)已退出且未 ready"
        tail -30 "$WORKDIR/logs/vllm-$port.log"
        return 1
      fi
    fi
    sleep 5
  done
  echo "[FATAL] :$port 启动超时，日志: $WORKDIR/logs/vllm-$port.log"
  tail -30 "$WORKDIR/logs/vllm-$port.log"
  return 1
}

# ---- 3. 逐个启动副本 ----
# 命令与参数按 AutoDL 实机 **vLLM 0.26.0** 固化，不做跨版本通用适配。
# 三条都是实机报错换来的，不是从文档推断的：
#   1) 子命令是 `serve`，**没有** `server`（0.26.0 子命令表：chat/complete/serve/launch/bench/collect-env/run-batch）
#   2) 模型用**位置参数**，`--model` 会 WARNING 且将来移除
#   3) **没有** `--disable-log-requests`（0.19+ 已移除，硬传报 unrecognized arguments 后秒退）
# 换版本时只改下面这条 nohup 命令即可。
for ((i=0; i<REPLICAS; i++)); do
  port=$((BASE_PORT + i))
  if ready "$port"; then
    echo "[vLLM :$port] 已在运行，跳过"
    continue
  fi
  echo "[启动] vLLM 实例 :$port  (gpu_util=$GPU_UTIL, max_len=$MAX_LEN)"
  # 清掉上一轮的 pid 文件，否则 wait_ready 的存活检查会读到旧 pid 而误判“进程已退出”
  rm -f "$WORKDIR/vllm-$port.pid"
  nohup vllm serve "$MODEL_DIR" \
      --served-model-name "$SERVED_NAME" \
      --port "$port" \
      --gpu-memory-utilization "$GPU_UTIL" \
      --max-model-len "$MAX_LEN" \
      > "$WORKDIR/logs/vllm-$port.log" 2>&1 &
  echo $! > "$WORKDIR/vllm-$port.pid"
  wait_ready "$port" || exit 1
done

# ---- 4. 自检 ----
echo "=== 副本状态 ==="
for ((i=0; i<REPLICAS; i++)); do
  port=$((BASE_PORT + i))
  echo -n ":$port -> "
  curl -s -m 3 "http://127.0.0.1:$port/v1/models" | head -c 200; echo
done
echo "=== 显存 ==="
nvidia-smi --query-gpu=memory.used,memory.total --format=csv
