from __future__ import annotations

import pytest

from tests.e2e.utils.baseline import COMPLETIONS_VARIANTS
from tests.e2e.utils.compare import assert_response_transparent

pytestmark = pytest.mark.asyncio


@pytest.mark.P0
@pytest.mark.lightweight
@pytest.mark.full
@pytest.mark.nightly
async def test_transparency_completions_nonstream(http_client, baseline_store):
    prompt = "写一句关于上海生活的话"
    kwargs = {"max_tokens": 50, "temperature": 0, "seed": 42}

    for variant, extra in COMPLETIONS_VARIANTS.items():
        resp = await http_client.completions(prompt=prompt, **kwargs, **extra)
        assert resp.status_code == 200, f"completions nonstream v{variant}: {resp.status_code}"
        baseline = baseline_store.load_completions_nonstream(variant)
        assert_response_transparent(
            resp.json(), baseline, note=f"completions nonstream v{variant}"
        )