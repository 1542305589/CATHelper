from __future__ import annotations

import asyncio

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_empty_response_max0(http_client, metrics_client):
    before = metrics_client.get_counter("vllm_anomaly_requests_total")

    resp = await http_client.chat(
        messages=[{"role": "user", "content": "Hello"}], max_tokens=0
    )
    # 中间件不干预：透传注入后的请求，接受 vLLM 原生结果。
    # vLLM 0.18 对 max_tokens=0 原生返回 400（不改变 vLLM 状态）。
    assert resp.status_code == 400

    await asyncio.sleep(1.0)
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 0
