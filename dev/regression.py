#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""
inference-gateway 功能回归套件

一条命令把网关的「全部功能」跑一遍，输出 PASS / FAIL / KNOWN 表格。

用法：
    python dev/regression.py               # 完整回归
    python dev/regression.py --keep        # 结束后保留网关日志
    python dev/regression.py --group g     # 只跑某一组

设计要点：
1. 自带假后端，不依赖 Ollama / vLLM / 网络，任何机器上都能跑。
2. 判据一律取自「网关自己的可观测面」（HTTP 响应 + /metrics + 网关日志），
   绝不取客户端观感 —— 客户端自己会解压 gzip，用客户端能否看到 usage
   当判据会得出反向结论（这个坑实际踩过一次）。
3. 已知缺陷（F1/F2/F3）的用例标记为 KNOWN：失败时显示为 KNOWN 而非 FAIL，
   修复后会自动变成 FIXED。周五的目标是 KNOWN 清零。
"""

import argparse
import gzip
import hashlib
import http.server
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

GW_PORT = 19200
# 注：早先这里还有个 "unhealthy" 角色（健康检查恒返 500），但它从未被写进
# CONFIG —— 起了服务却没人引用，制造出「健康检查已覆盖」的假象。现已删除，
# 不健康的角色统一由 flaky 扮演（且是真被配置引用的）。
PORTS = {"good": 19201, "slow503": 19202, "nousage": 19203, "flaky": 19205}

# 可控健康状态：供「健康检查摘除 / 恢复」用例在运行时切换。
# flaky 后端始终存在，只是健康检查结果由这里决定，这样才能验证
# 「摘除 → 恢复 → 再摘除」的完整闭环，而不只是看一眼初始状态。
FLAKY_UP = {"up": False}
FLAKY_HITS = {"n": 0}

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# API Key（明文不落 sensitve 前缀，避免触发各类密钥扫描）
KEY_ALPHA = "alpha-token-2026"
KEY_BETA = "beta-token-2026"
KEY_GAMMA = "gamma-token-2026"


def sha256hex(s):
    return hashlib.sha256(s.encode()).hexdigest()


def free_port(p):
    try:
        s = socket.socket()
        s.bind(("127.0.0.1", p))
        s.close()
        return True
    except OSError:
        return False


# ============================================================================
# 假后端
# ============================================================================

def make_json():
    return {
        "id": "chatcmpl-fake", "object": "chat.completion", "created": 0,
        "model": "qwen2.5:1.5b",
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": "hello from fake backend"},
            "finish_reason": "stop",
        }],
        "usage": {
            "prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18,
            "prompt_tokens_details": {"cached_tokens": 4},
        },
    }


def make_json_no_usage():
    d = make_json()
    d.pop("usage")
    return d


class FakeBackend(http.server.BaseHTTPRequestHandler):
    """按端口角色扮演不同故障模式的假后端。"""

    role = "good"

    def log_message(self, *a):  # 静音
        pass

    def _send(self, code, payload, chunked=False):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        if chunked:
            self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        if chunked:
            for part in payload:
                data = part.encode()
                self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data))
                self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n")
        else:
            self.wfile.write(payload)

    def do_GET(self):
        if self.path.startswith("/health"):
            if self.role == "flaky":
                # 健康与否由用例在运行时切换，用于验证摘除 / 恢复闭环
                if FLAKY_UP["up"]:
                    self._send(200, b'{"status":"ok"}')
                else:
                    self._send(500, b'{"status":"down"}')
                return
            if self.role == "unhealthy":
                body = b'{"status":"down"}'
                self.send_response(500)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)
                return
            body = b'{"status":"ok"}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)
            return
        self._send(404, b"{}")

    def do_POST(self):
        if self.role == "flaky":
            FLAKY_HITS["n"] += 1
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            body = json.loads(raw or b"{}")
        except Exception:
            body = {}
        use_gzip = "gzip" in (self.headers.get("Accept-Encoding") or "")

        def encode(obj):
            data = json.dumps(obj).encode()
            return gzip.compress(data) if use_gzip else data

        # 慢后端：睡眠后返回可重试错误，用于触发网关重试
        if self.role == "slow503":
            time.sleep(1.5)
            self.send_response(503)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":"overloaded"}')
            return

        if self.role == "unhealthy":
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":"bad gateway"}')
            return

        data = make_json() if self.role != "nousage" else make_json_no_usage()

        if not body.get("stream"):
            if use_gzip:
                self.send_header_ = None
                raw = encode(data)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Encoding", "gzip")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
                return
            self._send(200, json.dumps(data).encode())
            return

        # 流式
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        if use_gzip:
            self.send_header("Content-Encoding", "gzip")
        self.end_headers()
        parts = []
        for i in range(3):
            chunk = {"id": "fake", "object": "chat.completion.chunk", "created": 0,
                     "model": "qwen2.5:1.5b",
                     "choices": [{"index": 0, "delta": {"content": "tok%d " % i},
                                  "finish_reason": None}]}
            parts.append("data: " + json.dumps(chunk) + "\n\n")
        # 仅当请求带了 stream_options.include_usage 才吐 usage chunk
        if (body.get("stream_options") or {}).get("include_usage"):
            tail = {"id": "fake", "object": "chat.completion.chunk", "created": 0,
                    "model": "qwen2.5:1.5b", "choices": [],
                    "usage": {"prompt_tokens": 11, "completion_tokens": 7,
                              "total_tokens": 18,
                              "prompt_tokens_details": {"cached_tokens": 4}}}
            parts.append("data: " + json.dumps(tail) + "\n\n")
        parts.append("data: [DONE]\n\n")
        for p in parts:
            out = p.encode()
            self.wfile.write(out)
            self.wfile.flush()


def start_backends():
    servers = []
    for role, port in PORTS.items():
        if not free_port(port):
            raise SystemExit("端口 %d 被占用，先清理旧进程" % port)
        handler = type("H_%s" % role, (FakeBackend,), {"role": role})
        srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)
        t = threading.Thread(target=srv.serve_forever, daemon=True)
        t.start()
        servers.append(srv)
    return servers


# ============================================================================
# 网关启动
# ============================================================================

CONFIG = """
gateway:
  port: {gw}
  timeout: "120s"
  max_retries: 1
  max_body_size: "16MB"
  stream_idle_timeout: "120s"
  stream_max_duration: "0"

auth:
  enabled: true
  protect_metrics: false
  keys:
    - name: "alpha"
      key_hash: "{h_alpha}"
      rate_limit: 0
      burst: 0
    - name: "beta"
      key_hash: "{h_beta}"
      rate_limit: 0
      burst: 0
    - name: "gamma"
      key_hash: "{h_gamma}"
      rate_limit: 2
      burst: 2

default_pool: "default"

backends:
  - name: "good"
    url: "http://127.0.0.1:{p_good}"
    health_check_path: "/health"
    health_check_timeout: "2s"
    health_check_interval: "1s"
    pool: "default"
  - name: "slow-503"
    url: "http://127.0.0.1:{p_slow}"
    health_check_path: "/health"
    health_check_timeout: "2s"
    health_check_interval: "1s"
    pool: "default"
  - name: "nousage"
    url: "http://127.0.0.1:{p_no}"
    health_check_path: "/health"
    health_check_timeout: "2s"
    health_check_interval: "1s"
    pool: "nousage-pool"
  - name: "flaky"
    url: "http://127.0.0.1:{p_flaky}"
    health_check_path: "/health"
    health_check_timeout: "2s"
    health_check_interval: "1s"
    pool: "flaky-pool"

models:
  - name: "no-usage-model"
    pool: "nousage-pool"
  - name: "flaky-model"
    pool: "flaky-pool"
"""


def build_gateway(tmpdir):
    exe = os.path.join(tmpdir, "igw_regression.exe")
    r = subprocess.run(["go", "build", "-o", exe, "./cmd/gateway"],
                       cwd=REPO_ROOT, capture_output=True, text=True)
    if r.returncode != 0:
        raise SystemExit("网关编译失败:\n" + r.stderr)
    return exe


def start_gateway(exe, tmpdir):
    cfg_path = os.path.join(tmpdir, "config.regression.yaml")
    cfg = CONFIG.format(
        gw=GW_PORT,
        h_alpha=sha256hex(KEY_ALPHA), h_beta=sha256hex(KEY_BETA),
        h_gamma=sha256hex(KEY_GAMMA),
        p_good=PORTS["good"], p_slow=PORTS["slow503"], p_no=PORTS["nousage"],
        p_flaky=PORTS["flaky"],
    )
    open(cfg_path, "w", encoding="utf-8").write(cfg)
    log_path = os.path.join(tmpdir, "gateway.log")
    lf = open(log_path, "wb")
    proc = subprocess.Popen([exe, "--config", cfg_path],
                            stdout=lf, stderr=subprocess.STDOUT, cwd=REPO_ROOT)
    deadline = time.time() + 30
    while time.time() < deadline:
        if proc.poll() is not None:
            raise SystemExit("网关进程提前退出，日志见 %s" % log_path)
        try:
            with urllib.request.urlopen("http://127.0.0.1:%d/health" % GW_PORT,
                                        timeout=1) as r:
                if r.status == 200:
                    time.sleep(2.5)  # 等健康检查跑一轮
                    return proc, log_path
        except Exception:
            time.sleep(0.3)
    proc.kill()
    raise SystemExit("网关启动超时，日志见 %s" % log_path)


# ============================================================================
# HTTP 辅助
# ============================================================================

def req(method, path, body=None, headers=None, timeout=30, stream=False):
    url = "http://127.0.0.1:%d%s" % (GW_PORT, path)
    data = None
    if body is not None:
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
    r = urllib.request.Request(url, data=data, method=method)
    for k, v in (headers or {}).items():
        r.add_header(k, v)
    try:
        resp = urllib.request.urlopen(r, timeout=timeout)
        raw = resp.read() if not stream else b""
        return {"status": resp.status, "headers": dict(resp.headers),
                "body": raw, "resp": resp}
    except urllib.error.HTTPError as e:
        raw = e.read()
        return {"status": e.code, "headers": dict(e.headers or {}), "body": raw}


def get_metrics():
    r = req("GET", "/metrics")
    out = {}
    for line in r["body"].decode("utf-8", "replace").splitlines():
        if line.startswith("#") or not line.strip():
            continue
        if "{" in line:
            name = line[:line.index("{")]
            rest = line[line.index("{") + 1:line.rindex("}")]
            val = line[line.rindex("}") + 1:].strip()
            try:
                out[(name, rest)] = float(val)
            except ValueError:
                pass
        else:
            parts = line.split()
            if len(parts) == 2:
                try:
                    out[(parts[0], "")] = float(parts[1])
                except ValueError:
                    pass
    return out


def mget(metrics, name, **labels):
    """按 label 精确匹配取值，返回 0.0 表示不存在"""
    for (n, ls), v in metrics.items():
        if n != name:
            continue
        ok = True
        for k, want in labels.items():
            if '%s="%s"' % (k, want) not in ls:
                ok = False
                break
        if ok:
            return v
    return 0.0


def mdelta(m0, m1, name, contains=""):
    """某指标（可按 label 子串过滤）在两次采样间的增量。"""
    def s(m):
        return sum(v for (n, l), v in m.items() if n == name and contains in l)
    return s(m1) - s(m0)


def read_log(log_path):
    try:
        return open(log_path, "rb").read().decode("utf-8", "replace")
    except Exception:
        return ""


def read_log_from(log_path, offset):
    """只读新增部分——让用例能对自己发出的请求做精确归因。"""
    try:
        with open(log_path, "rb") as f:
            f.seek(offset)
            return f.read().decode("utf-8", "replace")
    except Exception:
        return ""


# STABLE_MODEL 走独立的单后端池（nousage-pool），用于隔离验证「机制本身」。
# default 池含 slow-503，随机选中会让 SSE/计量等用例因「选到坏节点」而失败，
# 掩盖真正要验证的东西。流式不重试的问题由 H1 单独负责暴露。
STABLE_MODEL = "no-usage-model"

AUTH_H = {"Authorization": "Bearer " + KEY_ALPHA}


# ============================================================================
# 用例
# ============================================================================

class Case:
    def __init__(self, cid, group, name, fn, known=False, note=""):
        self.cid, self.group, self.name = cid, group, name
        self.fn, self.known, self.note = fn, known, note


def _chat(extra=None, **kw):
    body = {"model": "qwen2.5:1.5b",
            "messages": [{"role": "user", "content": "hi"}]}
    body.update(extra or {})
    return req("POST", "/v1/chat/completions", body=body, **kw)


def _chat_stable(extra=None, **kw):
    """打到只含健康后端的池，排除「随机选到坏节点」的干扰。"""
    body = {"model": STABLE_MODEL,
            "messages": [{"role": "user", "content": "hi"}]}
    body.update(extra or {})
    return req("POST", "/v1/chat/completions", body=body, **kw)


CASES = []


def case(cid, group, name, known=False, note=""):
    def deco(fn):
        CASES.append(Case(cid, group, name, fn, known, note))
        return fn
    return deco


# ---- A 组：基础转发与管理端点 ----

@case("A1", "a", "非流式请求转发成功")
def c_a1(ctx):
    r = _chat(headers=AUTH_H)
    if r["status"] != 200:
        return False, "期望 200，实际 %d" % r["status"]
    j = json.loads(r["body"])
    if "hello from fake backend" not in json.dumps(j):
        return False, "响应内容未透传"
    return True, "200，响应体完整透传"


@case("A2", "a", "流式 SSE 逐块透传")
def c_a2(ctx):
    # 用单后端池：本用例验证 SSE 机制本身，流式选节点问题由 H1 覆盖
    r = _chat_stable({"stream": True}, headers=AUTH_H)
    if r["status"] != 200:
        return False, "期望 200，实际 %d" % r["status"]
    if "text/event-stream" not in r["headers"].get("Content-Type", ""):
        return False, "Content-Type 不是 text/event-stream"
    txt = r["body"].decode("utf-8", "replace")
    n = txt.count("data: ")
    if n < 4:
        return False, "data 行仅 %d 条，疑似未流式" % n
    return True, "%d 条 data 行逐块到达" % n


@case("A3", "a", "/health 免鉴权可用", note="K8s probe 需要")
def c_a3(ctx):
    r = req("GET", "/health")
    if r["status"] != 200:
        return False, "期望 200（且不带 Key），实际 %d" % r["status"]
    return True, "200 且无需鉴权"


@case("A4", "a", "/backends 返回后端清单")
def c_a4(ctx):
    r = req("GET", "/backends")
    if r["status"] != 200:
        return False, "期望 200，实际 %d" % r["status"]
    txt = r["body"].decode("utf-8", "replace")
    if "slow-503" not in txt:
        return False, "清单里看不到后端名"
    return True, "清单含全部后端"


@case("A5", "a", "/metrics 返回 Prometheus 格式")
def c_a5(ctx):
    r = req("GET", "/metrics")
    if r["status"] != 200:
        return False, "期望 200，实际 %d" % r["status"]
    txt = r["body"].decode("utf-8", "replace")
    for must in ["inference_gateway_requests_total",
                 "inference_gateway_backend_health"]:
        if must not in txt:
            return False, "缺少指标 %s" % must
    return True, "核心指标齐全"


# ---- B 组：安全边界 ----

@case("B1", "b", "超大请求体被网关拒绝（网关侧必须记账）")
def c_b1(ctx):
    # 判据只看网关侧：20MB 打进去，errors_total 必须记一笔
    # request_body_too_large。客户端「能不能读到」413 不由网关决定 ——
    # handler 返回后 Go 只 drain 有限字节就关连接，还在发送的客户端会先
    # 吃到 RST（实测 20MB×10 次约 2 次如此，但 10/10 网关侧都正确拒绝）。
    # 把传输层现象当缺陷，这条用例会变成 flaky，反而掩盖真正要守的契约。
    m0 = get_metrics()
    big = {"model": "qwen2.5:1.5b",
           "messages": [{"role": "user", "content": "x" * (20 * 1024 * 1024)}]}
    try:
        r = _chat(big, headers=AUTH_H)
        client = "HTTP %d" % r["status"]
    except Exception as e:
        client = type(e).__name__
    m1 = get_metrics()
    got = mdelta(m0, m1, "inference_gateway_errors_total",
                 "request_body_too_large")
    if got <= 0:
        return False, "网关侧未记账 request_body_too_large（客户端 %s）" % client
    return True, "网关记账 %+g，客户端 %s" % (got, client)


@case("B2", "b", "缺少 API Key → 401")
def c_b2(ctx):
    r = _chat()
    if r["status"] != 401:
        return False, "期望 401，实际 %d" % r["status"]
    if "missing_key" not in r["body"].decode("utf-8", "replace"):
        return False, "错误原因未区分 missing_key"
    return True, "401 missing_key"


@case("B3", "b", "错误 API Key → 401")
def c_b3(ctx):
    r = _chat(headers={"Authorization": "Bearer totally-wrong"})
    if r["status"] != 401:
        return False, "期望 401，实际 %d" % r["status"]
    if "invalid_key" not in r["body"].decode("utf-8", "replace"):
        return False, "错误原因未区分 invalid_key"
    return True, "401 invalid_key"


@case("B4", "b", "X-API-Key 头写法可用")
def c_b4(ctx):
    r = _chat(headers={"X-API-Key": KEY_ALPHA})
    return (True, "200") if r["status"] == 200 else (
        False, "期望 200，实际 %d" % r["status"])


@case("B5", "b", "上限内请求体正常放行")
def c_b5(ctx):
    # 边界下沿：刚好在 16MB 以内必须放行，用来卡住「上限算错/单位错」
    body = {"model": "qwen2.5:1.5b",
            "messages": [{"role": "user",
                          "content": "x" * (16 * 1024 * 1024 - 1024)}]}
    r = _chat(body, headers=AUTH_H)
    if r["status"] != 200:
        return False, "期望 200（刚好在 16MB 内），实际 %d" % r["status"]
    return True, "16MB-1KB → 200"


@case("B6", "b", "小幅超限时客户端可读到干净 413")
def c_b6(ctx):
    # 只超 64KB：剩余字节能被 drain 完，客户端必然读到完整 413（实测 10/10）。
    # 这条守「客户端体验」，B1 守「网关必须拒绝」，两者分开才都稳定。
    body = {"model": "qwen2.5:1.5b",
            "messages": [{"role": "user",
                          "content": "x" * (16 * 1024 * 1024 + 64 * 1024)}]}
    r = _chat(body, headers=AUTH_H)
    if r["status"] != 413:
        return False, "期望 413，实际 %d" % r["status"]
    if b"too large" not in r["body"]:
        return False, "413 响应体缺少 too large 说明：%r" % r["body"][:80]
    return True, "16MB+64KB → 413 且响应体可读"


# ---- C 组：限流 ----

@case("C1", "c", "令牌桶限流生效且带 Retry-After")
def c_c1(ctx):
    codes = []
    for _ in range(6):
        # 必须打「快请求」：default 池里的 slow-503 每次要 sleep 1.5s，
        # 请求间隔一旦大于令牌补充周期，令牌桶永远处于满状态，限流永远不触发。
        codes.append(_chat_stable(headers={"X-API-Key": KEY_GAMMA})["status"])
    ok = codes[:2] == [200, 200]  # burst=2
    throttled = [c for c in codes[2:] if c == 429]
    if not throttled:
        return False, "连打 6 次（rate=2 burst=2）未出现 429：%s" % codes
    r = _chat_stable(headers={"X-API-Key": KEY_GAMMA})
    ra = r["headers"].get("Retry-After", "")
    if not ra and r["status"] == 429:
        return False, "429 未带 Retry-After，客户端会盲重试"
    return True, "序列 %s，429 带 Retry-After=%s" % (codes, ra or "(本轮已放过)")


# ---- D 组：重试与轮询（F1）----

@case("D1", "d", "后端 503 时自动换节点")
def c_d1(ctx):
    r = _chat(headers=AUTH_H)
    if r["status"] != 200:
        return False, "期望 200（重试后成功），实际 %d" % r["status"]
    return True, "503 → 换节点后 200"


@case("D2", "d", "轮询不退化：首次尝试轮流落不同后端", known=True,
      note="F1：Next 与 NextExcluding 双重推进同一游标")
def c_d2(ctx):
    # 关键：必须自己发请求并只读「本用例期间新增的日志」。
    # 早先版本直接翻全量日志，把启动瞬态的前两次 good 也算进去，
    # 于是 8 个样本里只要混进 1 个不同值就判定通过 —— 那是假阳性。
    off = os.path.getsize(ctx["log"])
    for _ in range(8):
        _chat(headers=AUTH_H)
    new = read_log_from(ctx["log"], off)

    firsts = []
    for l in new.splitlines():
        if "请求 #" not in l or "attempt 1/" not in l:
            continue
        for name in ("good", "slow-503"):
            if "→ " + name in l:
                firsts.append(name)
                break

    tail = firsts[-6:]
    if len(tail) < 6:
        return False, "样本不足（需 6 次），实际 %d" % len(tail)
    if len(set(tail)) == 1:
        return False, "连续 6 次首次尝试全部命中 %s —— 轮询退化为固定节点" % tail[0]
    return True, "首次尝试分布：%s" % ", ".join(tail)


# ---- E 组：token 计量（F3）----

@case("E1", "e", "非流式 usage 计入 token 指标")
def c_e1(ctx):
    m0 = get_metrics()
    _chat(headers=AUTH_H)
    m1 = get_metrics()
    d = m1.get(("inference_gateway_prompt_tokens_total", None), 0) - \
        m0.get(("inference_gateway_prompt_tokens_total", None), 0)
    got = sum(v for (n, l), v in m1.items()
              if n == "inference_gateway_prompt_tokens_total") - \
        sum(v for (n, l), v in m0.items()
            if n == "inference_gateway_prompt_tokens_total")
    if got <= 0:
        return False, "prompt_tokens 未增长（%+g）" % got
    return True, "prompt_tokens %+g" % got


@case("E2", "e", "流式 usage 计入（网关注入 stream_options）")
def c_e2(ctx):
    m0 = get_metrics()
    # 单后端池：把「注入是否生效」与「是否恰好选到坏节点」两件事隔开
    r = _chat_stable({"stream": True}, headers=AUTH_H)
    m1 = get_metrics()
    if r["status"] != 200:
        return False, "流式请求失败：%d" % r["status"]
    got = sum(v for (n, l), v in m1.items()
              if n == "inference_gateway_prompt_tokens_total") - \
        sum(v for (n, l), v in m0.items()
            if n == "inference_gateway_prompt_tokens_total")
    if got <= 0:
        return False, "流式 prompt_tokens 未增长 —— 客户端未传 stream_options 时网关应自行注入"
    return True, "客户端未传 stream_options，网关注入后 prompt_tokens %+g" % got


@case("E3", "e", "缓存命中 token 单独计量")
def c_e3(ctx):
    m0 = get_metrics()
    _chat(headers=AUTH_H)
    m1 = get_metrics()
    got = sum(v for (n, l), v in m1.items()
              if n == "inference_gateway_cached_tokens_total") - \
        sum(v for (n, l), v in m0.items()
            if n == "inference_gateway_cached_tokens_total")
    if got <= 0:
        return False, "cached_tokens 未记录（后端上报了 4）"
    return True, "cached_tokens %+g" % got


@case("E4", "e", "gzip 响应下 token 仍被计量", known=True,
      note="F3：copyHeaders 转发 Accept-Encoding，Transport 不解压")
def c_e4(ctx):
    m0 = get_metrics()
    r = _chat(headers={**AUTH_H, "Accept-Encoding": "gzip, deflate"})
    m1 = get_metrics()
    if r["status"] != 200:
        return False, "带 gzip 请求失败：%d" % r["status"]
    got = sum(v for (n, l), v in m1.items()
              if n == "inference_gateway_prompt_tokens_total") - \
        sum(v for (n, l), v in m0.items()
            if n == "inference_gateway_prompt_tokens_total")
    miss = sum(v for (n, l), v in m1.items()
               if n == "inference_gateway_usage_missing_total") - \
        sum(v for (n, l), v in m0.items()
            if n == "inference_gateway_usage_missing_total")
    if got <= 0:
        return False, "gzip 下 prompt_tokens %+g，usage_missing %+g —— 计量静默失效" % (got, miss)
    return True, "prompt_tokens %+g" % got


@case("E5", "e", "后端无 usage 时记账而非静默")
def c_e5(ctx):
    if ctx["reload_flag"]:
        pass
    m0 = get_metrics()
    body = {"model": "no-usage-model",
            "messages": [{"role": "user", "content": "hi"}]}
    r = req("POST", "/v1/chat/completions", body=body, headers=AUTH_H)
    m1 = get_metrics()
    if r["status"] != 200:
        return False, "转发失败：%d" % r["status"]
    miss = sum(v for (n, l), v in m1.items()
               if n == "inference_gateway_usage_missing_total") - \
        sum(v for (n, l), v in m0.items()
            if n == "inference_gateway_usage_missing_total")
    if miss <= 0:
        return False, "无 usage 后端未记账 usage_missing"
    return True, "usage_missing %+g，且转发仍 200" % miss


@case("E6", "e", "按 Key 用量归因")
def c_e6(ctx):
    m0 = get_metrics()
    _chat(headers={"X-API-Key": KEY_BETA})
    m1 = get_metrics()
    got = mget(m1, "inference_gateway_key_requests_total",
               key="beta", status_code="200")
    base = mget(m0, "inference_gateway_key_requests_total",
                key="beta", status_code="200")
    if got - base <= 0:
        return False, "key=beta 的请求未被归因"
    return True, "key_requests_total{key=beta} %+g" % (got - base)


# ---- F 组：延迟指标（F2）----

@case("F1", "f", "后端延迟反映客户端真实等待", known=True,
      note="F2：start 计时写在重试循环内 + 失败路径不计延迟")
def c_f1(ctx):
    before = sum(v for (n, l), v in get_metrics().items()
                 if n == "inference_gateway_requests_total")
    t0 = time.time()
    r = _chat(headers=AUTH_H)
    wall = time.time() - t0
    if r["status"] != 200:
        return False, "请求失败：%d" % r["status"]
    # slow-503 sleep 1.5s，客户端必然等待 >1s
    if wall < 1.0:
        return False, "客户端仅等待 %.2fs，用例前提不成立" % wall
    m = get_metrics()
    lat = {l: v for (n, l), v in m.items()
           if n == "inference_gateway_backend_latency_seconds"}
    slow_lat = lat.get('backend="slow-503"')
    if slow_lat is None:
        return False, "降级中的 slow-503 在延迟指标里完全不存在（看不到它在拖后腿）"
    if slow_lat < 1.0:
        return False, "客户端等了 %.2fs，指标却记 %.4fs（低估）" % (wall, slow_lat)
    return True, "客户端 %.2fs，指标 %.2fs" % (wall, slow_lat)


# ---- G 组：指标合法性 ----

@case("G1", "g", "/metrics 输出格式合法（可被 Prometheus 解析）")
def c_g1(ctx):
    txt = req("GET", "/metrics")["body"].decode("utf-8", "replace")
    bad = []
    for line in txt.splitlines():
        if line.startswith("#") or not line.strip():
            continue
        if "{" in line:
            labels = line[line.index("{") + 1:line.rindex("}")]
            for seg in labels.split('","'):
                if seg.count('"') % 2 != 0:
                    bad.append(line)
    if bad:
        return False, "存在引号不配对的行：%s" % bad[:2]
    return True, "标签引号配对正常"


@case("G2", "g", "健康后端可被 Prometheus 发现")
def c_g2(ctx):
    m = get_metrics()
    up = sum(v for (n, l), v in m.items()
             if n == "inference_gateway_backend_health")
    if up <= 0:
        return False, "没有任何后端标记为健康"
    return True, "健康后端合计 %g" % up


# ---- H 组：流式可靠性 ----

@case("H1", "h", "流式请求遇后端 5xx 应换节点重试", known=True,
      note="F11：handleStreaming 只调一次 pool.Next()，5xx 直接回客户端")
def c_h1(ctx):
    codes = []
    for _ in range(6):
        codes.append(_chat({"stream": True}, headers=AUTH_H)["status"])
    bad = [c for c in codes if c != 200]
    if bad:
        return False, "6 次流式有 %d 次直接失败（%s）—— 池内明明有健康后端却没换节点" \
                      % (len(bad), ", ".join(str(c) for c in bad))
    return True, "6 次流式全部 200"


# ---- I 组：健康检查摘除与恢复 ----
#
# flaky 后端独占 flaky-pool，健康与否由 FLAKY_UP 在运行时切换，
# 这样「摘除 → 恢复 → 再摘除」的闭环能被完整验证。
# 早先版本里 unhealthy 后端起了服务却没写进 config，等于这段逻辑从没被测过。

def _chat_flaky():
    body = {"model": "flaky-model",
            "messages": [{"role": "user", "content": "hi"}]}
    return req("POST", "/v1/chat/completions", body=body, headers=AUTH_H)


@case("I1", "i", "不健康后端被摘除且不再接流量")
def c_i1(ctx):
    FLAKY_UP["up"] = False
    time.sleep(3)  # 等至少一个健康检查周期（interval=1s）
    m = get_metrics()
    h = mget(m, "inference_gateway_backend_health", backend="flaky")
    if h != 0:
        return False, "flaky 健康检查返 500，指标却记为 %g" % h
    # flaky-pool 只有它一个后端，被摘除后请求应无处可去。
    # 实测契约：无可用后端时网关返 503（不是 502，也不是把请求丢给别的池）。
    r = _chat_flaky()
    if r["status"] != 503:
        return False, "已摘除后应无可用后端（实测契约 503），实际 %d" % r["status"]
    return True, "health=0，请求 503（无可用后端）"


@case("I2", "i", "后端恢复健康后重新纳入")
def c_i2(ctx):
    FLAKY_UP["up"] = True
    time.sleep(3)
    m = get_metrics()
    h = mget(m, "inference_gateway_backend_health", backend="flaky")
    if h != 1:
        return False, "flaky 已恢复，指标仍为 %g —— 未被重新纳入" % h
    hits0 = FLAKY_HITS["n"]
    r = _chat_flaky()
    if r["status"] != 200:
        return False, "恢复后请求失败：%d" % r["status"]
    if FLAKY_HITS["n"] <= hits0:
        return False, "返回 200 但不是 flaky 处理的"
    return True, "health=1，请求命中 flaky 并 200"


@case("I3", "i", "再次转不健康时被重新摘除")
def c_i3(ctx):
    FLAKY_UP["up"] = False
    time.sleep(3)
    m = get_metrics()
    h = mget(m, "inference_gateway_backend_health", backend="flaky")
    if h != 0:
        return False, "flaky 再次返 500，指标仍为 %g —— 未摘除" % h
    r = _chat_flaky()
    if r["status"] != 503:
        return False, "已摘除后应无可用后端（实测契约 503），实际 %d" % r["status"]
    return True, "health=0，重新摘除生效（请求 503）"


# ============================================================================
# 执行
# ============================================================================

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--group", default="")
    ap.add_argument("--keep", action="store_true")
    args = ap.parse_args()

    print("=" * 78)
    print("inference-gateway 功能回归")
    print("仓库：%s" % REPO_ROOT)
    print("=" * 78)

    tmpdir = tempfile.mkdtemp(prefix="igw_reg_")
    print("[1/5] 启动假后端 ...")
    servers = start_backends()
    print("      good=%d slow-503=%d nousage=%d flaky=%d" % (
        PORTS["good"], PORTS["slow503"], PORTS["nousage"], PORTS["flaky"]))

    print("[2/5] 编译网关 ...")
    exe = build_gateway(tmpdir)
    print("      OK")

    print("[3/5] 启动网关 :%d ..." % GW_PORT)
    proc, log_path = start_gateway(exe, tmpdir)
    print("      OK，日志 %s" % log_path)

    ctx = {"log": log_path, "reload_flag": False}

    print("[4/5] 执行用例 ...\n")
    groups = args.group.lower().split(",") if args.group else None
    results = []
    for c in CASES:
        if groups and c.group not in groups and c.cid[:1].lower() not in groups:
            continue
        try:
            ok, detail = c.fn(ctx)
        except Exception as e:
            ok, detail = False, "执行异常：%r" % e
        if ok:
            state = "FIXED" if c.known else "PASS"
        else:
            state = "KNOWN" if c.known else "FAIL"
        results.append((state, c, detail))

    print("%-6s %-4s %-38s %s" % ("状态", "用例", "名称", "详情"))
    print("-" * 100)
    for state, c, detail in results:
        mark = {"PASS": "[OK]  ", "FAIL": "[FAIL]",
                "KNOWN": "[KNOWN]", "FIXED": "[FIXED]"}[state]
        print("%-6s %-4s %-38s %s" % (mark, c.cid, c.name, detail))
        if c.note and state in ("KNOWN", "FIXED"):
            print("%-6s      %-38s └─ %s" % ("", "", c.note))

    n_pass = sum(1 for s, _, _ in results if s in ("PASS", "FIXED"))
    n_fail = sum(1 for s, _, _ in results if s == "FAIL")
    n_known = sum(1 for s, _, _ in results if s == "KNOWN")

    print("-" * 100)
    print("通过 %d / 失败 %d / 已知缺陷 %d / 合计 %d" %
          (n_pass, n_fail, n_known, len(results)))
    if n_known:
        print("\n已知缺陷明细（修复后应转为 FIXED）：")
        for s, c, d in results:
            if s == "KNOWN":
                print("  %s  %s  %s" % (c.cid, c.name, c.note))

    print("\n[5/5] 清理 ...")
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except Exception:
        proc.kill()
    for s in servers:
        s.shutdown()
        s.server_close()
    if not args.keep:
        try:
            import shutil
            shutil.rmtree(tmpdir, ignore_errors=True)
        except Exception:
            pass
    print("完成。日志 %s" % ("已保留" if args.keep else "已清理"))

    return 1 if n_fail else 0


if __name__ == "__main__":
    sys.exit(main())
