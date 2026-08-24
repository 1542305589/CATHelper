"""AnomalyMiddleware 构造期 eager 初始化测试（spec §2.13 §2.15 §4.4 §4.6）。

重构后 __init__ 同步加载 tokenizer + 生成 tk2cat + 构造 ILLDetector + DetectorRunner
（无预热线程、无运行期 lazy _ensure_resolver/_ensure_runner）：
- tokenizer 加载失败 -> fail-fast（raise，spec §2.15）。
- tk2cat 生成失败 -> 软降级（_tk2cat=None，服务仍启动，spec §2.13）。
- runner 构造期注入 topk_n + tk2cat。
"""
from __future__ import annotations

import pytest

from _helpers import FakeVLLM
from anomaly_middleware import AnomalyMiddleware
from anomaly_middleware.env import PluginConfig


class _FakeTok:
    """伪 tokenizer：get_vocab / vocab_size / decode。"""

    def __init__(self, vocab):
        self._vocab = dict(vocab)
        self.vocab_size = max(vocab.values()) + 1

    def get_vocab(self):
        return self._vocab

    def decode(self, ids):
        inv = {v: k for k, v in self._vocab.items()}
        return "".join(inv.get(i, "") for i in ids)


@pytest.fixture
def fake_tok():
    return _FakeTok({"你": 0, "好": 1, " ": 2})


@pytest.fixture
def fake_mapping():
    return ({"0": "chinese_cjk", "1": "chinese_cjk", "2": "whitespace"}, 3)


def _patch_ok(monkeypatch, fake_tok, fake_mapping):
    """打桩：acquire_tokenizer_sync 返回伪 tokenizer，generate_tk2cat 返回伪映射。"""
    import anomaly_middleware.token_resolver as tr
    import anomaly_middleware.token_categorizer as gmc
    monkeypatch.setattr(tr, "acquire_tokenizer_sync", lambda explicit=None: fake_tok)
    monkeypatch.setattr(gmc, "generate_tk2cat", lambda tok: fake_mapping)
    monkeypatch.delenv("VLLM_ANOMALY_ENABLED", raising=False)
    monkeypatch.delenv("VLLM_ANOMALY_SAVE_PATH", raising=False)
    monkeypatch.delenv("VLLM_ANOMALY_TOKENIZER_MODEL", raising=False)


def _app():
    return FakeVLLM(lambda scope, body: ("json", {}))


# --------------------------- 构造期同步加载 --------------------------- #
def test_init_loads_resolver_tk2cat_runner(monkeypatch, fake_tok, fake_mapping):
    """构造期同步加载 tokenizer + tk2cat + 构造 runner（spec §2.15 §4.4 §4.6）。"""
    _patch_ok(monkeypatch, fake_tok, fake_mapping)
    mw = AnomalyMiddleware(_app())
    assert mw._resolver is not None
    assert mw._tk2cat == fake_mapping[0]
    assert mw._vocab_size == fake_mapping[1]
    assert mw._runner is not None
    assert mw._runner._topk_n == mw.config.top_logprobs  # topk_n 注入
    assert mw._runner._tk2cat == fake_mapping[0]  # tk2cat 注入
    assert mw._runner._vocab_size == fake_mapping[1]
    mw.shutdown()


def test_init_tk2cat_failure_soft_degrades(monkeypatch, fake_tok, fake_mapping):
    """tk2cat 生成失败 -> 软降级（_tk2cat=None），服务仍启动（spec §2.13）。"""
    import anomaly_middleware.token_resolver as tr
    import anomaly_middleware.token_categorizer as gmc
    monkeypatch.setattr(tr, "acquire_tokenizer_sync", lambda explicit=None: fake_tok)
    monkeypatch.setattr(gmc, "generate_tk2cat", lambda tok: (_ for _ in ()).throw(
        RuntimeError("no decode path")))
    monkeypatch.delenv("VLLM_ANOMALY_ENABLED", raising=False)
    monkeypatch.delenv("VLLM_ANOMALY_SAVE_PATH", raising=False)
    monkeypatch.delenv("VLLM_ANOMALY_TOKENIZER_MODEL", raising=False)
    mw = AnomalyMiddleware(_app())
    assert mw._resolver is not None  # tokenizer 加载成功
    assert mw._tk2cat is None  # 映射失败 -> None（降级）
    assert mw._vocab_size is None
    assert mw._runner is not None  # 服务仍启动
    mw.shutdown()


def test_init_tokenizer_failure_raises(monkeypatch):
    """tokenizer 加载失败 -> fail-fast（raise，spec §2.15）。"""
    import anomaly_middleware.token_resolver as tr

    def _raise(explicit=None):
        raise RuntimeError("no tokenizer")

    monkeypatch.setattr(tr, "acquire_tokenizer_sync", _raise)
    monkeypatch.delenv("VLLM_ANOMALY_ENABLED", raising=False)
    monkeypatch.delenv("VLLM_ANOMALY_SAVE_PATH", raising=False)
    with pytest.raises(RuntimeError):
        AnomalyMiddleware(_app())
