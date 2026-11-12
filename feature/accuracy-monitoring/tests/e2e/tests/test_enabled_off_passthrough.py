from __future__ import annotations

import asyncio

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def custom_service(vllm_service_factory, model_yaml):
    return vllm_service_factory(
        {
            "model": model_yaml,
            "middleware": True,
            "env": {"VLLM_ANOMALY_ENABLED": "0"},
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
async def test_enabled_off_passthrough(custom_http, custom_metrics):
    before = custom_metrics.get_counter("vllm_anomaly_requests_total")

    resp = await custom_http.chat(
        messages=[{"role": "user", "content": "Hello"}], max_tokens=32, temperature=0.0
    )
    assert resp.status_code == 200
    data = resp.json()
    assert data["choices"][0]["message"]["content"]

    await asyncio.sleep(1.0)
    after = custom_metrics.get_counter("vllm_anomaly_requests_total")
    assert after - before == 0

    metrics_resp = await custom_http.get("/anomaly/metrics")
    assert metrics_resp.status_code == 200
    assert "text/plain" in metrics_resp.headers.get("content-type", "")
