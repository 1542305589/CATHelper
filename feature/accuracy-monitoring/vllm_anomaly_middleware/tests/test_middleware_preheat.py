"""预热 / _ensure_resolver 生成映射 / 竞态补调 set_vocabulary（spec §4.5 §4.6）。"""
from __future__ import annotations

import pytest

from _helpers import FakeVLLM
from vllm_anomaly_middleware import AnomalyMiddleware
from vllm_anomaly_middleware.config import PluginConfig


class _FakeTok:
    """伪 tokenizer：get_vocab / vocab_size / decode（无 backend -> 走高层 decode）。"""

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


# --------------------------- 预热 --------------------------- #
def test_preheat_sets_tk2cat_when_tokenizer_model_set(monkeypatch, fake_tok, fake_mapping):
    """config.tokenizer_model 已设 -> __init__ 启动预热线程 -> _tk2cat/_vocab_size 就绪。"""
    import vllm_anomaly_middleware.token_resolver as tr
    import vllm_anomaly_middleware.token_categorizer as gmc

    monkeypatch.setattr(tr, "_from_pretrained", lambda path, **k: fake_tok)
    monkeypatch.setattr(gmc, "generate_tk2cat", lambda tok: fake_mapping)

    mw = AnomalyMiddleware(FakeVLLM(lambda scope, body: ("json", {})))
    mw.config = PluginConfig(tokenizer_model="/data/Qwen3", top_logprobs=20)
    mw._start_preheat()  # __init__ 已据 env（未设）跳过；显式触发
    mw._preheat_thread.join(timeout=5)

    assert mw._resolver_inited is True
    assert mw._tk2cat == fake_mapping[0]
    assert mw._vocab_size == fake_mapping[1]
    mw.shutdown()


def test_preheat_skipped_when_no_tokenizer_model(monkeypatch):
    import vllm_anomaly_middleware.token_resolver as tr

    called = {"n": 0}

    def boom(path, **k):
        called["n"] += 1
        raise FileNotFoundError

    monkeypatch.setattr(tr, "_from_pretrained", boom)
    mw = AnomalyMiddleware(FakeVLLM(lambda scope, body: ("json", {})))
    mw.config = PluginConfig(top_logprobs=20)  # 无 tokenizer_model
    # 不应启动预热线程 / 不应加载 tokenizer
    assert getattr(mw, "_preheat_thread", None) is None or mw._preheat_thread is None
    assert called["n"] == 0
    mw.shutdown()


def test_preheat_tk2cat_failure_does_not_break_resolver(monkeypatch, fake_tok):
    """tk2cat 生成失败 -> resolver 仍就绪，检测降级无词表。"""
    import vllm_anomaly_middleware.token_resolver as tr
    import vllm_anomaly_middleware.token_categorizer as gmc

    def _raise_generate(tok):
        raise RuntimeError("no decode path")

    monkeypatch.setattr(tr, "_from_pretrained", lambda path, **k: fake_tok)
    monkeypatch.setattr(gmc, "generate_tk2cat", _raise_generate)
    mw = AnomalyMiddleware(FakeVLLM(lambda scope, body: ("json", {})))
    mw.config = PluginConfig(tokenizer_model="/data/m", top_logprobs=20)
    mw._start_preheat()
    mw._preheat_thread.join(timeout=5)
    assert mw._resolver_inited is True  # tokenizer 加载成功 -> resolver 就绪
    assert mw._tk2cat is None  # 映射失败 -> None（降级）
    mw.shutdown()


# --------------------------- _ensure_resolver 慢路径 --------------------------- #
async def test_ensure_resolver_slow_path_generates_tk2cat(monkeypatch, fake_tok, fake_mapping):
    """未预热 -> _ensure_resolver 慢路径：acquire_tokenizer + generate_tk2cat。"""
    import vllm_anomaly_middleware.token_resolver as tr
    import vllm_anomaly_middleware.token_categorizer as gmc

    async def _no_fetch(server):
        return None

    monkeypatch.setattr(tr, "_from_pretrained", lambda path, **k: fake_tok)
    monkeypatch.setattr(gmc, "generate_tk2cat", lambda tok: fake_mapping)
    monkeypatch.setattr(tr, "_fetch_served_model_id", _no_fetch)

    mw = AnomalyMiddleware(FakeVLLM(lambda scope, body: ("json", {})))
    mw.config = PluginConfig(tokenizer_model="/data/m", top_logprobs=20)
    mw._runner = None
    mw._runner_inited = False
    mw._resolver_inited = False

    await mw._ensure_resolver("hint", None)
    assert mw._tk2cat == fake_mapping[0]
    assert mw._vocab_size == fake_mapping[1]
    assert mw._resolver_inited is True
    mw.shutdown()


async def test_ensure_runner_injects_topk_n_and_vocabulary(monkeypatch, fake_tok, fake_mapping):
    """_ensure_runner 传 topk_n + 预热完成 -> 构造后立即 set_vocabulary。"""
    import vllm_anomaly_middleware.token_resolver as tr
    import vllm_anomaly_middleware.token_categorizer as gmc

    monkeypatch.setattr(tr, "_from_pretrained", lambda path, **k: fake_tok)
    monkeypatch.setattr(gmc, "generate_tk2cat", lambda tok: fake_mapping)

    mw = AnomalyMiddleware(FakeVLLM(lambda scope, body: ("json", {})))
    mw.config = PluginConfig(tokenizer_model="/data/m", top_logprobs=7)
    # 先预热（填充 _tk2cat）
    mw._start_preheat()
    mw._preheat_thread.join(timeout=5)
    # 再 _ensure_runner
    assert mw._ensure_runner() is True
    assert mw._runner._topk_n == 7
    assert mw._runner._tk2cat == fake_mapping[0]  # 已注入
    mw.shutdown()


async def test_ensure_resolver_fast_path_backfills_set_vocabulary(monkeypatch, fake_tok, fake_mapping):
    """竞态：预热在 _ensure_runner 之后完成 -> _ensure_resolver 快路径补调 set_vocabulary。"""
    import vllm_anomaly_middleware.token_resolver as tr
    import vllm_anomaly_middleware.token_categorizer as gmc

    monkeypatch.setattr(tr, "_from_pretrained", lambda path, **k: fake_tok)
    monkeypatch.setattr(gmc, "generate_tk2cat", lambda tok: fake_mapping)

    mw = AnomalyMiddleware(FakeVLLM(lambda scope, body: ("json", {})))
    mw.config = PluginConfig(tokenizer_model="/data/m", top_logprobs=20)
    # 模拟：runner 先构造（_tk2cat 当时为 None，跳过注入）
    mw._ensure_runner()
    assert mw._runner._tk2cat is None  # 构造时无映射
    # 预热完成（填 _tk2cat + _resolver_inited=True）
    mw._start_preheat()
    mw._preheat_thread.join(timeout=5)
    assert mw._tk2cat is not None
    # 快路径补调
    await mw._ensure_resolver("hint", None)
    assert mw._runner._tk2cat == fake_mapping[0]
    mw.shutdown()
