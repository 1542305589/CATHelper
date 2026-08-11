"""ILLDetector 算法功能全覆盖：生僻字(1)/乱码(2)/重复(3)/NaN(4) 四类异常 + 窗口/状态辅助。

对齐 spec §2.5 与 §4.5：detector() 返回 DetectionResult(is_ill, ill_type)。
现有 test_detector.py 主要覆盖配置注入与 topk_n 截断；本文件补齐算法输出路径，
用确定性构造数据验证每类异常的可检出性与正常样本的假阳性控制。
"""
from __future__ import annotations

import numpy as np
import pytest

from anomaly_middleware.detector import ILLDetector

CONFIG = "configs/detector.yaml"


@pytest.fixture
def det():
    return ILLDetector(CONFIG)


# --------------------------------------------------------------------------- #
# 辅助方法：窗口 / n-gram / 乱码状态（§4.5）
# --------------------------------------------------------------------------- #
def test_get_ngrams_empty_when_shorter_than_n(det):
    assert det.get_ngrams([1, 2]) == []  # repet_n=3, 2 个 token < 3


def test_get_ngrams_counts(det):
    gs = det.get_ngrams([1, 1, 2, 2])
    assert len(gs) == 2  # (1,1,2),(1,2,2)


def test_get_distinct_n_all_same_is_low(det):
    # 全相同 token -> distinct_n 低（重复度高）。8 个 token 得 6 个相同 3-gram -> 1/6≈0.167
    assert det.get_distinct_n([1, 1, 1, 1, 1, 1, 1, 1]) < det.repet_distinct_n_thresh


def test_get_distinct_n_high_for_varied(det):
    # 多样 token -> distinct_n 高（不判重复）
    assert det.get_distinct_n([1, 2, 3, 4, 5, 6, 7, 8]) > det.repet_distinct_n_thresh


def test_sliding_window_yields_stride_windows(det):
    seq = list(range(0, 200))
    starts = [i for i, _ in det.sliding_window(seq)]
    assert starts == [0, 64, 128, 192]  # stride=64
    sizes = [len(w) for _, w in det.sliding_window(seq)]
    assert sizes == [128, 128, 72, 8]  # window_size=128, 末窗不完整


def test_update_garbled_state_threshold(det):
    # window_thresh=0 -> 首个乱码窗口即退出
    det._garbled_count = 0
    assert det._update_garbled_state(True) is True
    det._garbled_count = 0
    assert det._update_garbled_state(False) is False


# --------------------------------------------------------------------------- #
# ill_type=1 生僻字（带词表：category 判定）
# --------------------------------------------------------------------------- #
def _vocab_tk2cat():
    """topk token id 映射多类别：3=hiragana,4=hangul,5=whitespace,6=punct,7=latin。"""
    return {str(i): ["chinese_cjk", "japanese_hiragana", "korean_hangul",
                     "whitespace", "punctuation", "english_latin"][i % 6]
            for i in range(2, 20)}


def test_detect_rare_character_with_vocab(det):
    """explogp 总和 < 0.4 且 topk 类别数 > category_thresh(2) -> 生僻字。"""
    det.set_vocabulary(_vocab_tk2cat(), 20)
    topk = [{3: -5.0, 4: -5.5, 5: -6.0, 6: -6.5, 7: -7.0} for _ in range(10)]
    tokens = [3] * 10  # 短序列 < stride -> 只走生僻字路径
    res = det.detector(topk, tokens, topk_n=5)
    assert res.is_ill is True
    assert res.ill_type == 1


def test_detect_rare_character_with_vocab_filtered_out(det):
    """topk 类别被过滤（english_latin/whitespace/punct）-> 不计为生僻字。"""
    det.set_vocabulary({str(i): "english_latin" for i in range(2, 20)}, 20)
    topk = [{2: -5.0, 3: -5.5, 4: -6.0, 5: -6.5, 6: -7.0} for _ in range(10)]
    res = det.detector(topk, [2] * 10, topk_n=5)
    assert res.is_ill is False  # 全部 latin -> 不触发


def test_detect_rare_character_no_vocab_uses_top1_logp(det):
    """无词表降级：top1 logp < rare_top1_logp_thresh(-6) 即生僻字。"""
    topk = [{1: -10.0, 2: -11.0, 3: -12.0} for _ in range(10)]
    res = det.detector(topk, [1] * 10, topk_n=3)
    assert res.is_ill is True
    assert res.ill_type == 1


def test_short_normal_sequence_no_rare(det):
    """短序列 + 高概率 topk -> 正常（假阳性控制）。"""
    topk = [{1: -0.1, 2: -0.2, 3: -0.3} for _ in range(8)]
    res = det.detector(topk, [1, 1, 1, 1, 1, 1, 1, 1], topk_n=3)
    assert res.is_ill is False
    assert res.ill_type == 0


# --------------------------------------------------------------------------- #
# ill_type=2 乱码（无词表：top1 logp + ratio）
# --------------------------------------------------------------------------- #
def test_detect_garbled_no_vocab(det):
    """整窗 top1 logp 极低（<-5）占比 > 0.2 -> 乱码。长序列走滑窗。"""
    n = 140  # > stride=64，进入滑窗检测
    topk = [{1000: -20.0, 1001: -21.0, 1002: -22.0, 1003: -23.0, 1004: -24.0}
            for _ in range(n)]
    tokens = [1000] * n
    res = det.detector(topk, tokens, topk_n=5)
    assert res.is_ill is True
    assert res.ill_type == 2


def test_detect_garbled_no_vocab_ratio_below_thresh(det):
    """低概率占比 <= 0.2 且 logp 未达 rare 阈值(-6) -> 乱码/生僻字均不触发。"""
    n = 100
    # 前 15 个 logp=-5.5（-6 < -5.5 < -5），占比 0.15 <= 0.2；
    # -5.5 > rare_top1_thresh(-6) -> 不触发生僻字；占比 <= 0.2 -> 不触发乱码
    topk = []
    for i in range(n):
        if i < 15:
            topk.append({1: -5.5, 2: -6.0})
        else:
            topk.append({1: -0.1, 2: -0.2})
    tokens = [1] * n
    res = det.detector(topk, tokens, topk_n=2)
    assert res.is_ill is False


def test_detect_garbled_no_vocab_ratio_above_thresh(det):
    """低概率占比 > 0.2 且 logp 介于 (-6, -5) -> 仅乱码触发（ill_type=2）。"""
    n = 100
    topk = []
    for i in range(n):
        if i < 25:  # 占比 0.25 > 0.2
            topk.append({1: -5.5, 2: -6.0})
        else:
            topk.append({1: -0.1, 2: -0.2})
    tokens = [1] * n
    res = det.detector(topk, tokens, topk_n=2)
    assert res.is_ill is True
    assert res.ill_type == 2


# --------------------------------------------------------------------------- #
# ill_type=3 重复（trajectory 单方法，需 > single_window_thresh 窗口）
# --------------------------------------------------------------------------- #
def test_detect_repetition_trajectory_only(det):
    """长序列全相同 token + 高 logp -> distinct_n 低 -> 重复。"""
    n = 1024  # 提供 > single_window_thresh(14) 个完整窗口
    topk = [{1: -0.1, 2: -1.0, 3: -2.0} for _ in range(n)]
    tokens = [1] * n
    res = det.detector(topk, tokens, topk_n=3)
    assert res.is_ill is True
    assert res.ill_type == 3


def test_detect_repetition_short_sequence_normal(det):
    """短序列无重复 -> 正常。"""
    topk = [{1: -0.1, 2: -0.2} for _ in range(20)]
    res = det.detector(topk, [1, 2, 1, 2, 1, 2, 1, 2, 1, 2,
                              1, 2, 1, 2, 1, 2, 1, 2, 1, 2], topk_n=2)
    assert res.is_ill is False


# --------------------------------------------------------------------------- #
# ill_type=4 NaN（logprob 出现 nan/inf）
# --------------------------------------------------------------------------- #
def test_detect_nan_value(det):
    topk = [{1: float("nan"), 2: -1.0, 3: -2.0} for _ in range(5)]
    res = det.detector(topk, [1, 1, 1, 1, 1], topk_n=3)
    assert res.is_ill is True
    assert res.ill_type == 4


def test_detect_inf_value(det):
    topk = [{1: float("inf"), 2: -1.0, 3: -2.0} for _ in range(5)]
    res = det.detector(topk, [1, 1, 1, 1, 1], topk_n=3)
    assert res.is_ill is True
    assert res.ill_type == 4


# --------------------------------------------------------------------------- #
# run()：多请求并行返回，一请求异常不影响其他
# --------------------------------------------------------------------------- #
def test_run_multi_request_isolation(det):
    nan_topk = [{1: float("nan")} for _ in range(5)]
    normal_topk = [{1: -0.1, 2: -0.2} for _ in range(5)]
    results = det.run([normal_topk, nan_topk, normal_topk],
                      [[1] * 5, [1] * 5, [1] * 5], topk_n=2)
    assert results == [[False, 0], [True, 4], [False, 0]]
