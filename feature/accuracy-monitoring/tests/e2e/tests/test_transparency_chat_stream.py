from __future__ import annotations

import pytest

from tests.e2e.utils.baseline import CHAT_VARIANTS
from tests.e2e.utils.compare import assert_stream_transparent

pytestmark = pytest.mark.asyncio


@pytest.mark.P0
@pytest.mark.lightweight
@pytest.mark.full
@pytest.mark.nightly
async def test_transparency_chat_stream(http_client, baseline_store):
    messages = [{"role": "user", "content": "写一句关于上海生活的话"}]
    kwargs = {"max_tokens": 50, "temperature": 0, "seed": 42}

    for variant, extra in CHAT_VARIANTS.items():
        events = []
        async for event in http_client.chat_stream(messages=messages, **kwargs, **extra):
            events.append(event)

        assert events and events[-1].get("done"), f"chat stream v{variant}"
        baseline = baseline_store.load_chat_stream(variant)
        assert_stream_transparent(events, baseline, note=f"chat stream v{variant}")