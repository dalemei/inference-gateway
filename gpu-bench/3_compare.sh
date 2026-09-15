#!/usr/bin/env bash
# 实验 1 + 2：直连单副本 vs 经网关（同机，纯转发开销）
set -u
cd "$(dirname "$0")"
source ./env.sh

GW="http://127.0.0.1:$GW_PORT"
DIRECT="http://127.0.0.1:$BASE_PORT"
STAMP=$(date +%Y%m%d-%H%M%S)

# ---- 0. 预热：让 vLLM 完成 CUDA graph 捕获 / KV cache 稳定 ----
echo "=== 0. 预热 ==="
for t in "$DIRECT" "$GW"; do
  python3 "$PWD/bench.py" --target "$t" --model "$SERVED_NAME" \
      --concurrency 2 --requests 4 --max-tokens 32 > /dev/null 2>&1
  echo "  $t 预热完成"
done

# ---- 1. 固定并发对比（流式，测 TTFT）----
echo "=== 1. 固定并发 c=8，流式，64 请求 ==="
python3 "$PWD/bench.py" --target "$DIRECT" --model "$SERVED_NAME" \
    --concurrency 8 --requests 64 --max-tokens 128 --stream \
    --label "direct" --out "$RESULT_DIR/${STAMP}-direct-c8.json" | tail -5
python3 "$PWD/bench.py" --target "$GW" --model "$SERVED_NAME" \
    --concurrency 8 --requests 64 --max-tokens 128 --stream \
    --label "gateway" --out "$RESULT_DIR/${STAMP}-gw-c8.json" | tail -5

# ---- 2. 并发曲线 ----
echo "=== 2. 并发曲线 ==="
for c in 1 4 8 16 32; do
  n=$((c * 8))
  echo "--- concurrency=$c requests=$n ---"
  python3 "$PWD/bench.py" --target "$DIRECT" --model "$SERVED_NAME" \
      --concurrency $c --requests $n --max-tokens 128 --stream \
      --label "direct-c$c" --out "$RESULT_DIR/${STAMP}-direct-c$c.json" > /dev/null
  python3 "$PWD/bench.py" --target "$GW" --model "$SERVED_NAME" \
      --concurrency $c --requests $n --max-tokens 128 --stream \
      --label "gateway-c$c" --out "$RESULT_DIR/${STAMP}-gw-c$c.json" > /dev/null
  echo "  done"
done

# ---- 3. 汇总表 ----
echo "=== 3. 汇总 ==="
python3 - "$RESULT_DIR" "$STAMP" <<'PY'
import json, os, sys
d, stamp = sys.argv[1], sys.argv[2]

def load(name):
    p = os.path.join(d, "%s-%s.json" % (stamp, name))
    if not os.path.exists(p):
        return None
    with open(p, encoding="utf-8") as f:
        return json.load(f)

print("\n| 场景 | 并发 | QPS | E2E p50 | E2E p95 | TTFT p50 | TTFT p95 | 错误率 |")
print("|---|---|---|---|---|---|---|---|")
for c in [1, 4, 8, 16, 32]:
    a = load("direct-c%d" % c)
    b = load("gw-c%d" % c)
    if not a or not b:
        continue
    def row(tag, x):
        return "| %s | %d | %.2f | %.0fms | %.0fms | %.0fms | %.0fms | %.1f%% |" % (
            tag, c, x["qps"], x["e2e_p50_ms"], x["e2e_p95_ms"],
            x["ttft_p50_ms"], x["ttft_p95_ms"], x["error_rate_pct"])
    print(row("直连", a))
    print(row("经网关", b))

a, b = load("direct-c8"), load("gw-c8")
if a and b:
    def delta(k):
        if a[k] == 0:
            return "n/a"
        return "%+.1f%%" % (100.0 * (b[k] - a[k]) / a[k])
    print("\n**网关开销（c=8）**：QPS %s | E2E p50 %s | TTFT p50 %s" %
          (delta("qps"), delta("e2e_p50_ms"), delta("ttft_p50_ms")))
    print("判定：QPS 降幅 < 5%% 且 TTFT 增幅 < 10ms 即视为网关非瓶颈。")
PY

echo
echo "结果 JSON 在 $RESULT_DIR/${STAMP}-*.json"
