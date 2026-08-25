from __future__ import annotations

import pytest

from tests.e2e.utils.baseline import CHAT_VARIANTS, COMPLETIONS_VARIANTS

pytestmark = pytest.mark.asyncio


@pytest.fixture
def no_mw_http(vllm_service_no_mw, served_name):
    from tests.e2e.client.http_client import HttpClient

    return HttpClient(vllm_service_no_mw.url, served_name)


@pytest.mark.P0
@pytest.mark.lightweight
@pytest.mark.full
@pytest.mark.nightly
async def test_baseline_collection(no_mw_http, baseline_store):
    chat_messages = [{"role": "user", "content": "写一句关于上海生活的话"}]
    completions_prompt = "写一句关于上海生活的话"
    kwargs = {"max_tokens": 50, "temperature": 0, "seed": 42}

    for variant, extra in CHAT_VARIANTS.items():
        resp = await no_mw_http.chat(messages=chat_messages, **kwargs, **extra)
        assert resp.status_code == 200, f"chat nonstream v{variant}: {resp.status_code}"
        baseline_store.store_chat_nonstream(variant, resp.json())

    for variant, extra in CHAT_VARIANTS.items():
        events = []
        async for event in no_mw_http.chat_stream(messages=chat_messages, **kwargs, **extra):
            events.append(event)
        assert events and events[-1].get("done"), f"chat stream v{variant}"
        baseline_store.store_chat_stream(variant, events)

    for variant, extra in COMPLETIONS_VARIANTS.items():
        resp = await no_mw_http.completions(prompt=completions_prompt, **kwargs, **extra)
        assert resp.status_code == 200, f"completions nonstream v{variant}: {resp.status_code}"
        baseline_store.store_completions_nonstream(variant, resp.json())

    for variant, extra in COMPLETIONS_VARIANTS.items():
        events = []
        async for event in no_mw_http.completions_stream(
            prompt=completions_prompt, **kwargs, **extra
        ):
            events.append(event)
        assert events and events[-1].get("done"), f"completions stream v{variant}"
        baseline_store.store_completions_stream(variant, events)
