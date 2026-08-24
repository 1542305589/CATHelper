"""detector_runner 单元测试：run_async / topk_n / tk2cat 构造注入 / 异常隔离 /
schedule_detection 指标（spec §2.5 §2.6 §2.7 §6.5, design §4.4）。

重构后 runner 为 ProcessPoolExecutor 架构：
- 构造期创建进程池，worker 初始化时构造 ILLDetector + 注入 tk2cat（无 lazy/无 _unusable）。
- run_async(logprobs_list, token_ids_list) 经共享内存零拷贝检测，返回 [[is_ill, ill_type], ...]。
- 进程池崩溃（BrokenProcessPool）→ 重建 + 计 error（spec §2.13）。
"""
from __future__ import annotations

import asyncio

import numpy as np
import pytest
from concurrent.futures.process import BrokenProcessPool

from anomaly_middleware.env import resolve_config_path
from anomaly_middleware.detector_runner import DetectorRunner, schedule_detection
from anomaly_middleware.metrics import Metrics


@pytest.fixture
def vendored_config():
    return resolve_config_path()


def _arrays(rows):
    """list[{tid: logprob}] -> (logprobs_2d, token_ids_2d)，不足补 (-100, 0)。"""
    n = len(rows)
    k = max((len(r) for r in rows), default=0)
    lp = np.full((n, k), -100.0, dtype=np.float32)
    tid = np.zeros((n, k), dtype=np.int32)
    for i, d in enumerate(rows):
        for j, (t, v) in enumerate(d.items()):
            tid[i, j] = t
            lp[i, j] = v
    return lp, tid


def _normal_arrays():
    # 单 choice，2 token，每 token 3 个 topk 候选；top-1=1（高概率）-> 正常
    return _arrays([{1: -0.1, 2: -2.0, 3: -3.0}, {1: -0.2, 2: -2.0, 3: -3.0}])


@pytest.mark.asyncio
async def test_run_async_valid(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1)
    lp, tid = _normal_arrays()
    results = await runner.run_async([lp], [tid])
    assert results == [[False, 0]]
    runner.shutdown()


@pytest.mark.asyncio
async def test_bad_config_raises_on_run(tmp_path):
    # 检测器配置缺失：worker 初始化失败 -> BrokenProcessPool（spec §2.13 推理期进程池崩溃）
    runner = DetectorRunner(str(tmp_path / "nope.yaml"), max_workers=1)
    lp, tid = _normal_arrays()
    with pytest.raises(BrokenProcessPool):
        await runner.run_async([lp], [tid])
    runner.shutdown()


@pytest.mark.asyncio
async def test_schedule_detection_records(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1)
    metrics = Metrics()
    pending = set()
    lp, tid = _normal_arrays()
    task = schedule_detection(
        runner, [lp], [tid],
        request_id="rid", model="glm-4-7", metrics=metrics, pending_tasks=pending,
    )
    await asyncio.wait_for(task, timeout=30)
    text = metrics.render_metrics().decode()
    assert "vllm_anomaly_requests_total 1" in text
    assert pending == set()  # done_callback 出集
    runner.shutdown()


@pytest.mark.asyncio
async def test_schedule_detection_error_isolation(tmp_path):
    # 不可用 runner（配置缺失）：检测快速失败 -> 计 error，不抛（spec §2.13）
    runner = DetectorRunner(str(tmp_path / "nope.yaml"), max_workers=1)
    metrics = Metrics()
    pending = set()
    lp, tid = _normal_arrays()
    task = schedule_detection(
        runner, [lp], [tid],
        request_id="rid", model="m", metrics=metrics, pending_tasks=pending,
    )
    await asyncio.wait_for(task, timeout=30)
    text = metrics.render_metrics().decode()
    assert "vllm_anomaly_detection_errors_total 1" in text
    runner.shutdown()


@pytest.mark.asyncio
async def test_detection_serialized_single_worker(vendored_config):
    """单 worker：多次 run_async 串行，均正常返回。"""
    runner = DetectorRunner(vendored_config, max_workers=1)
    lp, tid = _normal_arrays()
    for _ in range(3):
        assert await runner.run_async([lp], [tid]) == [[False, 0]]
    runner.shutdown()


# --------------------------- topk_n 参数化 --------------------------- #
def test_runner_topk_n_stored(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1, topk_n=3)
    assert runner._topk_n == 3
    runner.shutdown()


@pytest.mark.asyncio
async def test_runner_topk_n_truncates_larger_data(vendored_config):
    runner = DetectorRunner(vendored_config, max_workers=1, topk_n=3)
    lp, tid = _arrays([{1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5},
                       {1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5}])
    results = await runner.run_async([lp], [tid])
    assert results == [[False, 0]]
    runner.shutdown()


# --------------------------- tk2cat 构造注入 --------------------------- #
def test_runner_tk2cat_cached_at_construction(vendored_config):
    """tk2cat 在构造期缓存到 runner._tk2cat/_vocab_size，并经 initargs 注入 worker。"""
    runner = DetectorRunner(
        vendored_config, max_workers=1,
        tk2cat={"1": "chinese_cjk"}, vocab_size=100,
    )
    assert runner._tk2cat == {"1": "chinese_cjk"}
    assert runner._vocab_size == 100
    runner.shutdown()
