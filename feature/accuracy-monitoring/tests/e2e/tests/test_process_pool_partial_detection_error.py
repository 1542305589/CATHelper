from __future__ import annotations

import asyncio

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def pool_service(vllm_service_factory, model_yaml):
    return vllm_service_factory({
        "model": model_yaml,
        "middleware": True,
        "env": {"VLLM_ANOMALY_DETECTOR_WORKERS": "4"},
        "with_injector": True,
    })


@pytest.fixture
def pool_http(pool_service, served_name):
    from tests.e2e.client.http_client import HttpClient

    return HttpClient(pool_service.url, served_name)


@pytest.fixture
def pool_metrics(pool_service):
    from tests.e2e.metrics.prometheus_client import PrometheusClient

    return PrometheusClient(f"{pool_service.url}/anomaly/metrics")


@pytest.mark.P2
@pytest.mark.nightly
@pytest.mark.inject
async def test_process_pool_partial_detection_error(
    pool_service, injector, pool_http, pool_metrics, anomaly_data, served_name,
):
    if not injector.health_check():
        pytest.skip("injector infrastructure not available")
    injector.set_override("run_async", anomaly_data["detection_error"], count=1)
    before = pool_metrics.get_counter("vllm_anomaly_requests_total")
    before_errors = pool_metrics.get_counter("vllm_anomaly_detection_errors_total")
    before_detected = pool_metrics.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="0"}}'
    )
    messages = [{"role": "user", "content": "Hello"}]
    responses = await asyncio.gather(
        *(pool_http.chat(messages=messages) for _ in range(4))
    )
    for resp in responses:
        assert resp.status_code == 200
    pool_metrics.wait_for(
        "vllm_anomaly_detection_errors_total",
        lambda v: v - before_errors >= 1,
        timeout=30.0,
    )
    pool_metrics.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 3,
        timeout=30.0,
    )
    after = pool_metrics.get_counter("vllm_anomaly_requests_total")
    assert after - before == 3
    after_errors = pool_metrics.get_counter("vllm_anomaly_detection_errors_total")
    assert after_errors - before_errors == 1
    after_detected = pool_metrics.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="0"}}'
    )
    assert after_detected - before_detected == 0
