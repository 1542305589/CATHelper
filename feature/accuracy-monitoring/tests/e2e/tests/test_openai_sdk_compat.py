from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_openai_sdk_compat(vllm_service_b, served_name):
    from tests.e2e.client.openai_sdk_client import OpenAISdkClient

    client = OpenAISdkClient(vllm_service_b.url, served_name)
    resp = client.chat(messages=[{"role": "user", "content": "Hello"}])
    assert resp["choices"][0]["message"]["content"]

    chunks = client.chat_stream(
        messages=[{"role": "user", "content": "Count to 3"}],
    )
    assert len(chunks) > 0
