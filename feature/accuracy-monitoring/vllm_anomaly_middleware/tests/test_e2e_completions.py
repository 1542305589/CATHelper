"""e2e: completions 非流式 — 注入 / 响应恢复 / 无 token_id 泄漏（spec §2.2 §2.3）。"""
from __future__ import annotations

import json

import pytest

from _helpers import build_completions_response
from conftest import drain

pytestmark = pytest.mark.asyncio


def _comp_resp_fn(model="glm-4-7", n_top=20):
    def fn(scope, body):
        return (
            "json",
            build_completions_response(model, [100, 200], [-0.1, -0.2], n_top=n_top),
        )
    return fn


async def test_completions_injection_and_restore_no_logprobs(client_factory):
    client, fake, mw = client_factory(_comp_resp_fn())
    resp = await client.post(
        "/v1/completions",
        json={"model": "glm-4-7", "prompt": "hi"},
    )
    assert resp.status_code == 200
    injected = json.loads(fake.received[0][1])
    assert injected["logprobs"] == 20  # 注入 N
    assert injected["return_tokens_as_token_ids"] is True
    # 恢复：客户端未请求 → logprobs=null，无 token_id:
    assert resp.json()["choices"][0]["logprobs"] is None
    assert "token_id:" not in resp.text
    assert "x-anomaly-request-id" in resp.headers


async def test_completions_inject_max(client_factory):
    # 客户端 logprobs=5, N=20 → 注入 20
    client, fake, mw = client_factory(_comp_resp_fn(n_top=20), top_logprobs=20)
    await client.post("/v1/completions", json={"model": "m", "prompt": "x", "logprobs": 5})
    assert json.loads(fake.received[0][1])["logprobs"] == 20
    # 客户端 logprobs=10, N=5 → 注入 10
    client2, fake2, _ = client_factory(_comp_resp_fn(n_top=20), top_logprobs=5)
    await client2.post("/v1/completions", json={"model": "m", "prompt": "x", "logprobs": 10})
    assert json.loads(fake2.received[0][1])["logprobs"] == 10


async def test_completions_restore_token_ids_kept(client_factory):
    # 客户端 logprobs=3, return_tokens_as_token_ids=True → 截断 3，保留 token_id:
    client, fake, mw = client_factory(_comp_resp_fn(n_top=20))
    resp = await client.post(
        "/v1/completions",
        json={"model": "m", "prompt": "x", "logprobs": 3, "return_tokens_as_token_ids": True},
    )
    lp = resp.json()["choices"][0]["logprobs"]
    assert lp["tokens"] == ["token_id:100", "token_id:200"]
    assert len(lp["top_logprobs"]) == 2
    for pos in lp["top_logprobs"]:
        assert len(pos) == 3
        assert all(k.startswith("token_id:") for k in pos)


async def test_completions_restore_no_token_ids_null(client_factory):
    # 客户端 logprobs=3, 无 token_ids → tokens=null, top_logprobs=null（绝不留 token_id:）
    client, fake, mw = client_factory(_comp_resp_fn(n_top=20))
    resp = await client.post(
        "/v1/completions",
        json={"model": "m", "prompt": "x", "logprobs": 3},
    )
    lp = resp.json()["choices"][0]["logprobs"]
    assert lp["tokens"] == [None, None]
    assert lp["top_logprobs"] == [None, None]
    assert "token_id:" not in resp.text
