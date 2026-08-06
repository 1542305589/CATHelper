"""e2e: 流式 — 增量转发 / 跨块重组 / [DONE] 保留 / 恢复（spec §2.4）。"""
from __future__ import annotations

import json

import pytest

from _helpers import (
    chat_stream_chunk,
    chat_top_entry,
    completions_stream_chunk,
)
from conftest import drain

NI = "你"
HAO = "好"

pytestmark = pytest.mark.asyncio


def _chat_stream_fn(model="glm-4-7", n_top=20, split=None):
    def fn(scope, body):
        e1 = chat_top_entry(100, NI, -0.1, n_top=n_top)
        e2 = chat_top_entry(200, HAO, -0.2, n_top=n_top)
        chunks = [
            chat_stream_chunk(model, e1, delta_text=NI),
            chat_stream_chunk(model, e2, delta_text=HAO),
            None,  # [DONE]
        ]
        return ("stream", chunks, split) if split is not None else ("stream", chunks)
    return fn


def _comp_stream_fn(model="glm-4-7", n_top=20):
    def fn(scope, body):
        chunks = [
            completions_stream_chunk(model, 100, -0.1, n_top=n_top),
            completions_stream_chunk(model, 200, -0.2, n_top=n_top),
            None,
        ]
        return ("stream", chunks)
    return fn


async def _collect_stream(client, url, body):
    content = b""
    async with client.stream("POST", url, json=body) as r:
        assert r.status_code == 200
        assert "text/event-stream" in r.headers["content-type"]
        assert "x-anomaly-request-id" in r.headers
        async for chunk in r.aiter_bytes():
            content += chunk
    return content


async def test_chat_stream_incremental_and_done(client_factory):
    client, fake, mw = client_factory(_chat_stream_fn())
    content = await _collect_stream(
        client,
        "/v1/chat/completions",
        {"model": "glm-4-7", "messages": [], "stream": True},
    )
    # 终端 [DONE] 保留
    assert b"data: [DONE]" in content
    # 客户端未请求 logprobs → 各块 logprobs=null，无 token_id:
    assert b'"logprobs": null' in content
    assert b"token_id:" not in content
    # 多块（至少 2 条 data + DONE）
    assert content.count(b"data: ") >= 3


async def test_chat_stream_cross_chunk_reassembly(client_factory):
    # 第一条事件被拆到两个 body 块 → 重组后客户端收到一条完整事件
    client, fake, mw = client_factory(_chat_stream_fn(split=0))
    content = await _collect_stream(
        client,
        "/v1/chat/completions",
        {"model": "glm-4-7", "messages": [], "logprobs": True, "top_logprobs": 3, "stream": True},
    )
    # 第一条事件完整（data: {...}\n\n），未被拆断
    first = content[content.find(b"data: "):]
    first_event = first[: first.find(b"\n\n") + 2]
    parsed = json.loads(first_event[len(b"data: "):].split(b"\n")[0])
    assert "choices" in parsed
    # 截断到 3
    entry = parsed["choices"][0]["logprobs"]["content"][0]
    assert len(entry["top_logprobs"]) == 3
    assert entry["token"] == NI  # 解码文本
    assert b"data: [DONE]" in content


async def test_chat_stream_detection_after_done(client_factory):
    client, fake, mw = client_factory(_chat_stream_fn())
    await _collect_stream(
        client,
        "/v1/chat/completions",
        {"model": "glm-4-7", "messages": [], "stream": True},
    )
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 1" in text


async def test_completions_stream_restore_no_token_id(client_factory):
    client, fake, mw = client_factory(_comp_stream_fn())
    content = await _collect_stream(
        client,
        "/v1/completions",
        {"model": "glm-4-7", "prompt": "x", "stream": True},
    )
    assert b"data: [DONE]" in content
    assert b"token_id:" not in content
    # 客户端未请求 logprobs → logprobs=null
    assert b'"logprobs": null' in content


async def test_chat_stream_detection_full_topk_not_client_m(client_factory):
    # 回归：客户端 top_logprobs=2、检测 N=20。NaN(index 5, 超出客户端 2) 应被检出 →
    # 证明送入检测的 topk 未被客户端 M 截断。
    def fn(scope, body):
        e = chat_top_entry(100, NI, -0.1, n_top=20, nan_at=5)
        return ("stream", [chat_stream_chunk("glm-4-7", e, delta_text=NI), None])
    client, fake, mw = client_factory(fn)
    await _collect_stream(
        client,
        "/v1/chat/completions",
        {
            "model": "glm-4-7",
            "messages": [],
            "stream": True,
            "logprobs": True,
            "top_logprobs": 2,
        },
    )
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert 'ill_type="4"' in text
    assert 'vllm_anomaly_last_nan_value{model="glm-4-7"} 1.0' in text


async def test_completions_stream_n3_choice_index_preserved(client_factory):
    # 回归：n=3 流式，choice2(NaN) 独立成组 → 异常应记在 choice_index="2"
    def fn(scope, body):
        chunks = [
            completions_stream_chunk("glm-4-7", 101, -0.1, index=0),
            completions_stream_chunk("glm-4-7", 201, -0.2, index=1),
            completions_stream_chunk("glm-4-7", 301, float("nan"), index=2),
            None,
        ]
        return ("stream", chunks)
    client, fake, mw = client_factory(fn)
    await _collect_stream(
        client,
        "/v1/completions",
        {"model": "glm-4-7", "prompt": "x", "stream": True, "n": 3, "logprobs": 20},
    )
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert 'ill_type="4"' in text
    assert 'choice_index="2"' in text


async def test_stream_no_buffering_done_present(client_factory):
    # 流式不先全缓冲：客户端随处理增量收到块 + [DONE]
    client, fake, mw = client_factory(_chat_stream_fn())
    content = await _collect_stream(
        client,
        "/v1/chat/completions",
        {"model": "glm-4-7", "messages": [], "stream": True},
    )
    # 至少包含两条恢复块 + DONE（中间件未先全缓冲再发）
    assert content.count(b"data: ") >= 3
    assert content.rstrip().endswith(b"data: [DONE]")
