from __future__ import annotations

import asyncio

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P1
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.inject
async def test_empty_data_boundary(
    vllm_service_b, injector, http_client, metrics_client,
):
    if not injector.health_check():
        pytest.skip("injector infrastructure not available")

    # 注入空抽取：extract_chat_response 返回空数组（shape=(0,20)），模拟空响应
    injector.set_override("extract_empty", {}, count=1)

    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    before_errors = metrics_client.get_counter("vllm_anomaly_detection_errors_total")

    resp = await http_client.chat(
        messages=[{"role": "user", "content": "你好"}],
        max_tokens=50,
    )
    assert resp.status_code == 200

    await asyncio.sleep(3.0)
    # 空响应不检测：requests_total 不计数、errors 不计数
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 0
    after_errors = metrics_client.get_counter("vllm_anomaly_detection_errors_total")
    assert after_errors - before_errors == 0