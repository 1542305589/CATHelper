"""detector_runner 单元测试：lazy 构造 / run_sync / run_async / unusable / 异常隔离 /
topk_n / set_vocabulary 注入（spec §2.5 §2.6 §2.7 §6.5）。"""
from __future__ import annotations

import asyncio

import pytest

from anomaly_middleware.env import PluginConfig, resolve_config_path
from anomaly_middleware.detector_runner import DetectorRunner, schedule_detection
from anomaly_middleware.metrics import Metrics


@pytest.fixture
def vendored_config():
    return resolve_config_path()


def _normal_data():
    # 单 choice，2 token，每 token 3 个 topk 候选
    topk = [[{1: -0.1, 2: -2.0, 3: -3.0}, {1: -0.2, 2: -2.0, 3: -3.0}]]
    tokens = [[1, 2]]
    return topk, tokens


def test_run_sync_valid(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1)
    topk, tokens = _normal_data()
    results = runner.run_sync(topk, tokens)
    assert results == [[False, 0]]
    runner.shutdown()


@pytest.mark.asyncio
async def test_run_async_valid(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1)
    topk, tokens = _normal_data()
    results = await runner.run_async(topk, tokens)
    assert results == [[False, 0]]
    runner.shutdown()


def test_construction_failure_marks_unusable(tmp_path):
    runner = DetectorRunner(
        str(tmp_path / "nope.yaml"),
        max_workers=1,
    )
    topk, tokens = _normal_data()
    # 首次：构造失败 → 抛异常 + 标记 unusable
    with pytest.raises(Exception):
        runner.run_sync(topk, tokens)
    assert runner._unusable is True
    # 第二次：快速失败（不再尝试构造）
    with pytest.raises(RuntimeError):
        runner.run_sync(topk, tokens)
    runner.shutdown()


@pytest.mark.asyncio
async def test_schedule_detection_records(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1)
    metrics = Metrics()
    pending = set()
    topk, tokens = _normal_data()
    task = schedule_detection(
        runner, topk, tokens,
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
        max_workers=1,
    )
    runner._unusable = True
    runner._unusable_reason = "test"
    metrics = Metrics()
    pending = set()
    topk, tokens = _normal_data()
    task = schedule_detection(
        runner, topk, tokens,
        request_id="rid", model="m", metrics=metrics, pending_tasks=pending,
    )
    await asyncio.wait_for(task, timeout=10)
    text = metrics.render_metrics().decode()
    assert "vllm_anomaly_detection_errors_total 1" in text
    runner.shutdown()


@pytest.mark.asyncio
async def test_detection_serialized_single_worker(vendored_config):
    """单 worker + 锁：多次 run_sync 串行（不并发，避免实例态竞争）。"""
    runner = DetectorRunner(vendored_config, max_workers=1)
    topk, tokens = _normal_data()
    # 串行多次调用，均正常返回
    for _ in range(3):
        assert runner.run_sync(topk, tokens) == [[False, 0]]
    runner.shutdown()


# --------------------------- topk_n 参数化（Task 4） --------------------------- #
def test_runner_topk_n_stored(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1, topk_n=3)
    assert runner._topk_n == 3
    runner.shutdown()


def test_runner_topk_n_truncates_larger_data(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1, topk_n=3)
    big = [{1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5},
           {1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5}]
    results = runner.run_sync([big], [[1, 2]])
    assert results == [[False, 0]]
    runner.shutdown()


def test_runner_set_vocabulary_injects_into_lazy_detector(vendored_config):
    """set_vocabulary 缓存映射；_get_detector 懒构造时注入到 ILLDetector。"""
    runner = DetectorRunner(vendored_config, max_workers=1)
    runner.set_vocabulary({"1": "chinese_cjk"}, 100)
    topk = [[{1: -0.1, 2: -2.0, 3: -3.0}, {1: -0.2, 2: -2.0, 3: -3.0}]]
    tokens = [[1, 2]]
    runner.run_sync(topk, tokens)  # 触发懒构造
    det = runner._detector
    assert det is not None
    assert det._precomputed_tk2cat == {"1": "chinese_cjk"}
    assert det._precomputed_vocab_size == 100
    runner.shutdown()
