from __future__ import annotations

import asyncio

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_stream_no_done(http_client, metrics_client):
    """流式无 [DONE] 即断：客户端在收到部分块后中断，服务不崩溃，后续正常。"""
    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    before_errors = metrics_client.get_counter("vllm_anomaly_detection_errors_total")

    received = 0
    try:
        async for _line in http_client.chat_stream(
            messages=[{"role": "user", "content": "Write a long story about the sea"}],
            stream=True,
        ):
            received += 1
            if received >= 1:
                break  # 模拟在 [DONE] 前断开连接
    except Exception:
        pass

    await asyncio.sleep(2.0)
    # 服务不崩溃、无未捕获异常；后续请求正常处理
    resp = await http_client.chat(messages=[{"role": "user", "content": "Hello"}])
    assert resp.status_code == 200

    # 检测管线未受影响（后续请求正常计数）
    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=20.0,
    )
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before >= 1
    after_errors = metrics_client.get_counter("vllm_anomaly_detection_errors_total")
    assert after_errors - before_errors == 0