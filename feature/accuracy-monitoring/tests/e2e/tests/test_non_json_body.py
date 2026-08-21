from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
async def test_non_json_body(http_client):
    resp = await http_client.post_raw(
        "/v1/chat/completions",
        b"this is not json {{{",
        {"Content-Type": "application/json"},
    )
    assert resp.status_code in (400, 422)
