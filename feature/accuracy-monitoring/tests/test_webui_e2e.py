"""e2e 全链路：真实本地上游 + webui 后台轮询 → 事件 → 告警 → webhook；
API 加实例立即可见；外部改 yaml 热重载自动生效。"""
from __future__ import annotations

import asyncio
import os
import time

import httpx
import pytest

from webui.main import create_app
from tests._webui_helpers import (
    FakeUpstream,
    WebhookSink,
    build_webui_config_dict,
    write_yaml,
)


async def _wait_for(predicate, timeout: float = 15.0, interval: float = 0.2) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return True
        await asyncio.sleep(interval)
    return False


def _e2e_config(instances, alerts, webhook_url="", **overrides):
    data = build_webui_config_dict(
        instances=instances,
        alerts=alerts,
        auth={"username": "admin", "password": "test123", "token_ttl_hours": 1},
        **overrides,
    )
    data["poll"] = {"interval_seconds": 2, "http_timeout_seconds": 1}
    data["webhooks"]["default"] = webhook_url
    return data


@pytest.fixture
def upstream():
    u = FakeUpstream(
        step_requests=1,
        step_detections=[(2, "glm-4-7", "0", 1)],
    ).start()
    yield u
    u.stop()


@pytest.fixture
def sink():
    s = WebhookSink()
    yield s
    s.stop()


async def _login(client):
    r = await client.post("/api/login", json={"username": "admin", "password": "test123"})
    assert r.status_code == 200
    return {"Authorization": f"Bearer {r.json()['token']}"}


# ------------------------------------------------------------------ #
# 1) 全链路：轮询 → 事件 → 告警 → webhook
# ------------------------------------------------------------------ #
async def test_e2e_full_chain(tmp_path, upstream, sink):
    cfg_path = str(tmp_path / "webui.yaml")
    write_yaml(
        cfg_path,
        _e2e_config(
            instances=[{"name": "prod-1", "url": upstream.url}],
            alerts=[{"name": "garbled_frequent", "ill_type": "garbled",
                     "threshold": 1, "window_seconds": 300}],
            webhook_url=sink.url,
        ),
    )
    app = create_app(cfg_path)
    ctx = app.state.ctx
    await ctx.start()
    try:
        ok = await _wait_for(lambda: len(sink.received) > 0)
        assert ok, "webhook 未在超时内收到推送"
        payload = sink.received[0]
        assert payload["rule_name"] == "garbled_frequent"
        assert payload["instance"] == "prod-1"
        assert payload["ill_type"] == "garbled"

        summary = ctx.store.summary()
        assert summary["requests"] >= 1
        assert summary["anomalies"] >= 1
        assert len(ctx.store.recent_events(10)) >= 1
        assert len(ctx.store.recent_alerts(10)) >= 1
    finally:
        await ctx.stop()


async def test_e2e_summary_via_api(tmp_path, upstream, sink):
    cfg_path = str(tmp_path / "webui.yaml")
    write_yaml(
        cfg_path,
        _e2e_config(
            instances=[{"name": "prod-1", "url": upstream.url}],
            alerts=[],
        ),
    )
    app = create_app(cfg_path)
    ctx = app.state.ctx
    await ctx.start()
    try:
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app), base_url="http://test"
        ) as client:
            h = await _login(client)
            ok = await _wait_for(
                lambda: ctx.store.summary()["requests"] > 0
            )
            assert ok, "轮询未产生请求数据"
            r = await client.get("/api/summary", headers=h)
            assert r.json()["requests"] >= 1
            r = await client.get("/api/instances", headers=h)
            inst = r.json()[0]
            assert inst["name"] == "prod-1"
            assert inst["state"] == "online"
    finally:
        await ctx.stop()


# ------------------------------------------------------------------ #
# 2) API 加实例立即可见 + 轮询生效
# ------------------------------------------------------------------ #
async def test_e2e_add_instance_via_api(tmp_path, upstream, sink):
    cfg_path = str(tmp_path / "webui.yaml")
    write_yaml(cfg_path, _e2e_config(instances=[], alerts=[]))
    app = create_app(cfg_path)
    ctx = app.state.ctx
    await ctx.start()
    try:
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app), base_url="http://test"
        ) as client:
            h = await _login(client)
            r = await client.post(
                "/api/instances",
                json={"name": "added-1", "url": upstream.url},
                headers=h,
            )
            assert r.status_code == 201
            # 立即可见（无需重启）
            r = await client.get("/api/instances", headers=h)
            assert [i["name"] for i in r.json()] == ["added-1"]
            # 后台轮询接管 → 在线并产生事件
            ok = await _wait_for(
                lambda: ctx.store.summary()["anomalies"] >= 1
            )
            assert ok, "新增实例未开始轮询产出事件"
            assert ctx.store.instance_stats("added-1").state == "online"
    finally:
        await ctx.stop()


# ------------------------------------------------------------------ #
# 3) 外部直接改 yaml 热重载：暂停 → 恢复 自动生效
# ------------------------------------------------------------------ #
async def test_e2e_external_yaml_hot_reload(tmp_path, upstream, sink):
    cfg_path = str(tmp_path / "webui.yaml")
    write_yaml(
        cfg_path,
        _e2e_config(
            instances=[{"name": "prod-1", "url": upstream.url, "paused": True}],
            alerts=[],
        ),
    )
    app = create_app(cfg_path)
    ctx = app.state.ctx
    await ctx.start()
    try:
        # 初始：暂停态，无轮询任务
        assert "prod-1" not in ctx.collector._tasks
        # 外部编辑 yaml：恢复 prod-1
        time.sleep(0.05)
        write_yaml(
            cfg_path,
            _e2e_config(
                instances=[{"name": "prod-1", "url": upstream.url}],
                alerts=[],
            ),
        )
        os.utime(cfg_path, None)
        # 热重载循环 1s 内检测到 → 恢复轮询 → 在线
        ok = await _wait_for(
            lambda: ctx.store.instance_stats("prod-1") is not None
            and ctx.store.instance_stats("prod-1").state == "online",
        )
        assert ok, "外部 yaml 热重载未恢复实例轮询"
        ok = await _wait_for(lambda: ctx.store.summary()["requests"] > 0)
        assert ok
    finally:
        await ctx.stop()


# ------------------------------------------------------------------ #
# 4) 外部改 yaml 暂停 → 数据保留、轮询停止
# ------------------------------------------------------------------ #
async def test_e2e_external_yaml_pause_keeps_data(tmp_path, upstream, sink):
    cfg_path = str(tmp_path / "webui.yaml")
    write_yaml(
        cfg_path,
        _e2e_config(instances=[{"name": "prod-1", "url": upstream.url}], alerts=[]),
    )
    app = create_app(cfg_path)
    ctx = app.state.ctx
    await ctx.start()
    try:
        ok = await _wait_for(lambda: ctx.store.summary()["requests"] > 0)
        assert ok
        reqs_before = ctx.store.summary()["requests"]
        time.sleep(0.05)
        write_yaml(
            cfg_path,
            _e2e_config(
                instances=[{"name": "prod-1", "url": upstream.url, "paused": True}],
                alerts=[],
            ),
        )
        os.utime(cfg_path, None)
        ok = await _wait_for(lambda: "prod-1" not in ctx.collector._tasks)
        assert ok, "暂停未停止轮询任务"
        # 数据保留
        assert ctx.store.instance_stats("prod-1") is not None
        assert ctx.store.instance_stats("prod-1").state == "paused"
        assert ctx.store.summary()["requests"] == reqs_before
    finally:
        await ctx.stop()