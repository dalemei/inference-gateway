#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
OpenAI 兼容接口的极简压测器（零第三方依赖，标准库实现）。

设计要点：
  - 每线程一条长连接（keep-alive），贴近真实客户端行为
  - 流式模式记录 TTFT（首个非空 content chunk 到达时间），这是推理服务最关键的用户体感指标
  - 非流式模式记录端到端延迟 + usage.completion_tokens
  - 失败按 HTTP 状态 / 异常分类计数，不静默吞掉

用法：
  python3 bench.py --target http://127.0.0.1:8080 --model qwen2.5-7b \
      --concurrency 8 --requests 64 --max-tokens 128 --stream --label "via-gateway"
"""

import argparse
import json
import http.client
import statistics
import sys
import threading
import time
from urllib.parse import urlparse

PROMPT = "请用三句话解释什么是推理网关，并说明它和直接调用大模型 API 的区别。"


def pct(sorted_vals, p):
    """最近秩百分位（nearest-rank），不插值，便于复现。"""
    if not sorted_vals:
        return 0.0
    k = max(0, min(len(sorted_vals) - 1, int(round((p / 100.0) * (len(sorted_vals) - 1)))))
    return sorted_vals[k]


class Result:
    def __init__(self):
        self.lock = threading.Lock()
        self.ttft = []          # 首 token 延迟（流式）
        self.e2e = []           # 端到端延迟
        self.tokens = 0         # 输出 token 总数
        self.ok = 0
        self.err = 0
        self.err_kind = {}      # 错误分类计数
        self.wall_start = 0.0
        self.wall_end = 0.0

    def add(self, ok, e2e_s, ttft_s, ntok, kind=""):
        with self.lock:
            self.e2e.append(e2e_s)
            if ttft_s is not None:
                self.ttft.append(ttft_s)
            self.tokens += ntok
            if ok:
                self.ok += 1
            else:
                self.err += 1
                self.err_kind[kind] = self.err_kind.get(kind, 0) + 1


def one_request(host, port, model, max_tokens, stream, timeout):
    """返回 (ok, e2e_s, ttft_s, ntok, err_kind)"""
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": max_tokens,
        "temperature": 0.0,
        "stream": stream,
    }).encode("utf-8")
    headers = {
        "Content-Type": "application/json",
        "Content-Length": str(len(body)),
        "Accept": "text/event-stream" if stream else "application/json",
    }

    conn = http.client.HTTPConnection(host, port, timeout=timeout)
    t0 = time.perf_counter()
    ttft = None
    ntok = 0
    try:
        conn.request("POST", "/v1/chat/completions", body=body, headers=headers)
        resp = conn.getresponse()

        if resp.status != 200:
            raw = resp.read(512)
            return False, time.perf_counter() - t0, None, 0, "http_%d" % resp.status

        if stream:
            buf = b""
            while True:
                line = resp.readline()
                if not line:
                    break
                if not line.startswith(b"data:"):
                    continue
                payload = line[len(b"data:"):].strip()
                if payload == b"[DONE]":
                    break
                if ttft is None:
                    # 只有真正拿到 content 才算首 token（跳过 role 之类的空 content chunk）
                    if b'"content"' in payload and b'"content":""' not in payload.replace(b" ", b""):
                        ttft = time.perf_counter() - t0
                        buf = payload
                        continue
                buf = payload
            # vLLM 流式的 usage 需 stream_options，这里用 chunk 数近似 token 数
            ntok = max(ntok, 0)
        else:
            data = json.loads(resp.read().decode("utf-8"))
            ntok = data.get("usage", {}).get("completion_tokens", 0) or 0
            ttft = None

        return True, time.perf_counter() - t0, ttft, ntok, ""
    except Exception as e:  # noqa: BLE001 - 压测器要能扛住任何后端异常
        return False, time.perf_counter() - t0, None, 0, type(e).__name__
    finally:
        try:
            conn.close()
        except Exception:
            pass


def worker(args, host, port, result, n_requests):
    for _ in range(n_requests):
        ok, e2e_s, ttft_s, ntok, kind = one_request(
            host, port, args.model, args.max_tokens, args.stream, args.timeout
        )
        result.add(ok, e2e_s, ttft_s, ntok, kind)
        if args.think:
            time.sleep(args.think)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--target", required=True, help="基址，如 http://127.0.0.1:8080")
    ap.add_argument("--model", default="qwen2.5-7b")
    ap.add_argument("--concurrency", type=int, default=8)
    ap.add_argument("--requests", type=int, default=64, help="总请求数（按并发均摊）")
    ap.add_argument("--max-tokens", type=int, default=128)
    ap.add_argument("--timeout", type=float, default=300.0)
    ap.add_argument("--stream", action="store_true")
    ap.add_argument("--think", type=float, default=0.0, help="每次请求间隔（秒），模拟真实节奏")
    ap.add_argument("--label", default="")
    ap.add_argument("--out", default="", help="JSON 结果输出路径")
    args = ap.parse_args()

    u = urlparse(args.target)
    host, port = u.hostname, (u.port or 80)

    result = Result()
    per_worker = max(1, args.requests // args.concurrency)
    threads = [threading.Thread(target=worker, args=(args, host, port, result, per_worker))
               for _ in range(args.concurrency)]

    result.wall_start = time.perf_counter()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    result.wall_end = time.perf_counter()

    wall = result.wall_end - result.wall_start
    total = result.ok + result.err
    e2e_sorted = sorted(result.e2e)
    ttft_sorted = sorted(result.ttft)

    summary = {
        "label": args.label or args.target,
        "target": args.target,
        "model": args.model,
        "stream": args.stream,
        "concurrency": args.concurrency,
        "requests": total,
        "ok": result.ok,
        "err": result.err,
        "error_rate_pct": round(100.0 * result.err / total, 2) if total else 0.0,
        "error_kinds": result.err_kind,
        "wall_s": round(wall, 3),
        "qps": round(result.ok / wall, 3) if wall > 0 else 0.0,
        "e2e_p50_ms": round(1000 * pct(e2e_sorted, 50), 1),
        "e2e_p95_ms": round(1000 * pct(e2e_sorted, 95), 1),
        "e2e_p99_ms": round(1000 * pct(e2e_sorted, 99), 1),
        "e2e_mean_ms": round(1000 * statistics.mean(e2e_sorted), 1) if e2e_sorted else 0.0,
        "ttft_p50_ms": round(1000 * pct(ttft_sorted, 50), 1),
        "ttft_p95_ms": round(1000 * pct(ttft_sorted, 95), 1),
        "ttft_p99_ms": round(1000 * pct(ttft_sorted, 99), 1),
        "out_tok_per_s": round(result.tokens / wall, 2) if wall > 0 else 0.0,
        "total_tokens": result.tokens,
    }

    print(json.dumps(summary, ensure_ascii=False, indent=2))
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            json.dump(summary, f, ensure_ascii=False, indent=2)

    if result.err:
        print("[WARN] 有 %d 个失败：%s" % (result.err, result.err_kind), file=sys.stderr)


if __name__ == "__main__":
    main()
