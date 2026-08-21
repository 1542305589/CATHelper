from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_metrics_no_leak(http_client, metrics_client):
    vllm_metrics = await http_client.get("/metrics")
    assert vllm_metrics.status_code == 200
    assert "vllm_anomaly_" not in vllm_metrics.text
    snapshot = metrics_client.snapshot()
    assert "vllm_anomaly_" in snapshot
