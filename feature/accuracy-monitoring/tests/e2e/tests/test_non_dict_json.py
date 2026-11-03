from __future__ import annotations

import asyncio
import json

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_non_dict_json(http_client, metrics_client):
    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    body = json.dumps([1, 2, 3])
    resp = await http_client.post_raw(
        "/v1/chat/completions",
        body.encode("utf-8"),
        {"Content-Type": "application/json"},
    )
    # 中间件不拦截：JSON 数组也走正常注入后透传，接受 vLLM 原生结果（Bug #3 原则）。
    # vLLM 0.18 对注入后仍缺 messages 的请求原生返回 400。
    assert resp.status_code == 400

    await asyncio.sleep(1.0)
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 0
