from __future__ import annotations

import asyncio

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_client_disconnect(http_client, metrics_client):
    received = 0
    try:
        async for line in http_client.chat_stream(
            messages=[{"role": "user", "content": "Write a long story about the sea"}],
            stream=True,
        ):
            received += 1
            if received >= 1:
                break
    except Exception:
        pass
    await asyncio.sleep(2.0)
    resp = await http_client.chat(messages=[{"role": "user", "content": "Hello"}])
    assert resp.status_code == 200
