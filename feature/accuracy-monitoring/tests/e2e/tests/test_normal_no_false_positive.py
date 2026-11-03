from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P0
@pytest.mark.lightweight
@pytest.mark.full
@pytest.mark.nightly
async def test_normal_no_false_positive(
    vllm_service_b, http_client, metrics_client, served_name
):
    ill_types = ["1", "2", "3", "4"]
    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    before_detected = {
        it: metrics_client.get_counter(
            f'vllm_anomaly_detected_total{{ill_type="{it}",model="{served_name}"}}'
        )
        for it in ill_types
    }

    messages = [{"role": "user", "content": "Hello, how are you?"}]
    for _ in range(5):
        resp = await http_client.chat(messages=messages, max_tokens=32, temperature=0.0)
        assert resp.status_code == 200

    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 5,
        timeout=10.0,
    )

    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 5

    for it in ill_types:
        after_detected = metrics_client.get_counter(
            f'vllm_anomaly_detected_total{{ill_type="{it}",model="{served_name}"}}'
        )
        assert after_detected - before_detected[it] == 0

    gauge_names = [
        "vllm_anomaly_last_rare_character",
        "vllm_anomaly_last_garbled",
        "vllm_anomaly_last_repetition",
        "vllm_anomaly_last_nan_value",
    ]
    for name in gauge_names:
        value = metrics_client.get_gauge(f'{name}{{model="{served_name}"}}')
        assert value == 0
