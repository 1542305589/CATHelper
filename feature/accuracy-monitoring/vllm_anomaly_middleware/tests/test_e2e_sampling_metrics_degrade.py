"""e2e: 采样 / 指标 / 降级 / 检测异常隔离（spec §2.6 §2.8 §2.10 §2.11 §2.13）。"""
from __future__ import annotations

import json
from unittest.mock import patch

import pytest

from _helpers import build_chat_response, chat_top_entry
from vllm_anomaly_middleware.config import PluginConfig, resolve_config_path
from vllm_anomaly_middleware.detector_runner import DetectorRunner
from vllm_anomaly_middleware.metrics import METRICS_CONTENT_TYPE
from conftest import drain

NI = "你"
pytestmark = pytest.mark.asyncio


def _chat_fn(model="glm-4-7", n_top=20):
    def fn(scope, body):
        e = chat_top_entry(100, NI, -0.1, n_top=n_top)
        return ("json", build_chat_response(model, [e]))
    return fn


async def test_metrics_endpoint(client_factory):
    client, fake, mw = client_factory(_chat_fn())
    resp = await client.get("/anomaly/metrics")
    assert resp.status_code == 200
    assert resp.headers["content-type"] == METRICS_CONTENT_TYPE
    assert b"vllm_anomaly" in resp.content
    # 下游无 /anomaly/metrics 路由（fake 未被调用）
    assert len(fake.received) == 0


async def test_sample_rate_zero_passthrough(client_factory):
    # 采样率 0.0 → 不注入、不检测，请求直接透传
    client, fake, mw = client_factory(_chat_fn(), sample_rate=0.0)
    resp = await client.post(
        "/v1/chat/completions", json={"model": "m", "messages": []}
    )
    assert resp.status_code == 200
    # 下游收到的 body 未被注入（无 return_tokens_as_token_ids）
    injected = json.loads(fake.received[0][1])
    assert "return_tokens_as_token_ids" not in injected
    assert "logprobs" not in injected
    # 未检测
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 0" in text


async def test_sample_rate_one_all_injected(client_factory):
    # 采样率 1.0 → 每请求都注入检测
    client, fake, mw = client_factory(_chat_fn(), sample_rate=1.0)
    await client.post("/v1/chat/completions", json={"model": "m", "messages": []})
    await client.post("/v1/chat/completions", json={"model": "m", "messages": []})
    for scope, body in fake.received:
        assert json.loads(body).get("return_tokens_as_token_ids") is True
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 2" in text


async def test_sample_rate_partial_with_patched_random(client_factory, monkeypatch):
    # 采样率 0.3，patch random 使 0.1<0.3 选中注入，0.5>=0.3 透传
    import vllm_anomaly_middleware.middleware as mwmod

    seq = iter([0.1, 0.5])
    monkeypatch.setattr(mwmod.random, "random", lambda: next(seq))
    client, fake, mw = client_factory(_chat_fn(), sample_rate=0.3)
    r1 = await client.post("/v1/chat/completions", json={"model": "m", "messages": []})
    r2 = await client.post("/v1/chat/completions", json={"model": "m", "messages": []})
    b1 = json.loads(fake.received[0][1])
    b2 = json.loads(fake.received[1][1])
    assert b1.get("return_tokens_as_token_ids") is True  # 0.1<0.3 选中注入
    assert "return_tokens_as_token_ids" not in b2  # 0.5>=0.3 透传
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 1" in text  # 仅 1 次检测


async def test_degrade_when_paths_unresolvable(client_factory, monkeypatch):
    # 模拟路径解析失败 → 永久降级透传 + 指标端点仍 200 报零
    import vllm_anomaly_middleware.middleware as mwmod

    monkeypatch.setattr(mwmod, "resolve_config_path", lambda c: None)
    client, fake, mw = client_factory(_chat_fn())
    resp = await client.post(
        "/v1/chat/completions", json={"model": "m", "messages": []}
    )
    assert resp.status_code == 200
    # 降级：body 未注入
    injected = json.loads(fake.received[0][1])
    assert "return_tokens_as_token_ids" not in injected
    # 永久降级
    assert mw.config.enabled is False
    # 指标端点仍可达报零
    mresp = await client.get("/anomaly/metrics")
    assert mresp.status_code == 200
    assert b"vllm_anomaly_requests_total 0" in mresp.content
    # 后续请求原样转发
    await client.post("/v1/chat/completions", json={"model": "m", "messages": []})
    assert "return_tokens_as_token_ids" not in json.loads(fake.received[1][1])


async def test_detection_error_isolation(client_factory):
    # 预置不可用 runner：检测快速失败计 error，客户端响应不受影响
    client, fake, mw = client_factory(_chat_fn())
    # 强制 runner 不可用
    paths = resolve_config_path(PluginConfig())
    runner = DetectorRunner(paths, max_workers=1)
    runner._unusable = True
    runner._unusable_reason = "forced for test"
    mw._runner = runner
    mw._runner_inited = True
    resp = await client.post(
        "/v1/chat/completions", json={"model": "glm-4-7", "messages": []}
    )
    assert resp.status_code == 200  # 客户端响应不受影响
    # 响应仍正常恢复
    assert resp.json()["choices"][0]["logprobs"] is None
    await drain(mw)
    text = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_detection_errors_total 1" in text
    # 后续请求正常处理（仍注入恢复；检测仍计 error）
    resp2 = await client.post(
        "/v1/chat/completions", json={"model": "glm-4-7", "messages": []}
    )
    assert resp2.status_code == 200
    await drain(mw)
    text2 = mw.metrics.render_metrics().decode()
    assert "vllm_anomaly_detection_errors_total 2" in text2
    runner.shutdown()


async def test_disabled_master_switch_passthrough(client_factory):
    # 总开关 False → 不注入不检测，指标端点仍可达
    client, fake, mw = client_factory(_chat_fn(), enabled=False)
    resp = await client.post(
        "/v1/chat/completions", json={"model": "m", "messages": []}
    )
    assert resp.status_code == 200
    assert "return_tokens_as_token_ids" not in json.loads(fake.received[0][1])
    mresp = await client.get("/anomaly/metrics")
    assert mresp.status_code == 200
    assert b"vllm_anomaly_requests_total 0" in mresp.content


async def test_non_target_passthrough_no_injection(client_factory):
    # 非 POST 或非目标路径 → 透传不改 body
    client, fake, mw = client_factory(_chat_fn())
    await client.post("/v1/some/other", json={"model": "m"})
    await client.get("/v1/chat/completions")
    for scope, body in fake.received:
        if not body:
            continue  # GET 无 body
        assert "return_tokens_as_token_ids" not in json.loads(body)


async def test_non_json_body_passthrough(client_factory):
    # 非 JSON 请求体 → 透传，不注入
    client, fake, mw = client_factory(_chat_fn())
    resp = await client.post(
        "/v1/chat/completions",
        content=b"not json",
        headers={"content-type": "text/plain"},
    )
    assert resp.status_code == 200
    # 下游收到原始 body
    assert fake.received[0][1] == b"not json"
