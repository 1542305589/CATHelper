"""ILLDetector 单测：set_vocabulary / 预计算 get_tk2cat / topk_n 截断（spec §4.6 §6.1 §5）。

重构后检测器仅依赖 config.yaml + 运行时注入的 tk2cat 映射（无 mtype_config /
token2category 文件 fallback）。get_tk2cat 返回预计算映射或 (None, None)。
"""
from __future__ import annotations

import os

import numpy as np
import pytest

import anomaly_middleware
from anomaly_middleware.detector import ILLDetector

_PKG = os.path.dirname(os.path.abspath(anomaly_middleware.__file__))
_PROJECT_ROOT = os.path.dirname(_PKG)
_CONFIG_PATH = os.path.join(_PROJECT_ROOT, "configs", "detector.yaml")


def _arrays(rows):
    """list[{tid: logprob}] -> (logprobs_2d, token_ids_2d)，列对齐，不足补 (-100, 0)。

    检测器内部按 logprob 降序重排（stable），故列顺序无关，只需 logprobs[i,j] 对齐
    token_ids[i,j]。最高 logprob 的 tid 即各位置 top-1（检测用的 token 序列）。
    """
    n = len(rows)
    k = max((len(r) for r in rows), default=0)
    lp = np.full((n, k), -100.0, dtype=np.float32)
    tid = np.zeros((n, k), dtype=np.int32)
    for i, d in enumerate(rows):
        for j, (t, v) in enumerate(d.items()):
            tid[i, j] = t
            lp[i, j] = v
    return lp, tid


@pytest.fixture
def detector():
    return ILLDetector(_CONFIG_PATH)


# --------------------------- set_vocabulary / get_tk2cat --------------------------- #
def test_set_vocabulary_then_get_tk2cat_returns_precomputed(detector):
    detector.set_vocabulary({"1": "chinese_cjk", "2": "whitespace"}, 100)
    tk2cat, vs = detector.get_tk2cat()
    assert tk2cat == {"1": "chinese_cjk", "2": "whitespace"}
    assert vs == 100


def test_set_vocabulary_idempotent(detector):
    detector.set_vocabulary({"1": "x"}, 10)
    detector.set_vocabulary({"9": "y"}, 99)  # 后者覆盖
    tk2cat, vs = detector.get_tk2cat()
    assert tk2cat == {"9": "y"}
    assert vs == 99


def test_get_tk2cat_no_precomputed_returns_none(detector):
    # 无预计算 -> (None, None)（降级为无词表检测，不崩溃）
    assert detector.get_tk2cat() == (None, None)


def test_get_tk2cat_no_args_needed(detector):
    # 重构后 get_tk2cat 无参（仅返回预计算）
    detector.set_vocabulary({"1": "x"}, 10)
    tk2cat, vs = detector.get_tk2cat()
    assert tk2cat == {"1": "x"}
    assert vs == 10


# --------------------------- _sort_and_truncate_topk --------------------------- #
def test_sort_and_truncate_topk_uses_param(detector):
    lp, tid = _arrays([{1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5}])
    out_lp, out_tid = detector._sort_and_truncate_topk(lp, tid, 3)
    assert out_tid.shape == (1, 3)
    # 降序：前 3 项 tid 为 1,2,3（logprob 最高）
    assert out_tid[0].tolist() == [1, 2, 3]


def test_sort_and_truncate_topk_more_than_available(detector):
    lp, tid = _arrays([{1: -0.1, 2: -0.2}])
    out_lp, out_tid = detector._sort_and_truncate_topk(lp, tid, 10)
    assert out_tid.shape == (1, 2)  # 切片取全部，不越界


# --------------------------- topk_n 参数化（无 self.topk 锁定） --------------------------- #
def test_detector_topk_n_truncates_without_locking(detector):
    """topk_n 参数化：每请求独立 topk，无 self.topk 锁定。"""
    # 首请求 topk_n=3 截断
    lp, tid = _arrays([{1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5},
                      {1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5}])
    detector.detector(lp, tid, topk_n=3)
    # 次请求 topk_n=5 不被锁定为 3（无 self.topk）
    lp2, tid2 = _arrays([{1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5}])
    detector.detector(lp2, tid2, topk_n=5)
    assert not hasattr(detector, "topk") or detector.__dict__.get("topk") is None


def test_detector_topk_n_none_uses_min_len(detector):
    """topk_n=None -> 用全部列，不锁定。"""
    lp, tid = _arrays([{1: -0.1, 2: -0.2, 3: -0.3}, {1: -0.1, 2: -0.2, 3: -0.3}])
    res = detector.detector(lp, tid)
    assert res.is_ill is False
    assert res.ill_type == 0


def test_run_with_topk_n(detector):
    lp, tid = _arrays([{1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5},
                       {1: -0.1, 2: -0.2, 3: -0.3, 4: -0.4, 5: -0.5}])
    results = detector.run([lp], [tid], topk_n=3)
    assert results == [[False, 0]]


def test_run_topk_n_none(detector):
    lp, tid = _arrays([{1: -0.1, 2: -2.0, 3: -3.0}, {1: -0.2, 2: -2.0, 3: -3.0}])
    results = detector.run([lp], [tid])
    assert results == [[False, 0]]


# --------------------------- 词表注入激活检测路径 --------------------------- #
def test_detector_with_vocabulary_uses_precomputed(detector):
    """注入映射后 get_tk2cat 返回预计算（不读文件、不依赖 model_config）。"""
    detector.set_vocabulary({"1": "chinese_cjk"}, 1000)
    lp, tid = _arrays([{1: -0.1, 2: -2.0, 3: -3.0}, {1: -0.2, 2: -2.0, 3: -3.0}])
    res = detector.detector(lp, tid, topk_n=3)
    assert res.ill_type in (0, 1, 2, 3, 4)
