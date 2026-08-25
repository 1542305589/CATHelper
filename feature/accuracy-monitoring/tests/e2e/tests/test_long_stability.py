from __future__ import annotations

import pytest

from tests.e2e.utils.memory import get_rss_mb

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.nightly
async def test_long_stability(vllm_service_b, http_client, metrics_client):
    total = 2000
    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    initial_rss = get_rss_mb(vllm_service_b.pid)
    max_rss = initial_rss
    for i in range(total):
        if i % 2 == 0:
            resp = await http_client.chat(
                messages=[{"role": "user", "content": f"Request {i}"}],
            )
        else:
            resp = await http_client.completions(
                prompt=f"Prompt {i}", max_tokens=16,
            )
        assert resp.status_code == 200
        if i > 0 and i % 100 == 0:
            rss = get_rss_mb(vllm_service_b.pid)
            if rss > max_rss:
                max_rss = rss
    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= total,
        timeout=600.0,
    )
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == total
    assert max_rss <= initial_rss * 2, (
        f"RSS grew from {initial_rss}MB to {max_rss}MB (>2x, possible leak)"
    )
