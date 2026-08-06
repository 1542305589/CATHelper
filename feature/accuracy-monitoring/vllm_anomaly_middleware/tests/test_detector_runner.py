"""detector_runner 单元测试：lazy 构造 / run_sync / run_async / unusable / 异常隔离（spec §2.5 §2.6 §2.7）。"""
from __future__ import annotations

import asyncio

import pytest

from vllm_anomaly_middleware.config import PluginConfig, resolve_detector_paths
from vllm_anomaly_middleware.detector_runner import DetectorRunner, schedule_detection
from vllm_anomaly_middleware.metrics import Metrics


@pytest.fixture
def vendored_paths():
    return resolve_detector_paths(PluginConfig())


def _normal_data():
    # 单 choice，2 token，每 token 3 个 topk 候选
    topk = [[{1: -0.1, 2: -2.0, 3: -3.0}, {1: -0.2, 2: -2.0, 3: -3.0}]]
    tokens = [[1, 2]]
    configs = ["glm-4-7"]
    return topk, tokens, configs


def test_run_sync_valid(vendored_paths):
    cfg, mtype, tk2 = vendored_paths
    runner = DetectorRunner(cfg, mtype, tk2, max_workers=1)
    topk, tokens, configs = _normal_data()
    results = runner.run_sync(topk, tokens, configs)
    assert results == [[False, 0]]
    runner.shutdown()


@pytest.mark.asyncio
async def test_run_async_valid(vendored_paths):
    cfg, mtype, tk2 = vendored_paths
    runner = DetectorRunner(cfg, mtype, tk2, max_workers=1)
    topk, tokens, configs = _normal_data()
    results = await runner.run_async(topk, tokens, configs)
    assert results == [[False, 0]]
    runner.shutdown()


def test_construction_failure_marks_unusable(tmp_path):
    runner = DetectorRunner(
        str(tmp_path / "nope.yaml"),
        str(tmp_path / "nope.json"),
        str(tmp_path / "notdir"),
        max_workers=1,
    )
    topk, tokens, configs = _normal_data()
    # 首次：构造失败 → 抛异常 + 标记 unusable
    with pytest.raises(Exception):
        runner.run_sync(topk, tokens, configs)
    assert runner._unusable is True
    # 第二次：快速失败（不再尝试构造）
    with pytest.raises(RuntimeError):
        runner.run_sync(topk, tokens, configs)
    runner.shutdown()


@pytest.mark.asyncio
async def test_schedule_detection_records(vendored_paths):
    cfg, mtype, tk2 = vendored_paths
    runner = DetectorRunner(cfg, mtype, tk2, max_workers=1)
    metrics = Metrics()
    pending = set()
    topk, tokens, configs = _normal_data()
    task = schedule_detection(
        runner, topk, tokens, configs,
        request_id="rid", model="glm-4-7", metrics=metrics, pending_tasks=pending,
    )
    await asyncio.wait_for(task, timeout=30)
    text = metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 1" in text
    assert pending == set()  # done_callback 出集
    runner.shutdown()


@pytest.mark.asyncio
async def test_schedule_detection_error_isolation(tmp_path):
    # 不可用 runner：每次检测快速失败 → 计 error，不抛
    runner = DetectorRunner(
        str(tmp_path / "nope.yaml"),
        str(tmp_path / "nope.json"),
        str(tmp_path / "notdir"),
        max_workers=1,
    )
    runner._unusable = True
    runner._unusable_reason = "test"
    metrics = Metrics()
    pending = set()
    topk, tokens, configs = _normal_data()
    task = schedule_detection(
        runner, topk, tokens, configs,
        request_id="rid", model="m", metrics=metrics, pending_tasks=pending,
    )
    await asyncio.wait_for(task, timeout=10)
    text = metrics.render_metrics().decode()
    assert "vllm_anomaly_detection_errors_total 1" in text
    runner.shutdown()


@pytest.mark.asyncio
async def test_detection_serialized_single_worker(vendored_paths):
    """单 worker + 锁：多次 run_sync 串行（不并发，避免实例态竞争）。"""
    cfg, mtype, tk2 = vendored_paths
    runner = DetectorRunner(cfg, mtype, tk2, max_workers=1)
    topk, tokens, configs = _normal_data()
    # 串行多次调用，均正常返回
    for _ in range(3):
        assert runner.run_sync(topk, tokens, configs) == [[False, 0]]
    runner.shutdown()
