"""token_resolver 单测：resolve 行为 + acquire_tokenizer（from_pretrained 主、/v1/models 兜底）。"""
from __future__ import annotations

import json

import pytest

from vllm_anomaly_middleware.token_resolver import TokenTextResolver, acquire_tokenizer


class FakeTok:
    """模拟 HF tokenizer：id -> text 字典；可注入异常。"""

    def __init__(self, mapping, raise_ids=None):
        self._m = mapping
        self._raise = set(raise_ids or [])

    def decode(self, ids, **kwargs):
        out = []
        for i in ids:
            if i in self._raise:
                raise ValueError("boom")
            out.append(self._m.get(i, ""))
        return "".join(out)


# --------------------------- resolve --------------------------- #
def test_resolve_returns_text_and_caches():
    tok = FakeTok({100: "你", 200: "好"})
    r = TokenTextResolver(tok)
    assert r.resolve(100) == "你"
    assert r.resolve(200) == "好"
    assert r.resolve(100) == "你"  # 命中缓存


def test_resolve_unknown_id_returns_none():
    tok = FakeTok({})  # 无映射 → decode 返回 "" → 视为无文本
    r = TokenTextResolver(tok)
    assert r.resolve(999) is None


def test_resolve_decode_raises_returns_none():
    tok = FakeTok({100: "x"}, raise_ids=[100])
    r = TokenTextResolver(tok)
    assert r.resolve(100) is None  # 异常被吞 → None


def test_resolve_none_id_returns_none():
    r = TokenTextResolver(FakeTok({}))
    assert r.resolve(None) is None  # type: ignore[arg-type]


# --------------------------- acquire_tokenizer --------------------------- #
async def test_acquire_tokenizer_from_pretrained_local(tmp_path, monkeypatch):
    """用本地 tokenizer 目录命中 from_pretrained（local_files_only）。"""
    fake_dir = tmp_path / "tok"
    fake_dir.mkdir()
    (fake_dir / "tokenizer_config.json").write_text(json.dumps({"model_type": "fake"}))
    captured = {}

    import vllm_anomaly_middleware.token_resolver as tr

    def fake_from_pretrained(path, **kwargs):
        captured["path"] = path
        captured["kwargs"] = kwargs
        return FakeTok({1: "a"})

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    tok = await acquire_tokenizer(str(fake_dir), server=None)
    assert isinstance(tok, FakeTok)
    assert captured["kwargs"].get("local_files_only") is True


async def test_acquire_tokenizer_models_fallback(monkeypatch):
    """model_hint 解析失败 → /v1/models 兜底取 served id。"""
    import vllm_anomaly_middleware.token_resolver as tr

    seq = {"n": 0}

    def fake_from_pretrained(path, **kwargs):
        seq["n"] += 1
        if seq["n"] == 1:
            raise FileNotFoundError("nope")  # model_hint 失败
        assert path == "served-id"  # 第二次用 served id
        return FakeTok({2: "b"})

    async def fake_fetch_models(server):
        assert server == ("127.0.0.1", 8000)
        return "served-id"

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    monkeypatch.setattr(tr, "_fetch_served_model_id", fake_fetch_models)

    tok = await acquire_tokenizer("my-alias", server=("127.0.0.1", 8000))
    assert isinstance(tok, FakeTok)


async def test_acquire_tokenizer_all_fail_returns_none(monkeypatch):
    import vllm_anomaly_middleware.token_resolver as tr

    def boom(_p, **_k):
        raise FileNotFoundError("nope")

    async def no_server(_s):
        return None

    monkeypatch.setattr(tr, "_from_pretrained", boom)
    monkeypatch.setattr(tr, "_fetch_served_model_id", no_server)
    assert await acquire_tokenizer("x", server=("127.0.0.1", 8000)) is None


async def test_acquire_tokenizer_models_unreachable_returns_none(monkeypatch):
    import vllm_anomaly_middleware.token_resolver as tr

    def boom(_p, **_k):
        raise FileNotFoundError("nope")

    async def raises(_s):
        raise RuntimeError("loopback fail")

    monkeypatch.setattr(tr, "_from_pretrained", boom)
    monkeypatch.setattr(tr, "_fetch_served_model_id", raises)
    assert await acquire_tokenizer("x", server=("127.0.0.1", 8000)) is None


async def test_acquire_tokenizer_no_server_no_hint_returns_none(monkeypatch):
    """无 model_hint 且无 server → 直接 None。"""
    import vllm_anomaly_middleware.token_resolver as tr

    def boom(_p, **_k):
        raise FileNotFoundError("nope")

    monkeypatch.setattr(tr, "_from_pretrained", boom)
    assert await acquire_tokenizer("", server=None) is None


async def test_acquire_tokenizer_cache_scan_fallback(monkeypatch):
    """裸 served 名 from_pretrained 失败 → HF 缓存扫描补全完整 repo id → 命中。

    复现线上：vLLM --model Qwen3-0.6B（裸名），HF 缓存键为 Qwen/Qwen3-0.6B。
    """
    import vllm_anomaly_middleware.token_resolver as tr

    calls = []

    def fake_from_pretrained(path, **kwargs):
        calls.append(path)
        if path == "Qwen3-0.6B":
            raise FileNotFoundError("not in cache under bare name")
        assert path == "Qwen/Qwen3-0.6B"
        return FakeTok({151667: "你好"})

    async def fake_fetch_models(server):
        return "Qwen3-0.6B"  # loopback 返回裸 served 名

    def fake_scan(hint):
        return ["Qwen/Qwen3-0.6B"] if hint == "Qwen3-0.6B" else []

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    monkeypatch.setattr(tr, "_fetch_served_model_id", fake_fetch_models)
    monkeypatch.setattr(tr, "_scan_hf_cache_candidates", fake_scan, raising=False)

    tok = await acquire_tokenizer("Qwen3-0.6B", server=("127.0.0.1", 8000))
    assert isinstance(tok, FakeTok)
    assert "Qwen3-0.6B" in calls  # 先试裸名（失败）
    assert "Qwen/Qwen3-0.6B" in calls  # 缓存扫描补全后命中


async def test_acquire_tokenizer_explicit_first(monkeypatch):
    """显式 tokenizer_model(env) 最高优先，直接命中，不碰 model_hint/loopback。"""
    import vllm_anomaly_middleware.token_resolver as tr

    calls = []

    def fake_from_pretrained(path, **kwargs):
        calls.append(path)
        if path == "/data/Qwen3-0.6B":
            return FakeTok({1: "x"})
        raise FileNotFoundError("nope")

    async def no_fetch(server):
        return None

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    monkeypatch.setattr(tr, "_fetch_served_model_id", no_fetch)
    monkeypatch.setattr(tr, "_scan_hf_cache_candidates", lambda hint: [], raising=False)

    tok = await acquire_tokenizer(
        "Qwen3-0.6B", server=None, explicit="/data/Qwen3-0.6B"
    )
    assert isinstance(tok, FakeTok)
    assert calls == ["/data/Qwen3-0.6B"]  # 只调用了 explicit，未碰 model_hint

# --------------------------- trust_remote_code (Task 2) --------------------------- #
async def test_from_pretrained_sets_trust_remote_code(monkeypatch):
    """_from_pretrained 默认补 trust_remote_code=True（Qwen/GLM 自定义 tokenizer）。"""
    import sys
    import types
    import vllm_anomaly_middleware.token_resolver as tr

    captured = {}

    class FakeAutoTokenizer:
        @staticmethod
        def from_pretrained(path, **kwargs):
            captured.update(kwargs)
            return FakeTok({1: "x"})

    fake_mod = types.ModuleType("transformers")
    fake_mod.AutoTokenizer = FakeAutoTokenizer
    monkeypatch.setitem(sys.modules, "transformers", fake_mod)

    tok = tr._from_pretrained("/data/Qwen3", local_files_only=True)
    assert isinstance(tok, FakeTok)
    assert captured.get("trust_remote_code") is True
    assert captured.get("local_files_only") is True


async def test_from_pretrained_respects_explicit_trust_remote_code(monkeypatch):
    """调用方显式传 False 时不覆盖。"""
    import sys
    import types
    import vllm_anomaly_middleware.token_resolver as tr

    captured = {}

    class FakeAutoTokenizer:
        @staticmethod
        def from_pretrained(path, **kwargs):
            captured.update(kwargs)
            return FakeTok({1: "x"})

    fake_mod = types.ModuleType("transformers")
    fake_mod.AutoTokenizer = FakeAutoTokenizer
    monkeypatch.setitem(sys.modules, "transformers", fake_mod)

    tr._from_pretrained("/data/m", trust_remote_code=False, local_files_only=True)
    assert captured.get("trust_remote_code") is False
