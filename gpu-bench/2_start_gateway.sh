#!/usr/bin/env bash
# 编译并启动网关（优先用本机交叉编译好的 igw-linux-amd64，避免 AutoDL 上装 Go）
set -u
cd "$(dirname "$0")"
source ./env.sh
cd "$REPO_DIR"

BIN="$REPO_DIR/igw-linux-amd64"

if [ ! -x "$BIN" ]; then
  if command -v go >/dev/null 2>&1; then
    echo "[构建] 本机 go build"
    GOOS=linux GOARCH=amd64 go build -o "$BIN" ./cmd/gateway || exit 1
  else
    echo "[FATAL] 缺 $BIN 且无 Go 环境。"
    echo "        在本机执行："
    echo "        cd <repo> && GOOS=linux GOARCH=amd64 go build -o igw-linux-amd64 ./cmd/gateway"
    echo "        然后把 igw-linux-amd64 上传/拷贝到 $REPO_DIR 再重跑本脚本。"
    exit 1
  fi
fi

# 停掉旧网关
pkill -f "igw-linux-amd64" 2>/dev/null; sleep 1

echo "[启动] 网关 :$GW_PORT"
nohup "$BIN" --config "$REPO_DIR/gpu-bench/config.gpu.yaml" \
    > "$WORKDIR/logs/gateway.log" 2>&1 &
echo $! > "$WORKDIR/gateway.pid"
sleep 4

echo "=== 网关健康 ==="
curl -s -m 5 "http://127.0.0.1:$GW_PORT/health"; echo
echo "=== 后端列表 ==="
curl -s -m 5 "http://127.0.0.1:$GW_PORT/backends"; echo
echo "=== 启动日志 ==="
tail -20 "$WORKDIR/logs/gateway.log"
