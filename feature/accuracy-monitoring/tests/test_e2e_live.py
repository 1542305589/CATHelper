"""真实 vLLM 服务器 E2E（Qwen3-0.6B，--middleware 部署）。

对齐 spec §2.1/§2.3/§2.4/§2.9/§2.10：
- 服务可部署：`vllm serve <model> --middleware anomaly_middleware.AnomalyMiddleware`
- 拦截 /v1/chat/completions 与 /v1/completions，流式/非流式全覆盖
- 客户端透明：不请求 logprobs -> logprobs=null；请求 topk -> 截断 + 文本还原，无 token_id: 泄漏
- return_tokens_as_token_ids=True -> 原样保留 token_id:
- x-anomaly-request-id 关联头唯一
- /anomaly/metrics 独立端点 + 检测结果计数、零检测错误

服务地址默认 localhost:8008（run_server.sh）；服务不可达时整模块 skip（不影响离线单测）。
"""
from __future__ import annotations

import json
import os

import httpx
import pytest

BASE_URL = os.environ.get("VLLM_ANOMALY_TEST_BASE", "http://127.0.0.1:8008")
MODEL = "Qwen3-0.6B"

# 服务不可达 -> 整模块 skip
try:
    _probe = httpx.get(f"{BASE_URL}/v1/models", timeout=5.0)
    _reachable = _probe.status_code == 200
except Exception:
    _reachable = False

pytestmark = pytest.mark.skipif(
    not _reachable,
    reason=f"vLLM 服务不可达: {BASE_URL}（请先运行 run_server.sh）",
)


@pytest.fixture
def client():
    with httpx.Client(base_url=BASE_URL, timeout=120.0) as c:
        yield c


def _metrics_text(client):
    r = client.get("/anomaly/metrics")
    assert r.status_code == 200
    assert "text/plain" in r.headers.get("content-type", "")
    return r.text


# --------------------------------------------------------------------------- #
# §2.14 / §3 部署与存活
# --------------------------------------------------------------------------- #
def test_server_serves_expected_model(client):
    data = client.get("/v1/models").json()
    ids = [m["id"] for m in data["data"]]
    assert MODEL in ids


def test_metrics_endpoint_live(client):
    text = _metrics_text(client)
    assert "vllm_anomaly_requests_total" in text
    assert "vllm_anomaly_detected_total" in text


# --------------------------------------------------------------------------- #
# §2.9 关联标识 + §2.3 客户端透明（chat 非流式）
# --------------------------------------------------------------------------- #
def test_live_chat_no_logprobs_transparent(client):
    """客户端未请求 logprobs -> 响应 logprobs=null、无 token_id:、关联头存在。"""
    resp = client.post(
        "/v1/chat/completions",
        json={"model": MODEL, "messages": [{"role": "user", "content": "你好"}],
              "max_tokens": 16},
    )
    assert resp.status_code == 200
    assert "x-anomaly-request-id" in resp.headers
    choice = resp.json()["choices"][0]
    assert choice.get("logprobs") is None
    assert "token_id:" not in resp.text


def test_live_chat_logprobs_truncate_and_no_leak(client):
    """客户端 logprobs=true, top_logprobs=3 -> 截断 3、token 为解码文本、无 token_id:。"""
    resp = client.post(
        "/v1/chat/completions",
        json={"model": MODEL, "messages": [{"role": "user", "content": "你好"}],
              "max_tokens": 16, "logprobs": True, "top_logprobs": 3},
    )
    assert resp.status_code == 200
    lp = resp.json()["choices"][0].get("logprobs")
    assert lp is not None
    content = lp.get("content") or []
    assert len(content) >= 1
    for entry in content:
        tps = entry.get("top_logprobs") or []
        assert len(tps) <= 3  # 截断到客户端 M=3
        for tp in tps:
            tok = tp.get("token") or ""
            assert not tok.startswith("token_id:")  # 全文无泄漏
    assert "token_id:" not in resp.text


def test_live_chat_return_tokens_as_token_ids_kept(client):
    """客户端设 return_tokens_as_token_ids=True -> 原样保留 token_id:。"""
    resp = client.post(
        "/v1/chat/completions",
        json={"model": MODEL, "messages": [{"role": "user", "content": "你好"}],
              "max_tokens": 16, "logprobs": True, "top_logprobs": 2,
              "return_tokens_as_token_ids": True},
    )
    assert resp.status_code == 200
    lp = resp.json()["choices"][0].get("logprobs")
    assert lp is not None
    content = lp.get("content") or []
    assert len(content) >= 1
    for entry in content:
        assert str(entry.get("token", "")).startswith("token_id:")
    assert "token_id:" in resp.text  # 客户端明确要求，保留


def test_live_chat_request_ids_unique(client):
    ids = set()
    for _ in range(3):
        r = client.post("/v1/chat/completions",
                        json={"model": MODEL, "messages": [{"role": "user", "content": "hi"}],
                              "max_tokens": 4})
        ids.add(r.headers.get("x-anomaly-request-id"))
    assert len(ids) == 3


# --------------------------------------------------------------------------- #
# §2.4 流式
# --------------------------------------------------------------------------- #
def test_live_chat_stream_incremental_and_no_leak(client):
    """流式：增量收到事件 + 终端 [DONE]，无 token_id: 泄漏。"""
    payload = {"model": MODEL,
               "messages": [{"role": "user", "content": "你好"}],
               "max_tokens": 16, "stream": True}
    content = b""
    with client.stream("POST", "/v1/chat/completions", json=payload) as r:
        assert r.status_code == 200
        assert "text/event-stream" in r.headers.get("content-type", "")
        assert "x-anomaly-request-id" in r.headers
        for chunk in r.iter_bytes():
            content += chunk
    assert b"data: [DONE]" in content
    assert b"token_id:" not in content


def test_live_chat_stream_logprobs_truncated(client):
    """流式 + top_logprobs=3 -> 每块 logprobs 截断到 3、文本还原。"""
    payload = {"model": MODEL,
               "messages": [{"role": "user", "content": "你好"}],
               "max_tokens": 16, "stream": True,
               "logprobs": True, "top_logprobs": 3}
    content = b""
    with client.stream("POST", "/v1/chat/completions", json=payload) as r:
        for chunk in r.iter_bytes():
            content += chunk
    assert b"data: [DONE]" in content
    assert b"token_id:" not in content
    # 至少一个恢复块含 logprobs（截断到 3 或未生成）
    for line in content.split(b"\n"):
        if not line.startswith(b"data: ") or line.strip() == b"data: [DONE]":
            continue
        obj = json.loads(line[len(b"data: "):])
        for choice in obj.get("choices", []):
            lp = choice.get("logprobs")
            if lp and lp.get("content"):
                for entry in lp["content"]:
                    assert len(entry.get("top_logprobs") or []) <= 3


# --------------------------------------------------------------------------- #
# §2.3 completions
# --------------------------------------------------------------------------- #
def test_live_completions_no_logprobs_transparent(client):
    resp = client.post(
        "/v1/completions",
        json={"model": MODEL, "prompt": "1+1=", "max_tokens": 8},
    )
    assert resp.status_code == 200
    assert resp.json()["choices"][0].get("logprobs") is None
    assert "token_id:" not in resp.text


def test_live_completions_logprobs_text_no_leak(client):
    """completions + logprobs=3 -> tokens/top_logprobs 还原为文本，无 token_id:。"""
    resp = client.post(
        "/v1/completions",
        json={"model": MODEL, "prompt": "1+1=", "max_tokens": 8, "logprobs": 3},
    )
    assert resp.status_code == 200
    lp = resp.json()["choices"][0].get("logprobs")
    assert lp is not None
    for tok in lp.get("tokens") or []:
        assert not (isinstance(tok, str) and tok.startswith("token_id:"))
    for pos in lp.get("top_logprobs") or []:
        assert isinstance(pos, dict)
        assert len(pos) <= 3
        for k in pos:
            assert not k.startswith("token_id:")
    assert "token_id:" not in resp.text


def test_live_completions_stream_no_leak(client):
    payload = {"model": MODEL, "prompt": "1+1=", "max_tokens": 8,
               "stream": True}
    content = b""
    with client.stream("POST", "/v1/completions", json=payload) as r:
        assert "x-anomaly-request-id" in r.headers
        for chunk in r.iter_bytes():
            content += chunk
    assert b"data: [DONE]" in content
    assert b"token_id:" not in content


# --------------------------------------------------------------------------- #
# §2.5/§2.6/§2.10 检测执行与零错误
# --------------------------------------------------------------------------- #
def test_live_detection_runs_without_errors(client):
    """多请求（chat/completions、流式/非流式）后：检测执行计数增长、检测错误为 0。"""
    before = client.get("/anomaly/metrics").text
    for path, body in [
        ("/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "user", "content": "hi"}],
          "max_tokens": 4}),
        ("/v1/completions",
         {"model": MODEL, "prompt": "hi", "max_tokens": 4}),
        ("/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "user", "content": "hi"}],
          "max_tokens": 4, "stream": True}),
    ]:
        resp = client.post(path, json=body)
        assert resp.status_code == 200

    after = client.get("/anomaly/metrics").text

    def _counter(text, name):
        for line in text.splitlines():
            if line.startswith(f"{name} "):
                return float(line.split()[-1])
        return 0.0

    req_before = _counter(before, "vllm_anomaly_requests_total")
    req_after = _counter(after, "vllm_anomaly_requests_total")
    assert req_after >= req_before + 3  # 3 个新请求都被检测
    assert _counter(after, "vllm_anomaly_detection_errors_total") == 0.0
