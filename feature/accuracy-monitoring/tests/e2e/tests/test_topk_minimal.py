from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def topk_service(vllm_service_factory, model_yaml):
    return vllm_service_factory({
        "model": model_yaml,
        "middleware": True,
        "env": {"VLLM_ANOMALY_TOP_LOGPROBS": "1"},
        "with_injector": False,
    })


@pytest.fixture
def topk_http(topk_service, served_name):
    from tests.e2e.client.http_client import HttpClient

    return HttpClient(topk_service.url, served_name)


@pytest.fixture
def topk_metrics(topk_service):
    from tests.e2e.metrics.prometheus_client import PrometheusClient

    return PrometheusClient(f"{topk_service.url}/anomaly/metrics")


@pytest.mark.P1
@pytest.mark.full
@pytest.mark.nightly
async def test_topk_minimal(topk_http, topk_metrics):
    before = topk_metrics.get_counter("vllm_anomaly_requests_total")
    before_errors = topk_metrics.get_counter("vllm_anomaly_detection_errors_total")
    resp = await topk_http.chat(messages=[{"role": "user", "content": "Hello"}])
    assert resp.status_code == 200
    topk_metrics.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=15.0,
    )
    after = topk_metrics.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1
    after_errors = topk_metrics.get_counter("vllm_anomaly_detection_errors_total")
    assert after_errors - before_errors == 0
