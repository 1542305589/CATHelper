from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P1
@pytest.mark.full
@pytest.mark.nightly
async def test_single_token_boundary(http_client, metrics_client):
    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    resp = await http_client.completions(prompt="Hello", max_tokens=1)
    assert resp.status_code == 200
    body = resp.json()
    assert body["usage"]["completion_tokens"] == 1
    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=15.0,
    )
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1
