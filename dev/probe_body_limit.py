#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""
诊断：max_body_size 超限时，客户端到底能不能收到干净的 413。

背景：回归用例 B1 用 20MB body 打网关，得到 ConnectionResetError(10054) 而非 413。
需要分清两件事：
  1. 网关侧有没有正确「拒绝」（写 413 / 记日志 / 记 metrics）
  2. 客户端侧能不能「读到」这个 413
这两件事可以不一致 —— Go net/http 在 handler 返回后只 drain 有限字节，
剩余请求体没读完就关连接，正在发送中的客户端会先吃到 RST。

用法：
    python dev/probe_body_limit.py
"""

import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import regression as R  # noqa: E402

MB = 1024 * 1024
AUTH = {"Authorization": "Bearer " + R.KEY_ALPHA}
ERR_METRIC = "inference_gateway_errors_total"
ERR_TYPE = "request_body_too_large"


def post(payload, headers=None, timeout=120):
    url = "http://127.0.0.1:%d/v1/chat/completions" % R.GW_PORT
    req = urllib.request.Request(url, data=payload, method="POST")
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    t0 = time.time()
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
        return {"ok": True, "status": resp.status,
                "body": resp.read()[:160], "t": time.time() - t0}
    except urllib.error.HTTPError as e:
        return {"ok": True, "status": e.code,
                "body": e.read()[:160], "t": time.time() - t0}
    except Exception as e:
        return {"ok": False, "err": repr(e), "t": time.time() - t0}


def err_delta(m0, m1):
    def s(m):
        return sum(v for (n, l), v in m.items()
                   if n == ERR_METRIC and ERR_TYPE in l)
    return s(m1) - s(m0)


def probe(log_path, label, total_len):
    body = json.dumps({
        "model": "qwen2.5:1.5b",
        "messages": [{"role": "user", "content": "x" * total_len}],
    }).encode()

    m0 = R.get_metrics()
    off = os.path.getsize(log_path)
    t0 = time.time()
    r = post(body, AUTH)
    m1 = R.get_metrics()
    newlog = R.read_log_from(log_path, off)

    print("\n=== %s  (body %.2f MB, 上限 16 MB) ===" % (label, len(body) / MB))
    if r["ok"]:
        print("  客户端收到 : HTTP %s" % r["status"])
        print("  响应体片段 : %r" % r["body"])
    else:
        print("  客户端异常 : %s" % r["err"])
    print("  往返耗时   : %.2fs" % r["t"])
    print("  网关 metrics %s{type=%s} 增量: %+g"
          % (ERR_METRIC, ERR_TYPE, err_delta(m0, m1)))
    hit = [l.strip() for l in newlog.splitlines()
           if "请求体" in l or "too large" in l.lower() or "413" in l]
    if hit:
        for l in hit[:3]:
            print("  网关日志   : %s" % l)
    else:
        print("  网关日志   : (无相关记录)")


def main():
    servers = R.start_backends()
    tmpdir = tempfile.mkdtemp(prefix="igw_body_")
    exe = R.build_gateway(tmpdir)
    proc, log_path = R.start_gateway(exe, tmpdir)
    print("网关日志: %s" % log_path)
    try:
        # 基线：刚好在上限内
        probe(log_path, "16MB - 1KB（限内基线）", 16 * MB - 1024)
        # 小幅超限：客户端剩余未发完的字节很少
        probe(log_path, "16MB + 64KB（小幅超限）", 16 * MB + 64 * 1024)
        probe(log_path, "16MB + 1MB（中度超限）", 16 * MB + 1 * MB)
        # 回归脚本当前用的量级
        probe(log_path, "20MB（回归脚本 B1 用的量级）", 20 * MB)

        # 关键：B1 在回归里偶发 ConnectionResetError，单独跑却成功。
        # 重复打同一量级，判断这是 flaky 还是必现。
        N = 10
        clean = 0
        for i in range(N):
            body = json.dumps({
                "model": "qwen2.5:1.5b",
                "messages": [{"role": "user", "content": "x" * 20 * MB}],
            }).encode()
            r = post(body, AUTH)
            if r["ok"] and r["status"] == 413:
                clean += 1
            else:
                print("  第 %d 次非 413：%s"
                      % (i + 1, r.get("err") or ("HTTP %s" % r["status"])))
        print("\n20MB × %d 次：收到干净 413 的次数 = %d / %d" % (N, clean, N))
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except Exception:
            proc.kill()
        for s in servers:
            s.shutdown()
            s.server_close()


if __name__ == "__main__":
    main()
