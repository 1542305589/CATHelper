"""collector 单元测试：解析、离线标记、基线语义、实例生命周期、轮询定时。"""
from __future__ import annotations

import asyncio

import httpx
import pytest

from webui.alerts import AlertEngine, AlertRule
from webui.collector import Collector, MetricsParseError, parse_metrics_text
from webui.config import InstanceConfig, PollConfig, StoreConfig
from webui.events import AnomalyEvent, DeltaSummary, EventSynthesizer
from webui.store import Store
from tests._webui_helpers import make_metrics_text


def _store() -> Store:
    return Store(
        StoreConfig(event_capacity=1000, alert_capacity=200,
                    raw_trend_window_seconds=3600, trend_bucket_seconds=60,
                    trend_horizon_seconds=86400)
    )


def _collector(store, alert_engine=None, poll_cfg=None):
    synth = EventSynthesizer()
    synth.set_id_source(store.alloc_event_id)
    alerts = alert_engine or AlertEngine(store.alerts, id_allocer=store.alloc_alert_id)
    return Collector(
        store=store,
        synthesizer=synth,
        alerts=alerts,
        poll_cfg=poll_cfg or PollConfig(interval_seconds=3, http_timeout_seconds=2),
    )


# ------------------------------------------------------------------ #
# 解析
# ------------------------------------------------------------------ #
def test_parse_full_metrics_text():
    text = make_metrics_text(
        requests=7,
        detections=[(1, "m1", "0", 2), (2, "m1", "0", 1), (4, "m2", "1", 3)],
        errors=2,
        duration_count=3,
    )
    parsed = parse_metrics_text(text)
    assert parsed["requests"] == 7
    assert parsed["errors"] == 2
    assert parsed["detected"] == {(1, "m1", "0"): 2, (2, "m1", "0"): 1, (4, "m2", "1"): 3}
    assert parsed["gauges"][("garbled", "m1")] == 1
    assert parsed["gauges"][("nan_value", "m2")] == 1
    assert parsed["hist_stats"]["count"] == 3
    assert parsed["hist_stats"]["mean"] == pytest.approx(0.05)


def test_parse_corrupted_series_skipped():
    text = (
        "# HELP vllm_anomaly_requests_total req\n"
        "# TYPE vllm_anomaly_requests_total counter\n"
        "vllm_anomaly_requests_total 3\n"
        "# HELP vllm_anomaly_detected_total det\n"
        "# TYPE vllm_anomaly_detected_total counter\n"
        'vllm_anomaly_detected_total{ill_type="2",model="m"} 1\n'  # 缺 choice_index
        'vllm_anomaly_detected_total{ill_type="1",model="m",choice_index="0"} 2\n'
    )
    parsed = parse_metrics_text(text)
    assert parsed["requests"] == 3
    assert parsed["detected"] == {(1, "m", "0"): 2}


def test_parse_invalid_text_raises():
    with pytest.raises(MetricsParseError):
        parse_metrics_text("this is not prometheus text {{{")


def test_parse_empty_text():
    assert parse_metrics_text("")["requests"] == 0
    assert parse_metrics_text("# hello")["detected"] == {}


# ------------------------------------------------------------------ #
# 轮询 + 增量
# ------------------------------------------------------------------ #
class FakeClient:
    def __init__(self):
        self.responses = []

    async def get(self, url):
        return self.responses.pop(0)

    async def aclose(self):
        pass


def test_poll_once_baseline_then_delta():
    store = _store()
    col = _collector(store)
    col._client = FakeClient()
    col._instances = {"a": InstanceConfig(name="a", url="http://v:8000")}
    col._client.responses = [
        httpx.Response(200, text=make_metrics_text(2, [(2, "m", "0", 1)])),
        httpx.Response(200, text=make_metrics_text(5, [(2, "m", "0", 4)])),
    ]

    async def run():
        assert await col.poll_once("a") is None  # 基线
        d = await col.poll_once("a")
        assert d is not None
        assert d.requests == 3
        assert d.anomalies_total == 3
        assert len(d.events) == 3
        assert store.instance_stats("a").state == "online"
        assert store.instance_stats("a").requests == 3
        assert len(store.recent_events(10)) == 3

    asyncio.run(run())


async def test_poll_loop_marks_offline_on_non_200():
    store = _store()
    col = _collector(store)
    col._client = FakeClient()
    col._instances = {"a": InstanceConfig(name="a", url="http://v:8000")}
    col._client.responses = [httpx.Response(500, text="boom")]
    task = asyncio.create_task(col._poll_loop(col._instances["a"]))
    await asyncio.sleep(0.05)
    assert store.instance_stats("a").state == "offline"
    task.cancel()
    await asyncio.gather(task, return_exceptions=True)


async def test_poll_loop_marks_offline_on_timeout():
    store = _store()
    col = _collector(store)
    col._instances = {"a": InstanceConfig(name="a", url="http://v:8000")}

    class TimeoutClient:
        async def get(self, url):
            raise httpx.TimeoutException("timeout")

        async def aclose(self):
            pass

    col._client = TimeoutClient()
    task = asyncio.create_task(col._poll_loop(col._instances["a"]))
    await asyncio.sleep(0.05)
    assert store.instance_stats("a").state == "offline"
    task.cancel()
    await asyncio.gather(task, return_exceptions=True)


async def test_poll_loop_marks_offline_on_parse_error():
    store = _store()
    col = _collector(store)
    col._client = FakeClient()
    col._instances = {"a": InstanceConfig(name="a", url="http://v:8000")}
    col._client.responses = [httpx.Response(200, text="garbage $$$")]
    task = asyncio.create_task(col._poll_loop(col._instances["a"]))
    await asyncio.sleep(0.05)
    assert store.instance_stats("a").state == "offline"
    task.cancel()
    await asyncio.gather(task, return_exceptions=True)


async def test_poll_loop_recovers_after_failure():
    """失败后下一轮成功 → 转在线；恢复首份仅基线，再下一轮才产生增量。"""
    store = _store()
    col = _collector(store, poll_cfg=PollConfig(interval_seconds=2, http_timeout_seconds=1))
    col._client = FakeClient()
    col._instances = {"a": InstanceConfig(name="a", url="http://v:8000")}
    col._client.responses = [
        httpx.Response(500, text="boom"),
        httpx.Response(200, text=make_metrics_text(1, [(2, "m", "0", 1)])),
        httpx.Response(200, text=make_metrics_text(2, [(2, "m", "0", 2)])),
    ]
    task = asyncio.create_task(col._poll_loop(col._instances["a"]))
    await asyncio.sleep(0.1)
    assert store.instance_stats("a").state == "offline"
    await asyncio.sleep(2.1)  # 恢复轮：首份基线，转在线
    assert store.instance_stats("a").state == "online"
    assert store.instance_stats("a").requests == 0
    assert len(store.recent_events(10)) == 0
    await asyncio.sleep(2.1)  # 增量轮
    assert store.instance_stats("a").requests == 1
    assert len(store.recent_events(10)) == 1
    task.cancel()
    await asyncio.gather(task, return_exceptions=True)


async def test_resume_first_snapshot_baseline_skips_jump():
    """暂停/恢复：reset 后下一快照仅作基线，计数器跳变不产生虚假事件。"""
    store = _store()
    col = _collector(store)
    col._client = FakeClient()
    col._instances = {"a": InstanceConfig(name="a", url="http://v:8000")}
    col._client.responses = [
        httpx.Response(200, text=make_metrics_text(1, [(2, "m", "0", 1)])),
        httpx.Response(200, text=make_metrics_text(100, [(2, "m", "0", 50)])),
    ]
    assert await col.poll_once("a") is None  # 基线
    d = await col.poll_once("a")
    assert d is not None and d.requests == 99 and len(d.events) == 49
    col._synth.reset("a")  # 模拟恢复：清基线
    col._client.responses.append(
        httpx.Response(200, text=make_metrics_text(60, [(2, "m", "0", 20)]))
    )
    assert await col.poll_once("a") is None  # 恢复首份仅基线（跳变被忽略）
    # 值变小也不产生负增量
    col._client.responses.append(
        httpx.Response(200, text=make_metrics_text(61, [(2, "m", "0", 21)]))
    )
    d = await col.poll_once("a")
    assert d is not None and d.requests == 1 and len(d.events) == 1


# ------------------------------------------------------------------ #
# 实例生命周期 / apply_instances
# ------------------------------------------------------------------ #
async def test_apply_instances_creates_and_removes_tasks():
    store = _store()
    col = _collector(store)
    col.start()
    col.apply_instances([InstanceConfig(name="a", url="http://v:1")])
    assert "a" in col._tasks
    col.apply_instances([])
    assert "a" not in col._tasks
    assert store.instance_stats("a") is None  # 删除→数据清除


async def test_apply_instances_pause_keeps_data():
    store = _store()
    col = _collector(store)
    col.start()
    col.apply_instances([InstanceConfig(name="a", url="http://v:1")])
    d = DeltaSummary(requests=1)
    store.record_delta("a", d)
    col.apply_instances([InstanceConfig(name="a", url="http://v:1", paused=True)])
    assert "a" not in col._tasks
    assert store.instance_stats("a").state == "paused"
    assert store.instance_stats("a").requests == 1  # 数据保留


async def test_apply_instances_resume_rebuilds_baseline():
    store = _store()
    col = _collector(store)
    col.start()
    col.apply_instances([InstanceConfig(name="a", url="http://v:1", paused=True)])
    assert "a" not in col._tasks
    col.apply_instances([InstanceConfig(name="a", url="http://v:1")])
    assert "a" in col._tasks


async def test_apply_instances_url_change_resets_baseline():
    store = _store()
    col = _collector(store)
    col.start()
    col.apply_instances([InstanceConfig(name="a", url="http://v:1")])
    col.apply_instances([InstanceConfig(name="a", url="http://v:2")])
    assert "a" in col._tasks


async def test_apply_instances_idempotent_no_purge():
    store = _store()
    col = _collector(store)
    col.start()
    insts = [InstanceConfig(name="a", url="http://v:1")]
    col.apply_instances(insts)
    col.apply_instances(insts)
    assert "a" in col._tasks


async def test_shutdown_cancels_tasks():
    store = _store()
    col = _collector(store)
    col.start()
    col.apply_instances([InstanceConfig(name="a", url="http://localhost:1")])
    await col.shutdown()
    assert col._tasks == {}


async def test_collector_integrated_with_store_and_alerts():
    """真实链路：解析 → 增量 → 事件 → 告警。"""
    store = _store()
    alerts = AlertEngine(store.alerts, id_allocer=store.alloc_alert_id)
    alerts.set_rules([AlertRule(name="r", ill_type=None, threshold=2, window_seconds=300)])
    col = _collector(store, alert_engine=alerts)
    col._client = FakeClient()
    col._instances = {"a": InstanceConfig(name="a", url="http://v:8000")}
    col._client.responses = [
        httpx.Response(200, text=make_metrics_text(1, [(2, "m", "0", 1)])),
        httpx.Response(200, text=make_metrics_text(3, [(2, "m", "0", 3)])),
    ]
    await col.poll_once("a")
    await col.poll_once("a")
    assert len(store.recent_events(10)) == 2
    assert len(store.recent_alerts(10)) == 1
    alert = store.recent_alerts(10)[0]
    assert alert.count == 2
    assert alert.ill_type == "garbled"