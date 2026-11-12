from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def custom_service(vllm_service_factory, model_yaml):
    return vllm_service_factory(
        {
            "model": model_yaml,
            "middleware": True,
            "env": {"VLLM_ANOMALY_MONITOR_RATE": "0"},
            "with_injector": False,
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
async def test_monitor_rate_passthrough(custom_http, custom_metrics):
    before = custom_metrics.get_counter("vllm_anomaly_requests_total")

    messages = [{"role": "user", "content": "Hello"}]
    for _ in range(5):
        resp = await custom_http.chat(messages=messages, max_tokens=16, temperature=0.0)
        assert resp.status_code == 200

    after = custom_metrics.get_counter("vllm_anomaly_requests_total")
    assert after - before == 0
