from __future__ import annotations

import asyncio

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def concurrent_service(vllm_service_factory, model_yaml):
    return vllm_service_factory({
        "model": model_yaml,
        "middleware": True,
        "env": {"VLLM_ANOMALY_DETECTOR_WORKERS": "4"},
        "with_injector": False,
    })


@pytest.fixture
def concurrent_http(concurrent_service, served_name):
    from tests.e2e.client.http_client import HttpClient

    return HttpClient(concurrent_service.url, served_name)


@pytest.fixture
def concurrent_metrics(concurrent_service):
    from tests.e2e.metrics.prometheus_client import PrometheusClient

    return PrometheusClient(f"{concurrent_service.url}/anomaly/metrics")


@pytest.mark.P2
@pytest.mark.nightly
async def test_concurrent_10_parallel(concurrent_http, concurrent_metrics):
    before = concurrent_metrics.get_counter("vllm_anomaly_requests_total")
    messages = [{"role": "user", "content": "Hello"}]
    responses = await asyncio.gather(
        *(concurrent_http.chat(messages=messages) for _ in range(10))
    )
    for resp in responses:
        assert resp.status_code == 200
    concurrent_metrics.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 10,
        timeout=60.0,
    )
    after = concurrent_metrics.get_counter("vllm_anomaly_requests_total")
    assert after - before == 10
