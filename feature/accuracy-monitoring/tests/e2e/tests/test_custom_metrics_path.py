from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def custom_service(vllm_service_factory, model_yaml):
    return vllm_service_factory(
        {
            "model": model_yaml,
            "middleware": True,
            "env": {"VLLM_ANOMALY_METRICS_PATH": "/custom/metrics"},
            "with_injector": False,
        }
    )


@pytest.fixture
def custom_http(custom_service, served_name):
    from tests.e2e.client.http_client import HttpClient

    return HttpClient(custom_service.url, served_name)


@pytest.mark.P1
@pytest.mark.full
@pytest.mark.nightly
async def test_custom_metrics_path(custom_http):
    resp = await custom_http.get("/custom/metrics")
    assert resp.status_code == 200
    assert "text/plain" in resp.headers.get("content-type", "")
    assert "vllm_anomaly_requests_total" in resp.text

    resp_default = await custom_http.get("/anomaly/metrics")
    assert resp_default.status_code == 404
