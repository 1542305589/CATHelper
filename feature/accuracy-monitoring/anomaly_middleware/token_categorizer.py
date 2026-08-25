# -------------------------------------------------------------------------
#  This file is part of the MindStudio project.
# Copyright (c) 2025 Huawei Technologies Co.,Ltd.
#
# MindStudio is licensed under Mulan PSL v2.
# You can use this software according to the terms and conditions of the Mulan PSL v2.
# You may obtain a copy of Mulan PSL v2 at:
#
#          http://license.coscl.org.cn/MulanPSL2
#
# THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
# EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
# MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
# See the Mulan PSL v2 for more details.
# -------------------------------------------------------------------------
"""token 分类纯函数 + 运行时 token2category 映射生成（spec §4.2 / §4.4）。

本模块为可导入模块：分类纯函数（categorize_token / invert_vocab / _classify_char）
供检测器与中间件共享；`generate_tk2cat(tokenizer)` 在运行时从已加载 tokenizer 直接
生成 `{str(token_id): category}` 映射，由中间件通过 set_vocabulary 注入检测器，
不再落盘为预生成文件。

transformers 改懒导入（本模块不强依赖 transformers）；decode 降级链优先
backend_tokenizer.decoder.decode，退到 tokenizer.decode，均无则 raise（调用方降级）。
"""
from __future__ import annotations

import unicodedata
from collections import Counter
from dataclasses import dataclass
from functools import lru_cache
from typing import Dict, Optional, Tuple


@dataclass
class TokenInfo:
    token_id: int
    category: str


SCRIPT_LABELS = {
    "cjk": "chinese_cjk",
    "hiragana": "japanese_hiragana",
    "katakana": "japanese_katakana",
    "hangul": "korean_hangul",
    "thai": "thai",
    "greek": "greek",
    "variation_selector": "variation_selector",
    "latin": "english_latin",
    "latin_space": "english_latin_space",
    "digit": "numbers",
    "emoji": "emoji",
    "whitespace": "whitespace",
    "punct": "punctuation",
    "symbol": "symbol",
    "control": "control",
    "arabic": "arabic",
    "cyrillic": "cyrillic",
    "devanagari": "devanagari",
    "math_letter": "mathematics",
    "modifier_letter": "mathematics",
    "fraction": "mathematics",
}

PUNCT_CHARS = set("'`\".;:!?-–—()[]{}<>/\\@#*$%&+|~^=_")
WHITESPACE_CHARS = set(" \t\n\r\f\v▁ĠĊ█")


@lru_cache(maxsize=4096)
def _classify_char(ch):
    if ch in WHITESPACE_CHARS:
        return "whitespace"
    codepoint = ord(ch)
    if 0x1F300 <= codepoint <= 0x1FAFF:
        return "emoji"
    if ch.isdigit():
        return "digit"
    name = unicodedata.name(ch, "")
    if not name:
        category = unicodedata.category(ch)
        if category.startswith("C"):
            return "control"
        if category.startswith("P"):
            return "punct"
        if category.startswith("S"):
            return "symbol"
        return "other"
    name_upper = name.upper()
    if "PLANCK CONSTANT" in name_upper or "MATHEMATICAL" in name_upper or "DOUBLE-STRUCK CAPITAL" in name_upper:
        return "math_letter"
    if "MODIFIER LETTER" in name_upper:
        return "modifier_letter"
    if "SPACE" in name_upper or unicodedata.category(ch) in {"Zs", "Zl", "Zp"}:
        return "whitespace"
    if "CJK UNIFIED IDEOGRAPH" in name_upper or "CJK COMPATIBILITY" in name_upper:
        return "cjk"
    if "HIRAGANA" in name_upper:
        return "hiragana"
    if "HANGUL" in name_upper:
        return "hangul"
    if "THAI" in name_upper:
        return "thai"
    if "ARABIC" in name_upper:
        return "arabic"
    if "CYRILLIC" in name_upper:
        return "cyrillic"
    if "DEVANAGARI" in name_upper:
        return "devanagari"
    if "LATIN" in name_upper:
        return "latin"
    if "VARIATION SELECTOR" in name_upper:
        return "variation_selector"
    category = unicodedata.category(ch)
    if category.startswith("P"):
        return "punct"
    if category.startswith("S"):
        return "symbol"
    if category.startswith("C"):
        return "control"
    return "other"


def categorize_token(token_id, token_raw, decoded):
    char_counts = Counter()
    printable = 0
    for char in decoded:
        char_class = _classify_char(char)
        char_counts[char_class] += 1
        if char_class not in {"control"}:
            printable += 1

    total_chars = sum(char_counts.values()) or 1
    printable_fraction = printable / total_chars
    dominant, dom_count = char_counts.most_common(1)[0] if char_counts else ("other", 0)
    dominant_ratio = dom_count / total_chars

    label = SCRIPT_LABELS.get(dominant, "other")
    if dominant == "latin" and printable_fraction > 0.8:
        if "whitespace" in char_counts:
            label = "english_latin_space"
        else:
            label = "english_latin"
    elif dominant == "digit" and dominant_ratio > 0.6:
        label = "numbers"
    elif dominant == "punct" and dominant_ratio > 0.7:
        label = "punctuation"
    elif dominant == "symbol" and dominant_ratio > 0.6:
        label = "symbol_cluster"
    elif dominant == "control":
        label = "control_bytes"
    elif dominant in {
        "cjk",
        "hiragana",
        "katakana",
        "hangul",
        "thai",
        "greek",
        "variation_selector",
    }:
        label = SCRIPT_LABELS[dominant]
    elif dominant == "whitespace" and printable < 0.4:
        label = "whitespace"
    elif dominant_ratio < 0.5 and printable_fraction < 0.7:
        label = "mixed_noise"

    if label not in {"punctuation", "symbol_cluster", "numbers"} and dominant not in {
        "latin",
        "cjk",
        "hiragana",
        "katakana",
        "hangul",
        "thai",
    }:
        dense_symbol_ratio = (char_counts.get("symbol", 0) + char_counts.get("punct", 0)) / total_chars
        if dense_symbol_ratio > 0.6 and total_chars >= 3:
            label = "gibberish_symbols"
    return TokenInfo(token_id=token_id, category=label)


def invert_vocab(vocab):
    size = max(vocab.values()) + 1
    tokens = ["" for _ in range(size)]
    for token, idx in vocab.items():
        if idx < size:
            tokens[idx] = token
    return tokens


# --------------------------------------------------------------------------- #
# decode 降级链 + 运行时 generate_tk2cat（spec §4.2 / §4.4）
# --------------------------------------------------------------------------- #
def _get_decode_fn(tokenizer):
    """返回 (token_str, idx) -> Optional[str] 的闭包，或 None。

    优先 backend_tokenizer.decoder.decode（最精确）；
    退到 tokenizer.decode([idx])（高层 API，覆盖慢速 tokenizer）；均无则 None。
    """
    backend = getattr(tokenizer, "backend_tokenizer", None)
    decoder = getattr(backend, "decoder", None) if backend is not None else None
    if decoder is not None and hasattr(decoder, "decode"):
        def _backend(token, idx):
            return decoder.decode([token])
        return _backend
    if hasattr(tokenizer, "decode"):
        def _highlevel(token, idx):
            return tokenizer.decode([idx])
        return _highlevel
    return None


def _safe_decode(decode_fn, token, idx):
    """逐 token decode + 异常吞掉；失败或无 decode_fn 返回 None（该 token 跳过）。"""
    if decode_fn is None:
        return None
    try:
        return decode_fn(token, idx)
    except Exception:
        return None


def generate_tk2cat(tokenizer) -> Tuple[Dict[str, str], int]:
    """从 tokenizer 生成 {str(token_id): category} 映射 + vocab_size。

    返回 (id_to_category, vocab_size)。无可用 decode 路径 -> raise（调用方降级）。
    逐 token 解码失败 -> 跳过（不入映射），不影响其余 token。
    """
    vocab = tokenizer.get_vocab()
    tokens = invert_vocab(vocab)
    vocab_size = tokenizer.vocab_size

    decode_fn = _get_decode_fn(tokenizer)
    if decode_fn is None:
        raise RuntimeError(
            "tokenizer 无可用 decode 路径"
            "（backend_tokenizer.decoder.decode / tokenizer.decode 均缺失）"
        )

    id_to_category: Dict[str, str] = {}
    for idx, token in enumerate(tokens):
        decoded = _safe_decode(decode_fn, token, idx)
        if decoded is None:
            continue
        info = categorize_token(idx, token, decoded)
        id_to_category[str(info.token_id)] = info.category
    return id_to_category, vocab_size
