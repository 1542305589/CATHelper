from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def custom_service(vllm_service_factory, model_yaml):
    return vllm_service_factory(
        {
            "model": model_yaml,
            "middleware": True,
            "env": {"VLLM_ANOMALY_TOKENIZER_MODEL": model_yaml["model_path"]},
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
async def test_explicit_tokenizer(custom_http, custom_metrics):
    before = custom_metrics.get_counter("vllm_anomaly_requests_total")

    resp = await custom_http.chat(
        messages=[{"role": "user", "content": "Hello"}], max_tokens=32
    )
    assert resp.status_code == 200

    custom_metrics.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=10.0,
    )
    after = custom_metrics.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1
