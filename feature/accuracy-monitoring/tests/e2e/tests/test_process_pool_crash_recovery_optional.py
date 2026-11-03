from __future__ import annotations

import asyncio

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def crash_service(vllm_service_factory, model_yaml):
    return vllm_service_factory({
        "model": model_yaml,
        "middleware": True,
        "env": {"VLLM_ANOMALY_DETECTOR_WORKERS": "4"},
        "with_injector": False,
    })


@pytest.fixture
def crash_http(crash_service, served_name):
    from tests.e2e.client.http_client import HttpClient

    return HttpClient(crash_service.url, served_name)


@pytest.fixture
def crash_metrics(crash_service):
    from tests.e2e.metrics.prometheus_client import PrometheusClient

    return PrometheusClient(f"{crash_service.url}/anomaly/metrics")


@pytest.mark.P2
@pytest.mark.nightly
@pytest.mark.xfail(reason="inherently flaky process-kill test", strict=False)
async def test_process_pool_crash_recovery_optional(
    crash_service, crash_http, crash_metrics,
):
    psutil = pytest.importorskip("psutil")
    before_errors = crash_metrics.get_counter("vllm_anomaly_detection_errors_total")
    messages = [{"role": "user", "content": "Hello"}]
    task = asyncio.gather(
        *(crash_http.chat(messages=messages) for _ in range(4))
    )
    await asyncio.sleep(0.5)
    try:
        parent = psutil.Process(crash_service.pid)
        children = parent.children(recursive=True)
        candidates = [c for c in children if c.pid != crash_service.pid]
        if candidates:
            candidates[-1].kill()
    except Exception:
        pass
    responses = await task
    for resp in responses:
        assert resp.status_code == 200
    crash_metrics.wait_for(
        "vllm_anomaly_detection_errors_total",
        lambda v: v - before_errors >= 1,
        timeout=30.0,
    )
    after_errors = crash_metrics.get_counter("vllm_anomaly_detection_errors_total")
    assert after_errors - before_errors >= 1
    resp = await crash_http.chat(messages=messages)
    assert resp.status_code == 200
