from __future__ import annotations

import time

import pytest

pytestmark = pytest.mark.asyncio


def _make_client(service, served_name):
    from tests.e2e.client.http_client import HttpClient

    return HttpClient(service.url, served_name)


async def _measure_latency(client, n=10, **kw):
    messages = [{"role": "user", "content": "Hi"}]
    for _ in range(2):
        resp = await client.chat(messages=messages, **kw)
        assert resp.status_code == 200
    latencies = []
    for _ in range(n):
        start = time.perf_counter()
        resp = await client.chat(messages=messages, **kw)
        elapsed = (time.perf_counter() - start) * 1000.0
        assert resp.status_code == 200
        latencies.append(elapsed)
    return sum(latencies) / len(latencies)


@pytest.mark.P2
@pytest.mark.nightly
async def test_latency_overhead(vllm_service_factory, model_yaml, served_name):
    # 基线请求须带上中间件强制注入的采集参数（logprobs / top_logprobs / rtati）。
    # 检测依赖这些参数，其引擎侧计算开销（全词表 top-k softmax，Ascend 上数百 ms）
    # 属于功能固有成本，不应计入"中间件开销"。
    from anomaly_middleware.env import TOP_LOGPROBS_DEFAULT

    injected = {
        "logprobs": True,
        "top_logprobs": TOP_LOGPROBS_DEFAULT,
        "return_tokens_as_token_ids": True,
    }

    no_mw = vllm_service_factory(
        {"model": model_yaml, "middleware": False, "env": {}, "with_injector": False}
    )
    baseline = await _measure_latency(_make_client(no_mw, served_name), n=10, **injected)

    mw = vllm_service_factory(
        {"model": model_yaml, "middleware": True, "env": {}, "with_injector": True}
    )
    with_mw = await _measure_latency(_make_client(mw, served_name), n=10)

    overhead = with_mw - baseline
    assert overhead < 200.0, f"middleware overhead {overhead:.2f}ms exceeds 200ms"