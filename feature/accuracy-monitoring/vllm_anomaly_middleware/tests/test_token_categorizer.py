"""generate_tk2cat + decode 降级链单测（spec §4.2 / §4.4）。

不依赖 transformers：用伪 tokenizer 模拟 get_vocab / vocab_size /
backend_tokenizer.decoder.decode / tokenizer.decode。
"""
from __future__ import annotations

import pytest

from vllm_anomaly_middleware.token_categorizer import (
    generate_tk2cat,
    _get_decode_fn,
    _safe_decode,
    categorize_token,
    invert_vocab,
)


class _FakeDecoder:
    """模拟 tokenizers.Backend decoder：decode([token_str]) -> text。"""

    def __init__(self, token_to_text, raise_tokens=()):
        self._m = token_to_text
        self._raise = set(raise_tokens)

    def decode(self, tokens):
        out = []
        for t in tokens:
            if t in self._raise:
                raise ValueError("boom")
            out.append(self._m.get(t, ""))
        return "".join(out)


class _FakeBackend:
    def __init__(self, token_to_text, raise_tokens=()):
        self.decoder = _FakeDecoder(token_to_text, raise_tokens)


class FakeTokenizer:
    """伪 HF tokenizer：vocab / vocab_size / backend_tokenizer / decode。"""

    def __init__(self, vocab, token_to_text, with_backend=True, raise_tokens=()):
        self._vocab = dict(vocab)
        self.vocab_size = max(vocab.values()) + 1
        self.backend_tokenizer = (
            _FakeBackend(token_to_text, raise_tokens) if with_backend else None
        )
        self._token_to_text = token_to_text

    def get_vocab(self):
        return self._vocab

    def decode(self, ids):
        inv = {v: k for k, v in self._vocab.items()}
        return "".join(self._token_to_text.get(inv.get(i, ""), "") for i in ids)


# --------------------------- generate_tk2cat --------------------------- #
def test_generate_tk2cat_backend_decoder():
    vocab = {"你": 0, "好": 1, " ": 2, "abc": 3}
    t2t = {"你": "你", "好": "好", " ": " ", "abc": "abc"}
    tok = FakeTokenizer(vocab, t2t, with_backend=True)
    mapping, vs = generate_tk2cat(tok)
    assert vs == 4
    assert mapping["0"] == "chinese_cjk"
    assert mapping["1"] == "chinese_cjk"
    assert mapping["2"] == "whitespace"
    assert mapping["3"] == "english_latin"


def test_generate_tk2cat_fallback_highlevel_decode():
    # 无 backend_tokenizer -> 退到 tokenizer.decode([idx])
    vocab = {"你": 0, " ": 1}
    t2t = {"你": "你", " ": " "}
    tok = FakeTokenizer(vocab, t2t, with_backend=False)
    mapping, vs = generate_tk2cat(tok)
    assert vs == 2
    assert mapping["0"] == "chinese_cjk"
    assert mapping["1"] == "whitespace"


def test_generate_tk2cat_skips_undecodable_token():
    # backend decoder 对某 token 抛异常 -> 该 token 跳过（不入映射）
    vocab = {"ok": 0, "bad": 1}
    t2t = {"ok": "x", "bad": "y"}
    tok = FakeTokenizer(vocab, t2t, with_backend=True, raise_tokens=("bad",))
    mapping, vs = generate_tk2cat(tok)
    assert vs == 2
    assert "0" in mapping  # "ok" -> "x" 正常分类
    assert "1" not in mapping  # "bad" 抛异常 -> 跳过


def test_generate_tk2cat_no_decode_path_raises():
    class BareTok:
        vocab_size = 1

        def get_vocab(self):
            return {"a": 0}
        # 无 backend_tokenizer，无 decode

    with pytest.raises(RuntimeError):
        generate_tk2cat(BareTok())


def test_generate_tk2cat_keys_are_strings():
    vocab = {"你": 5}
    tok = FakeTokenizer(vocab, {"你": "你"})
    mapping, _ = generate_tk2cat(tok)
    assert "5" in mapping
    assert all(isinstance(k, str) for k in mapping)


# --------------------------- decode 降级链 --------------------------- #
def test_get_decode_fn_prefers_backend():
    tok = FakeTokenizer({"a": 0}, {"a": "a"}, with_backend=True)
    fn = _get_decode_fn(tok)
    assert fn is not None
    assert fn("a", 0) == "a"  # backend decoder.decode([token])


def test_get_decode_fn_falls_back_to_highlevel():
    tok = FakeTokenizer({"a": 0}, {"a": "a"}, with_backend=False)
    fn = _get_decode_fn(tok)
    assert fn is not None
    assert fn("a", 0) == "a"  # tokenizer.decode([idx])


def test_get_decode_fn_none_when_both_missing():
    class BareTok:
        pass

    assert _get_decode_fn(BareTok()) is None


def test_safe_decode_swallows_exception():
    def boom(token, idx):
        raise ValueError("x")

    assert _safe_decode(boom, "t", 0) is None


def test_safe_decode_none_fn_returns_none():
    assert _safe_decode(None, "t", 0) is None
