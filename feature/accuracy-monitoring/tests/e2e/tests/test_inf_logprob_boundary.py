from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P1
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.inject
async def test_inf_logprob_boundary(
    vllm_service_b, injector, http_client, metrics_client, anomaly_data,
):
    if not injector.health_check():
        pytest.skip("injector infrastructure not available")
    injector.set_override("run_async", anomaly_data["inf_logprob"], count=1)
    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    before_errors = metrics_client.get_counter("vllm_anomaly_detection_errors_total")
    resp = await http_client.chat(messages=[{"role": "user", "content": "Hello"}])
    assert resp.status_code == 200
    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=15.0,
    )
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1
    after_errors = metrics_client.get_counter("vllm_anomaly_detection_errors_total")
    assert after_errors - before_errors == 0
