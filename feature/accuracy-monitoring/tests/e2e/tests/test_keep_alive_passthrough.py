from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.inject
async def test_keep_alive_passthrough(
    vllm_service_b, injector, http_client, metrics_client,
):
    if not injector.health_check():
        pytest.skip("injector infrastructure not available")

    injector.set_override(
        "sse_keepalive",
        {"bytes": ": keep-alive\nretry: 3000\n\n"},
        count=1,
    )

    before = metrics_client.get_counter("vllm_anomaly_requests_total")

    raw = bytearray()
    events = []
    async for chunk in http_client.chat_stream_raw(
        messages=[{"role": "user", "content": "Hello"}],
        stream=True,
    ):
        raw.extend(chunk)
        events.append(chunk)

    # keep-alive/注释事件原样透传，客户端可见
    assert b": keep-alive" in bytes(raw)
    assert b"retry: 3000" in bytes(raw)

    # data 事件仍在且可解析、[DONE] 透传
    data_lines = [
        line for line in bytes(raw).split(b"\n") if line.startswith(b"data: ")
    ]
    assert data_lines, "no data events received"
    assert b'data: [DONE]' in bytes(raw)

    # 检测数据未被 keep-alive 污染：请求仍正常计数
    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=15.0,
    )
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1