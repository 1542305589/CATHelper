from __future__ import annotations

import json
import os
import random
from collections import defaultdict
from typing import Dict, List, Tuple

EXCLUDED = {"english_latin", "english_latin_space", "punctuation", "whitespace"}


def _load_tokenizer(model_path: str):
    from transformers import AutoTokenizer

    return AutoTokenizer.from_pretrained(model_path, trust_remote_code=True)


def _generate_tk2cat(tokenizer) -> Tuple[Dict[str, str], int]:
    from anomaly_middleware.token_categorizer import generate_tk2cat

    return generate_tk2cat(tokenizer)


def _group_by_category(tk2cat: Dict[str, str]) -> Dict[str, List[int]]:
    by_cat: Dict[str, List[int]] = defaultdict(list)
    for tid_s, cat in tk2cat.items():
        by_cat[cat].append(int(tid_s))
    return by_cat


def _get_usable_categories(
    by_cat: Dict[str, List[int]], min_categories: int = 3
) -> List[Tuple[str, List[int]]]:
    usable = [
        (c, ids) for c, ids in by_cat.items() if c not in EXCLUDED and len(ids) > 0
    ]
    if len(usable) < min_categories:
        raise RuntimeError(
            f"model has insufficient surviving categories: "
            f"{len(usable)} < {min_categories}; available={sorted(by_cat.keys())}"
        )
    usable.sort(key=lambda x: -len(x[1]))
    return usable


def _build_guaranteed_position(
    categories: List[Tuple[str, List[int]]], topk: int, top1_logp: float
) -> Tuple[List[float], List[int]]:
    n_cats = min(len(categories), 3)
    selected = categories[:n_cats]

    tids: List[int] = []
    per_cat = topk // n_cats
    for _cat_name, cat_ids in selected:
        for j in range(per_cat):
            tids.append(cat_ids[j % len(cat_ids)])

    fill_cat = selected[0][1]
    while len(tids) < topk:
        tids.append(fill_cat[len(tids) % len(fill_cat)])

    tids = tids[:topk]
    lps = [top1_logp - j * 0.01 for j in range(topk)]
    return lps, tids


def _build_random_position(
    categories: List[Tuple[str, List[int]]], topk: int, top1_logp: float
) -> Tuple[List[float], List[int]]:
    all_ids: List[int] = []
    for _cat, ids in categories:
        all_ids.extend(ids)

    tids = [random.choice(all_ids) for _ in range(topk)]
    lps = [top1_logp - j * 0.01 for j in range(topk)]
    return lps, tids


def _distribute_guaranteed(n_positions: int, guaranteed: int) -> set:
    if guaranteed <= 0 or n_positions <= 0:
        return set()
    step = n_positions / guaranteed
    result = set()
    for i in range(guaranteed):
        idx = int(i * step)
        result.add(min(idx, n_positions - 1))
    return result


def build_rare_character(
    tk2cat: Dict[str, str],
    vocab_size: int,
    n_positions: int = 25,
    guaranteed: int = 3,
    topk: int = 20,
    top1_logp: float = -10.0,
) -> dict:
    by_cat = _group_by_category(tk2cat)
    usable = _get_usable_categories(by_cat, min_categories=3)

    guaranteed_idx = _distribute_guaranteed(n_positions, guaranteed)

    logprobs: List[List[float]] = []
    token_ids: List[List[int]] = []
    for i in range(n_positions):
        if i in guaranteed_idx:
            lps, tids = _build_guaranteed_position(usable, topk, top1_logp)
        else:
            lps, tids = _build_random_position(usable, topk, top1_logp)
        logprobs.append(lps)
        token_ids.append(tids)

    return {"logprobs": [logprobs], "token_ids": [token_ids]}


def build_garbled(
    tk2cat: Dict[str, str],
    vocab_size: int,
    n_positions: int = 140,
    guaranteed: int = 42,
    topk: int = 20,
    top1_logp: float = -10.0,
) -> dict:
    by_cat = _group_by_category(tk2cat)
    usable = _get_usable_categories(by_cat, min_categories=3)

    guaranteed_idx = _distribute_guaranteed(n_positions, guaranteed)

    logprobs: List[List[float]] = []
    token_ids: List[List[int]] = []
    for i in range(n_positions):
        if i in guaranteed_idx:
            lps, tids = _build_guaranteed_position(usable, topk, top1_logp)
        else:
            lps, tids = _build_random_position(usable, topk, top1_logp)
        logprobs.append(lps)
        token_ids.append(tids)

    return {"logprobs": [logprobs], "token_ids": [token_ids]}


def build_repetition(
    tk2cat: Dict[str, str],
    vocab_size: int,
    n_positions: int = 1024,
    topk: int = 20,
    top1_logp: float = -0.1,
) -> dict:
    valid_ids = [int(tid) for tid in tk2cat.keys()]
    tid = valid_ids[0] if valid_ids else 1
    lp_row = [top1_logp + j * 0.001 for j in range(topk)]
    ti_row = [tid] * topk
    logprobs = [list(lp_row) for _ in range(n_positions)]
    token_ids = [list(ti_row) for _ in range(n_positions)]
    return {"logprobs": [logprobs], "token_ids": [token_ids]}


def build_nan_value(topk: int = 20) -> dict:
    n_positions = 5
    logprobs: List[List[float]] = []
    token_ids: List[List[int]] = []
    for _ in range(n_positions):
        row: List[float] = [float("nan")] + [0.1] * (topk - 1)
        logprobs.append(row)
        token_ids.append([1] * topk)
    return {"logprobs": [logprobs], "token_ids": [token_ids]}


def build_inf_logprob(topk: int = 20) -> dict:
    n_positions = 5
    logprobs: List[List[float]] = []
    token_ids: List[List[int]] = []
    for _ in range(n_positions):
        row: List[float] = [float("inf")] + [0.1] * (topk - 1)
        logprobs.append(row)
        token_ids.append([1] * topk)
    return {"logprobs": [logprobs], "token_ids": [token_ids]}


def build_detection_error() -> dict:
    err_lp = [[0.1] * 20 for _ in range(10)]
    err_ti = [[1] * 20 for _ in range(5)]
    return {"logprobs": [err_lp], "token_ids": [err_ti]}


def verify_payload(
    payload: dict, tk2cat: Dict[str, str], vocab_size: int, expect_ill_type: int
) -> bool:
    import numpy as np

    from anomaly_middleware.detector import ILLDetector
    from anomaly_middleware.env import resolve_config_path

    det = ILLDetector(resolve_config_path())
    det.set_vocabulary(tk2cat, vocab_size)

    lp = np.array(payload["logprobs"][0], dtype=np.float32)
    ti = np.array(payload["token_ids"][0], dtype=np.int32)
    res = det.detector(lp, ti, topk_n=20)

    if not res.is_ill:
        raise AssertionError(f"payload did not trigger detection: {res}")
    if res.ill_type != expect_ill_type:
        raise AssertionError(
            f"expected ill_type={expect_ill_type}, got ill_type={res.ill_type}"
        )
    return True


_CACHE_NAMES = [
    "rare_character",
    "garbled",
    "repetition",
    "nan_value",
    "inf_logprob",
    "detection_error",
]


def _cache_dir_for(model_name: str, base_dir: str | None = None) -> str:
    if base_dir is None:
        base_dir = os.path.dirname(os.path.abspath(__file__))
    safe_name = model_name.replace("/", "_").replace("\\", "_")
    return os.path.join(base_dir, safe_name)


def _try_load_cache(cache_dir: str) -> dict | None:
    result: dict = {}
    for name in _CACHE_NAMES:
        path = os.path.join(cache_dir, f"{name}.json")
        if not os.path.exists(path):
            return None
        try:
            with open(path, "r", encoding="utf-8") as f:
                result[name] = json.load(f)
        except Exception:
            return None
    return result


def _save_cache(cache_dir: str, data: dict) -> None:
    os.makedirs(cache_dir, exist_ok=True)
    for name, payload in data.items():
        path = os.path.join(cache_dir, f"{name}.json")
        with open(path, "w", encoding="utf-8") as f:
            json.dump(payload, f)


def build_all(
    model_path: str,
    model_name: str,
    cache_dir: str | None = None,
    seed: int = 42,
) -> dict:
    random.seed(seed)

    cache_dir = _cache_dir_for(model_name, cache_dir)

    cached = _try_load_cache(cache_dir)
    if cached is not None:
        return cached

    tokenizer = _load_tokenizer(model_path)
    tk2cat, vocab_size = _generate_tk2cat(tokenizer)

    rare = build_rare_character(tk2cat, vocab_size)
    garbled = build_garbled(tk2cat, vocab_size)
    repetition = build_repetition(tk2cat, vocab_size)

    verify_payload(rare, tk2cat, vocab_size, expect_ill_type=1)
    verify_payload(garbled, tk2cat, vocab_size, expect_ill_type=2)

    data = {
        "rare_character": rare,
        "garbled": garbled,
        "repetition": repetition,
        "nan_value": build_nan_value(),
        "inf_logprob": build_inf_logprob(),
        "detection_error": build_detection_error(),
    }

    _save_cache(cache_dir, data)
    return data
