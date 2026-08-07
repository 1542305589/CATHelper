"""middleware 助手 + 分派单元测试：_read_all_body / _make_replay_receive /
_patch_scope_content_length / __call__ 分派（spec §2.1 §2.10 §2.11 §2.13）。"""
from __future__ import annotations

import json

import pytest

from vllm_anomaly_middleware.middleware import (
    AnomalyMiddleware,
    _make_replay_receive,
    _patch_scope_content_length,
    _read_all_body,
)
from vllm_anomaly_middleware.metrics import METRICS_CONTENT_TYPE
from vllm_anomaly_middleware.config import PluginConfig


# --------------------------- _read_all_body --------------------------- #
@pytest.mark.asyncio
async def test_read_all_body_single():
    async def receive():
        return {"type": "http.request", "body": b"hello", "more_body": False}
    assert await _read_all_body(receive) == b"hello"


@pytest.mark.asyncio
async def test_read_all_body_multi_chunk():
    msgs = [
        {"type": "http.request", "body": b"foo", "more_body": True},
        {"type": "http.request", "body": b"bar", "more_body": False},
    ]
    async def receive():
        return msgs.pop(0)
    assert await _read_all_body(receive) == b"foobar"


@pytest.mark.asyncio
async def test_read_all_body_disconnect():
    async def receive():
        return {"type": "http.disconnect"}
    assert await _read_all_body(receive) == b""


# --------------------------- _make_replay_receive --------------------------- #
@pytest.mark.asyncio
async def test_replay_receive_first_synthetic_then_delegate():
    calls = []
    async def original_receive():
        calls.append("orig")
        return {"type": "http.disconnect"}
    replay = _make_replay_receive(original_receive, b"BODY")
    first = await replay()
    assert first == {"type": "http.request", "body": b"BODY", "more_body": False}
    second = await replay()
    assert second == {"type": "http.disconnect"}
    assert calls == ["orig"]


@pytest.mark.asyncio
async def test_replay_receive_never_empty_body_second_call():
    # 二次读绝不返回空 body 的 http.request
    async def original_receive():
        return {"type": "http.disconnect"}
    replay = _make_replay_receive(original_receive, b"B")
    await replay()
    second = await replay()
    assert second["type"] == "http.disconnect"  # 非 http.request


# --------------------------- _patch_scope_content_length --------------------------- #
def test_patch_scope_rewrites_existing_cl():
    scope = {"headers": [[b"content-type", b"application/json"], [b"content-length", b"5"]]}
    new = _patch_scope_content_length(scope, 99)
    assert new is not scope  # 浅拷贝
    assert dict(new["headers"])["content-length".encode()] in (b"99",)
    # 原始 scope 未变
    assert scope["headers"][1][1] == b"5"


def test_patch_scope_adds_missing_cl():
    scope = {"headers": [[b"content-type", b"application/json"]]}
    new = _patch_scope_content_length(scope, 42)
    cls = dict((h[0], h[1]) for h in new["headers"])
    assert cls[b"content-length"] == b"42"


def test_patch_scope_headers_copied_not_shared():
    scope = {"headers": [[b"content-length", b"1"]]}
    new = _patch_scope_content_length(scope, 2)
    new["headers"][0][1] = b"x"
    # 原始 headers 列表未被改
    assert scope["headers"][0][1] == b"1"


# --------------------------- AnomalyMiddleware 分派 --------------------------- #
def _make_mw_with_fake(fake_app, **cfg):
    mw = AnomalyMiddleware(fake_app)
    mw.config = PluginConfig(
        enabled=cfg.get("enabled", True),
        top_logprobs=cfg.get("top_logprobs", 20),
        sample_rate=cfg.get("sample_rate", 1.0),
    )
    mw._runner = None
    mw._runner_inited = False
    return mw


class _Recorder:
    def __init__(self):
        self.calls = []

    async def __call__(self, scope, receive, send):
        self.calls.append((scope.get("type"), scope.get("method"), scope.get("path")))
        if scope.get("type") != "http":
            return  # 非 http（lifespan 等）不发送 http 消息
        # 读掉 body（避免下游未消费）
        async for _ in _recv_iter(receive):
            pass
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.body", "body": b"", "more_body": False})


async def _recv_iter(receive):
    while True:
        msg = await receive()
        if msg["type"] == "http.request" and not msg.get("more_body"):
            yield msg
            return
        if msg["type"] == "http.disconnect":
            return
        yield msg


async def _drive(mw, method, path, body=b"", headers=None):
    sent = []
    async def send(m):
        sent.append(m)
    scope = {
        "type": "http",
        "method": method,
        "path": path,
        "headers": headers or [[b"content-type", b"application/json"], [b"content-length", str(len(body)).encode()]],
    }
    msg = {"delivered": False}
    async def receive():
        if not msg["delivered"]:
            msg["delivered"] = True
            return {"type": "http.request", "body": body, "more_body": False}
        return {"type": "http.disconnect"}
    await mw(scope, receive, send)
    return sent


@pytest.mark.asyncio
async def test_dispatch_non_http_passthrough():
    rec = _Recorder()
    mw = _make_mw_with_fake(rec)

    async def nope():
        return None

    await mw({"type": "lifespan"}, nope, nope)  # 非 http → 透传给下游
    assert rec.calls == [("lifespan", None, None)]


@pytest.mark.asyncio
async def test_dispatch_get_metrics_endpoint():
    rec = _Recorder()
    mw = _make_mw_with_fake(rec)
    sent = await _drive(mw, "GET", "/anomaly/metrics")
    assert sent[0]["status"] == 200
    hdrs = dict((h[0], h[1]) for h in sent[0]["headers"])
    assert hdrs[b"content-type"] == METRICS_CONTENT_TYPE.encode("latin-1")
    assert b"vllm_anomaly" in sent[1]["body"]
    assert rec.calls == []  # 下游未被调用


@pytest.mark.asyncio
async def test_dispatch_get_models_passthrough():
    rec = _Recorder()
    mw = _make_mw_with_fake(rec)
    await _drive(mw, "GET", "/v1/models")
    assert len(rec.calls) == 1
    assert rec.calls[0][1] == "GET"


@pytest.mark.asyncio
async def test_dispatch_get_chat_completions_passthrough():
    # spec §2.1：GET /v1/chat/completions → 原样转发
    rec = _Recorder()
    mw = _make_mw_with_fake(rec)
    await _drive(mw, "GET", "/v1/chat/completions")
    assert len(rec.calls) == 1


@pytest.mark.asyncio
async def test_dispatch_disabled_passthrough_no_body_read():
    # spec §2.11/§2.13：enabled=False → 不注入不检测，透传
    rec = _Recorder()
    mw = _make_mw_with_fake(rec, enabled=False)
    body = json.dumps({"model": "m", "messages": []}).encode()
    await _drive(mw, "POST", "/v1/chat/completions", body=body)
    assert len(rec.calls) == 1


# --------------------------- _ensure_resolver --------------------------- #
class _FakeTok:
    def __init__(self, m):
        self._m = m

    def decode(self, ids, **kw):
        return "".join(self._m.get(i, "") for i in ids)


def _make_mw_for_resolver():
    rec = _Recorder()
    mw = AnomalyMiddleware(rec)
    mw.config = PluginConfig(enabled=True, top_logprobs=20)
    mw._runner = object()  # 跳过 runner 构造
    mw._runner_inited = True
    return mw


@pytest.mark.asyncio
async def test_ensure_resolver_uses_acquire_and_caches(monkeypatch):
    mw = _make_mw_for_resolver()
    import vllm_anomaly_middleware.token_resolver as tr

    async def fake_acquire(hint, server, explicit=None):
        assert hint == "m"
        assert server == ("127.0.0.1", 8000)
        return _FakeTok({1: "x"})

    monkeypatch.setattr(tr, "acquire_tokenizer", fake_acquire)
    r = await mw._ensure_resolver("m", ("127.0.0.1", 8000))
    assert r is not None
    assert r.resolve(1) == "x"

    # 双检锁：第二次不重复 acquire
    called = {"n": 0}

    async def counting(hint, server, explicit=None):
        called["n"] += 1
        return _FakeTok({})

    monkeypatch.setattr(tr, "acquire_tokenizer", counting)
    r2 = await mw._ensure_resolver("m", ("127.0.0.1", 8000))
    assert r2 is r  # 同一实例
    assert called["n"] == 0


@pytest.mark.asyncio
async def test_ensure_resolver_failure_returns_none(monkeypatch):
    mw = _make_mw_for_resolver()
    import vllm_anomaly_middleware.token_resolver as tr

    async def fail(hint, server):
        raise RuntimeError("boom")

    monkeypatch.setattr(tr, "acquire_tokenizer", fail)
    r = await mw._ensure_resolver("m", ("127.0.0.1", 8000))
    assert r is None  # 失败软降级，不抛
