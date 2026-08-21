from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def custom_service(vllm_service_factory, model_yaml):
    return vllm_service_factory(
        {
            "model": model_yaml,
            "middleware": True,
            "env": {"VLLM_ANOMALY_MONITOR_RATE": "1.0"},
            "with_injector": True,
        }
    )


@pytest.fixture
def custom_http(custom_service, served_name):
    from tests.e2e.client.http_client import HttpClient

    return HttpClient(custom_service.url, served_name)


@pytest.fixture
def custom_metrics(custom_service):
    from tests.e2e.metrics.prometheus_client import PrometheusClient

    return PrometheusClient(f"{custom_service.url}/anomaly/metrics")


@pytest.mark.P1
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.inject
async def test_monitor_rate_full_inject(
    custom_http, custom_metrics, served_name, injector, anomaly_data
):
    if not injector.health_check():
        pytest.skip("injector not available")

    injector.set_override("run_async", anomaly_data["rare_character"], count=1)

    before = custom_metrics.get_counter("vllm_anomaly_requests_total")
    before_detected = custom_metrics.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}"}}'
    )

    resp = await custom_http.chat(
        messages=[{"role": "user", "content": "Hello"}], max_tokens=32
    )
    assert resp.status_code == 200

    custom_metrics.wait_for(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}"}}',
        lambda v: v - before_detected >= 1,
        timeout=10.0,
    )

    after = custom_metrics.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1

    gauge = custom_metrics.get_gauge(
        f'vllm_anomaly_last_rare_character{{model="{served_name}"}}'
    )
    assert gauge == 1
