from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P0
@pytest.mark.lightweight
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.inject
async def test_inject_rare_character(
    vllm_service_b, http_client, metrics_client, served_name, injector, anomaly_data
):
    if not injector.health_check():
        pytest.skip("injector not available")

    injector.set_override("run_async", anomaly_data["rare_character"], count=1)

    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    before_detected = metrics_client.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}"}}'
    )

    resp = await http_client.chat(
        messages=[{"role": "user", "content": "Hello"}], max_tokens=32
    )
    assert resp.status_code == 200

    metrics_client.wait_for(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}"}}',
        lambda v: v - before_detected >= 1,
        timeout=10.0,
    )

    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1

    gauge = metrics_client.get_gauge(
        f'vllm_anomaly_last_rare_character{{model="{served_name}"}}'
    )
    assert gauge == 1
