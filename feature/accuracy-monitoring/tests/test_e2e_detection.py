"""e2e: 真实检测 — 正常/NaN/n>1 多候选不覆盖（spec §2.5 §2.7）。"""
from __future__ import annotations

import asyncio
import json

import pytest

from _helpers import chat_top_entry
from conftest import drain

NI = "你"
HAO = "好"
pytestmark = pytest.mark.asyncio


def _choices(entries):
    return [
        {
            "index": i,
            "message": {"role": "assistant", "content": "x"},
            "logprobs": {"content": [e]},
            "finish_reason": "stop",
        }
        for i, e in enumerate(entries)
    ]


def _resp(model, entries):
    return (
        "json",
        {"id": "c", "object": "chat.completion", "model": model, "choices": _choices(entries)},
    )


def _normal_fn():
    def fn(scope, body):
        return _resp("glm-4-7", [chat_top_entry(100, NI, -0.1, n_top=20)])
    return fn


def _nan_fn(nan_choice=0, n=1):
    def fn(scope, body):
        entries = []
        for i in range(n):
            if i == nan_choice:
                entries.append(chat_top_entry(100, NI, -0.1, n_top=20, nan_at=0))
            else:
                entries.append(chat_top_entry(200, HAO, -0.2, n_top=20))
        return _resp("glm-4-7", entries)
    return fn


async def test_detection_normal(client_factory):
    client, fake, mw = client_factory(_normal_fn(), real_runner=True)
    resp = await client.post(
        "/v1/chat/completions", json={"model": "glm-4-7", "messages": []}
    )
    assert resp.status_code == 200
    assert resp.json()["choices"][0]["logprobs"] is None  # 恢复
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 1" in text
    # 正常不计 detected
    assert 'vllm_anomaly_detected_total' in text
    assert 'ill_type="4"' not in text
    # 四 gauge 全 0
    assert 'vllm_anomaly_last_nan_value{model="glm-4-7"} 0.0' in text
    assert 'vllm_anomaly_last_rare_character{model="glm-4-7"} 0.0' in text


async def test_detection_nan_reports_ill_type_4(client_factory):
    client, fake, mw = client_factory(_nan_fn(nan_choice=0, n=1), real_runner=True)
    resp = await client.post(
        "/v1/chat/completions", json={"model": "glm-4-7", "messages": []}
    )
    assert resp.status_code == 200
    # 客户端未请求 logprobs → NaN 不泄漏给客户端
    assert "token_id:" not in resp.text
    assert resp.json()["choices"][0]["logprobs"] is None
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 1" in text
    assert 'ill_type="4"' in text
    assert 'choice_index="0"' in text
    assert 'vllm_anomaly_last_nan_value{model="glm-4-7"} 1.0' in text


async def test_detection_n3_anomaly_not_overwritten(client_factory):
    # n=3：choice1 NaN 异常；多候选异常分别上报，不覆盖
    client, fake, mw = client_factory(_nan_fn(nan_choice=1, n=3), real_runner=True)
    resp = await client.post(
        "/v1/chat/completions",
        json={"model": "glm-4-7", "messages": [], "n": 3},
    )
    assert resp.status_code == 200
    assert resp.json()["choices"][0]["logprobs"] is None
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    # 仅 choice1 异常 → choice_index="1"
    assert 'ill_type="4"' in text
    assert 'choice_index="1"' in text
    # choice_index="0" 不应被记为 ill_type=4（choice0 正常）
    # （choice_index="0" 的 ill_type=4 计数应为 0）
    assert 'vllm_anomaly_last_nan_value{model="glm-4-7"} 1.0' in text
    # 按请求计数 +1（不是 +3）
    assert "vllm_anomaly_requests_total 1" in text


async def test_detection_empty_response_skipped(client_factory):
    # 空 logprobs（无 token）→ 不调度检测
    def fn(scope, body):
        return (
            "json",
            {
                "id": "c",
                "object": "chat.completion",
                "model": "glm-4-7",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": ""},
                        "logprobs": {"content": []},
                        "finish_reason": "stop",
                    }
                ],
            },
        )
    client, fake, mw = client_factory(fn)
    await client.post("/v1/chat/completions", json={"model": "glm-4-7", "messages": []})
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 0" in text  # 空响应不检测


async def test_concurrent_requests_all_detected(client_factory):
    """并发 5 请求（spec §2.7：检测串行、任务不丢）-> 全部计数、零错误。"""
    client, fake, mw = client_factory(_normal_fn(), real_runner=True)
    await asyncio.gather(*[
        client.post("/v1/chat/completions",
                    json={"model": "glm-4-7", "messages": [], "n": 2})
        for _ in range(5)
    ])
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 5" in text  # 5 请求全部检测
    assert "vllm_anomaly_detection_errors_total 0" in text
    assert mw._pending_tasks == set()
