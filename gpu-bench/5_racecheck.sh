#!/usr/bin/env bash
# 用 -race 构建并压一轮，验证无 data race
# 为什么放到 GPU 机上跑：Windows 本机没有 gcc，Go 的 -race 需要 cgo；Linux 容器自带 gcc
set -u
cd "$(dirname "$0")"
source ./env.sh
cd "$REPO_DIR"

if ! command -v gcc >/dev/null 2>&1; then
  echo "[SKIP] 无 gcc，跳过 race 检测（apt-get install -y gcc 后可重跑）"
  exit 0
fi

echo "=== 构建 -race 版本 ==="
CGO_ENABLED=1 go build -race -o igw-race ./cmd/gateway || { echo "[FAIL] 构建失败"; exit 1; }

pkill -f igw-linux-amd64 2>/dev/null
pkill -f igw-race 2>/dev/null
sleep 1

export GORACE="halt_on_error=0 log_path=$WORKDIR/logs/race"
nohup ./igw-race --config "$REPO_DIR/gpu-bench/config.gpu.yaml" \
    > "$WORKDIR/logs/race-stdout.log" 2>&1 &
echo $! > "$WORKDIR/race.pid"
sleep 5

echo "=== 并发读指标（触发 Latency 并发读）+ 压一轮 ==="
for i in $(seq 1 30); do
  curl -s -m 5 "http://127.0.0.1:$GW_PORT/metrics"  -o /dev/null &
  curl -s -m 5 "http://127.0.0.1:$GW_PORT/backends" -o /dev/null &
done
python3 "$REPO_DIR/gpu-bench/bench.py" --target "http://127.0.0.1:$GW_PORT" \
    --model "$SERVED_NAME" --concurrency 8 --requests 32 --max-tokens 32 --stream > /dev/null
wait
sleep 2

echo "=== 结论 ==="
if grep -q "DATA RACE" "$WORKDIR/logs/race-stdout.log" \
   || ls "$WORKDIR/logs/race".* >/dev/null 2>&1; then
  echo "[FAIL] 检出 data race，详见 $WORKDIR/logs/race*"
  grep -A 20 "DATA RACE" "$WORKDIR/logs/race"* "$WORKDIR/logs/race-stdout.log" 2>/dev/null | head -40
  pkill -f igw-race
  exit 1
else
  echo "[PASS] 未检出 data race（README 可标注 race-clean）"
fi

pkill -f igw-race 2>/dev/null
rm -f "$REPO_DIR/igw-race"
