from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_repeat_request(http_client, metrics_client):
    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    messages = [{"role": "user", "content": "Tell me a short fact"}]
    responses = []
    for _ in range(3):
        resp = await http_client.chat(messages=messages)
        assert resp.status_code == 200
        responses.append(resp)
    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 3,
        timeout=30.0,
    )
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 3
    bodies = [r.json() for r in responses]
    for b in bodies:
        assert "choices" in b
        assert len(b["choices"]) >= 1
