from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def custom_service(vllm_service_factory, model_yaml):
    return vllm_service_factory(
        {
            "model": model_yaml,
            "middleware": True,
            "env": {"VLLM_ANOMALY_TOP_LOGPROBS": "5"},
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
async def test_top_logprobs_max_rule(custom_http, custom_metrics):
    before = custom_metrics.get_counter("vllm_anomaly_requests_total")

    resp = await custom_http.chat(
        messages=[{"role": "user", "content": "Hello"}],
        max_tokens=10,
        logprobs=True,
        top_logprobs=10,
    )
    assert resp.status_code == 200

    data = resp.json()
    for choice in data.get("choices", []):
        logprobs_data = choice.get("logprobs")
        if not logprobs_data:
            continue
        content = logprobs_data.get("content", [])
        for token_lp in content:
            if token_lp is None:
                continue
            top_lp = token_lp.get("top_logprobs")
            if top_lp is not None:
                # 客户端请求 top_logprobs=10 → 返回给客户端的为 top10（不按 N=5 收敛）
                assert len(top_lp) == 10

    custom_metrics.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=10.0,
    )
    after = custom_metrics.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1
