from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P0
@pytest.mark.lightweight
@pytest.mark.full
@pytest.mark.nightly
async def test_metrics_endpoint_check(
    vllm_service_b, http_client, metrics_client, served_name
):
    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    resp = await http_client.chat(
        messages=[{"role": "user", "content": "Hello"}], max_tokens=10
    )
    assert resp.status_code == 200

    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=10.0,
    )

    metrics_resp = await http_client.get("/anomaly/metrics")
    assert metrics_resp.status_code == 200
    assert "text/plain" in metrics_resp.headers.get("content-type", "")

    body = metrics_resp.text
    expected_metrics = [
        "vllm_anomaly_requests_total",
        "vllm_anomaly_detected_total",
        "vllm_anomaly_detection_errors_total",
        "vllm_anomaly_detection_duration_seconds",
    ]
    for name in expected_metrics:
        assert name in body, f"metric {name} not found in metrics endpoint response"
