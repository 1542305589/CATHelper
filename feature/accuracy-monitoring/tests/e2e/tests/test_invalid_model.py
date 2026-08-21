from __future__ import annotations

import asyncio
import json

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_invalid_model(http_client, metrics_client):
    before = metrics_client.get_counter("vllm_anomaly_requests_total")

    body = json.dumps(
        {
            "model": "nonexistent-model",
            "messages": [{"role": "user", "content": "Hello"}],
            "max_tokens": 32,
        }
    )
    resp = await http_client.post_raw(
        "/v1/chat/completions",
        body.encode("utf-8"),
        {"Content-Type": "application/json"},
    )
    assert resp.status_code in (400, 404)

    await asyncio.sleep(1.0)
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 0
