"""e2e: chat 非流式 — 注入 / Content-Length / 响应恢复 / 关联头（spec §2.2 §2.3 §2.9）。"""
from __future__ import annotations

import json

import pytest

from _helpers import build_chat_response, chat_top_entry, install_fake_resolver
from conftest import drain

NI = "你"
HAO = "好"


def _chat_resp_fn(model="glm-4-7", n_top=20, nan_choice=None):
    def fn(scope, body):
        choices = []
        n = 1
        for i in range(n):
            if i == nan_choice:
                e = chat_top_entry(100, NI, -0.1, n_top=n_top, nan_at=0)
            else:
                e = chat_top_entry(200, HAO, -0.2, n_top=n_top)
            choices.append(
                {
                    "index": i,
                    "message": {"role": "assistant", "content": "x"},
                    "logprobs": {"content": [e]},
                    "finish_reason": "stop",
                }
            )
        return (
            "json",
            {"id": "c", "object": "chat.completion", "model": model, "choices": choices},
        )
    return fn


pytestmark = pytest.mark.asyncio


async def test_chat_injection_and_restore_no_logprobs(client_factory):
    client, fake, mw = client_factory(_chat_resp_fn())
    resp = await client.post(
        "/v1/chat/completions",
        json={"model": "glm-4-7", "messages": [{"role": "user", "content": "hi"}]},
    )
    assert resp.status_code == 200
    # 注入：下游收到的 body 含 logprobs=true / top_logprobs=20 / return_tokens_as_token_ids=true
    scope, body_bytes = fake.received[0]
    injected = json.loads(body_bytes)
    assert injected["logprobs"] is True
    assert injected["top_logprobs"] == 20
    assert injected["return_tokens_as_token_ids"] is True
    # Content-Length 修正
    hdrs = dict((h[0].lower(), h[1]) for h in scope["headers"])
    assert hdrs[b"content-length"] == str(len(body_bytes)).encode()
    # 恢复：客户端未请求 logprobs → null，无 token_id:
    data = resp.json()
    assert data["choices"][0]["logprobs"] is None
    assert "token_id:" not in resp.text
    # 关联头
    assert "x-anomaly-request-id" in {k.lower() for k in resp.headers.keys()}


async def test_chat_request_id_unique(client_factory):
    client, fake, mw = client_factory(_chat_resp_fn())
    r1 = await client.post("/v1/chat/completions", json={"model": "glm-4-7", "messages": []})
    r2 = await client.post("/v1/chat/completions", json={"model": "glm-4-7", "messages": []})
    id1 = r1.headers["x-anomaly-request-id"]
    id2 = r2.headers["x-anomaly-request-id"]
    assert id1 != id2
    assert len(id1) == 32  # uuid4 hex


async def test_chat_restore_truncate_decode(client_factory):
    # 客户端 logprobs=true, top_logprobs=3 → 截断 3，token 解码为文本
    client, fake, mw = client_factory(_chat_resp_fn(n_top=20))
    resp = await client.post(
        "/v1/chat/completions",
        json={
            "model": "glm-4-7",
            "messages": [],
            "logprobs": True,
            "top_logprobs": 3,
        },
    )
    entry = resp.json()["choices"][0]["logprobs"]["content"][0]
    assert len(entry["top_logprobs"]) == 3
    assert entry["token"] == HAO  # 解码文本，非 token_id:（_chat_resp_fn 用 HAO 条目）
    for tp in entry["top_logprobs"]:
        assert tp["token"] == HAO
    assert "token_id:" not in resp.text


async def test_chat_restore_keep_token_ids_when_requested(client_factory):
    # 客户端 return_tokens_as_token_ids=True → 原样保留 token_id:
    client, fake, mw = client_factory(_chat_resp_fn(n_top=20))
    resp = await client.post(
        "/v1/chat/completions",
        json={
            "model": "glm-4-7",
            "messages": [],
            "logprobs": True,
            "top_logprobs": 3,
            "return_tokens_as_token_ids": True,
        },
    )
    entry = resp.json()["choices"][0]["logprobs"]["content"][0]
    assert entry["token"].startswith("token_id:")
    assert len(entry["top_logprobs"]) == 3
    assert entry["top_logprobs"][0]["token"].startswith("token_id:")


async def test_chat_inject_max_client_vs_n(client_factory):
    # 客户端 top_logprobs=5, 环境配置 N=20 → 注入 20
    client, fake, mw = client_factory(_chat_resp_fn(n_top=20), top_logprobs=20)
    await client.post(
        "/v1/chat/completions",
        json={"model": "m", "messages": [], "logprobs": True, "top_logprobs": 5},
    )
    injected = json.loads(fake.received[0][1])
    assert injected["top_logprobs"] == 20  # max(5, 20)
    # 客户端 top_logprobs=10, N=5 → 注入 10
    client2, fake2, mw2 = client_factory(_chat_resp_fn(n_top=20), top_logprobs=5)
    await client2.post(
        "/v1/chat/completions",
        json={"model": "m", "messages": [], "logprobs": True, "top_logprobs": 10},
    )
    injected2 = json.loads(fake2.received[0][1])
    assert injected2["top_logprobs"] == 10  # max(10, 5)
    # 恢复截断到客户端 10
    entry = (await client2.post(
        "/v1/chat/completions",
        json={"model": "m", "messages": [], "logprobs": True, "top_logprobs": 10},
    )).json()["choices"][0]["logprobs"]["content"][0]
    assert len(entry["top_logprobs"]) == 10


async def test_chat_detect_truncate_n_vs_client(client_factory):
    # 客户端 logprobs=10, N=4 → 注入 10；检测截前 4；返回客户端 10
    client, fake, mw = client_factory(_chat_resp_fn(n_top=10), top_logprobs=4, real_runner=True)
    resp = await client.post(
        "/v1/chat/completions",
        json={"model": "glm-4-7", "messages": [], "logprobs": True, "top_logprobs": 10},
    )
    injected = json.loads(fake.received[0][1])
    assert injected["top_logprobs"] == 10
    entry = resp.json()["choices"][0]["logprobs"]["content"][0]
    assert len(entry["top_logprobs"]) == 10  # 客户端 10
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 1" in text


async def test_chat_top_logprobs_resolver_text_no_leak(client_factory):
    # 真实 vLLM 形态：top bytes 破损（解码为 token_id: 字符串）→ resolver 还原为文本
    def fn(scope, body):
        e = chat_top_entry(200, HAO, -0.2, n_top=5, vllm_broken_top_bytes=True)
        return (
            "json",
            {
                "id": "c",
                "object": "chat.completion",
                "model": "glm-4-7",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "x"},
                        "logprobs": {"content": [e]},
                        "finish_reason": "stop",
                    }
                ],
            },
        )

    client, fake, mw = client_factory(fn)
    install_fake_resolver(
        mw, {200: HAO, 10000: "甲", 10001: "乙", 10002: "丙", 10003: "丁", 10004: "戊"}
    )
    resp = await client.post(
        "/v1/chat/completions",
        json={
            "model": "glm-4-7",
            "messages": [],
            "logprobs": True,
            "top_logprobs": 3,
        },
    )
    entry = resp.json()["choices"][0]["logprobs"]["content"][0]
    assert entry["token"] == HAO  # 主 token resolver 优先
    assert len(entry["top_logprobs"]) == 3
    for tp in entry["top_logprobs"]:
        assert tp["token"] in ("甲", "乙", "丙", "丁", "戊")  # resolver 文本，非 token_id:
    assert "token_id:" not in resp.text


async def test_chat_top_logprobs_no_resolver_fallback_to_token_id(client_factory):
    # resolver off + 客户端请求 topk + 未设 rtati → 触发降级回退（§4.7 例外）
    # 主 token: bytes 真实文本（三层第二层 HAO）
    # top_logprobs: bytes 破损 → 三层第三层 token_id:NNN（保证 topk logprob 数据不丢失）
    def fn(scope, body):
        e = chat_top_entry(200, HAO, -0.2, n_top=5, vllm_broken_top_bytes=True)
        return (
            "json",
            {
                "id": "c",
                "object": "chat.completion",
                "model": "glm-4-7",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "x"},
                        "logprobs": {"content": [e]},
                        "finish_reason": "stop",
                    }
                ],
            },
        )

    client, fake, mw = client_factory(fn)
    resp = await client.post(
        "/v1/chat/completions",
        json={"model": "glm-4-7", "messages": [], "logprobs": True, "top_logprobs": 3},
    )
    entry = resp.json()["choices"][0]["logprobs"]["content"][0]
    assert entry["token"] == HAO  # 主 token 三层第二层（bytes 真实文本）
    assert len(entry["top_logprobs"]) == 3
    for tp in entry["top_logprobs"]:
        assert tp["token"].startswith("token_id:")  # 三层第三层（token_id 回退）
    assert "token_id:" in resp.text  # 例外允许泄漏
