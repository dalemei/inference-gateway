#!/usr/bin/env bash
# 实验 3：真·副本故障下的摘除 / 重试 / 恢复
# 量化「健康检查间隔 vs 故障窗口」——不管数字多难看都照实记录
set -u
cd "$(dirname "$0")"
source ./env.sh

GW="http://127.0.0.1:$GW_PORT"
KILL_PORT=$((BASE_PORT + 1))      # 干掉第二个副本
STAMP=$(date +%Y%m%d-%H%M%S)
LOG="$RESULT_DIR/${STAMP}-failover.txt"

snap() { curl -s -m 5 "$GW/metrics" | grep -E "inference_gateway_(requests|retries|errors)_total"; }
health() { curl -s -m 5 "$GW/health"; }

echo "=== 故障前 ===" | tee -a "$LOG"
echo "health: $(health)" | tee -a "$LOG"
BEFORE=$(snap)
echo "$BEFORE" | tee -a "$LOG"

# ---- 后台持续压测（低并发，持续 ~40s）----
echo | tee -a "$LOG"
echo "=== 启动背景压测（c=4，100 请求）===" | tee -a "$LOG"
python3 "$PWD/bench.py" --target "$GW" --model "$SERVED_NAME" \
    --concurrency 4 --requests 100 --max-tokens 64 --stream \
    --label "failover" --out "$RESULT_DIR/${STAMP}-failover.json" \
    > "$RESULT_DIR/${STAMP}-failover-stdout.txt" 2>&1 &
BENCH_PID=$!

sleep 8
echo | tee -a "$LOG"
echo "=== [t+8s] kill :$KILL_PORT ===" | tee -a "$LOG"
pkill -f -- "--port $KILL_PORT" || echo "pkill 未匹配到进程（可能已经挂了）"
date +"%H:%M:%S kill 已发出" | tee -a "$LOG"

# ---- 观察摘除过程（每 2s 采一次 healthy_backends）----
for i in $(seq 1 10); do
  sleep 2
  echo "[t+$((8 + i*2))s] $(health)" | tee -a "$LOG"
done

# ---- 恢复 ----
echo | tee -a "$LOG"
echo "=== 重启 :$KILL_PORT ===" | tee -a "$LOG"
if [ -n "${VENV_PATH:-}" ]; then source "$VENV_PATH";
elif [ -f /root/vllm-env/bin/activate ]; then source /root/vllm-env/bin/activate;
elif [ -f /root/autodl-tmp/vllm-env/bin/activate ]; then source /root/autodl-tmp/vllm-env/bin/activate; fi
nohup python -m vllm.entrypoints.openai.api_server \
    --model "$MODEL_DIR" --served-model-name "$SERVED_NAME" --port "$KILL_PORT" \
    --gpu-memory-utilization "$GPU_UTIL" --max-model-len "$MAX_LEN" --disable-log-requests \
    > "$WORKDIR/logs/vllm-$KILL_PORT.log" 2>&1 &

for i in $(seq 1 20); do
  sleep 3
  h=$(health)
  echo "[恢复 +$((i*3))s] $h" | tee -a "$LOG"
  echo "$h" | grep -q '"healthy_backends":2' && { echo "[OK] 副本自动加回" | tee -a "$LOG"; break; }
done

wait $BENCH_PID
echo | tee -a "$LOG"
echo "=== 故障后指标 ===" | tee -a "$LOG"
AFTER=$(snap)
echo "$AFTER" | tee -a "$LOG"

echo | tee -a "$LOG"
echo "=== 压测结果（故障窗口内的表现）===" | tee -a "$LOG"
cat "$RESULT_DIR/${STAMP}-failover.json" | tee -a "$LOG"

echo | tee -a "$LOG"
echo "=== 网关日志：重试 / 503 相关 ===" | tee -a "$LOG"
grep -E "重试|换节点|503|不健康|unhealthy" "$WORKDIR/logs/gateway.log" | tail -20 | tee -a "$LOG"

echo
echo "完整记录：$LOG"
