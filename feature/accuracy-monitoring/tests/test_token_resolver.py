"""token_resolver 单测：resolve 行为 + acquire_tokenizer_sync（env/argv/缓存扫描，fail-fast）。"""
from __future__ import annotations

import json

import pytest

from anomaly_middleware.token_resolver import (
    TokenTextResolver,
    acquire_tokenizer_sync,
    parse_vllm_argv,
)


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


# --------------------------- acquire_tokenizer_sync --------------------------- #
def test_acquire_tokenizer_sync_explicit_local(tmp_path, monkeypatch):
    """explicit(env) 本地目录命中 from_pretrained（local_files_only）。"""
    fake_dir = tmp_path / "tok"
    fake_dir.mkdir()
    (fake_dir / "tokenizer_config.json").write_text(json.dumps({"model_type": "fake"}))
    captured = {}

    import anomaly_middleware.token_resolver as tr

    def fake_from_pretrained(path, **kwargs):
        captured["path"] = path
        captured["kwargs"] = kwargs
        return FakeTok({1: "a"})

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    monkeypatch.setattr(tr, "parse_vllm_argv", lambda: None)
    tok = tr.acquire_tokenizer_sync(explicit=str(fake_dir))
    assert isinstance(tok, FakeTok)
    assert captured["kwargs"].get("local_files_only") is True


def test_acquire_tokenizer_sync_explicit_first(monkeypatch):
    """explicit(env) 最高优先，直接命中，不碰 argv。"""
    import anomaly_middleware.token_resolver as tr

    calls = []

    def fake_from_pretrained(path, **kwargs):
        calls.append(path)
        if path == "/data/Qwen3-0.6B":
            return FakeTok({1: "x"})
        raise FileNotFoundError("nope")

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    # argv 有值但不应被触碰（explicit 先命中）
    monkeypatch.setattr(
        tr, "parse_vllm_argv",
        lambda: tr.VllmArgvInfo(model="/argv/model", tokenizer="/argv/tok"),
    )
    monkeypatch.setattr(tr, "_scan_hf_cache_candidates", lambda hint: [], raising=False)

    tok = tr.acquire_tokenizer_sync(explicit="/data/Qwen3-0.6B")
    assert isinstance(tok, FakeTok)
    assert calls == ["/data/Qwen3-0.6B"]  # 只调用了 explicit


def test_acquire_tokenizer_sync_argv_tokenizer_preferred(monkeypatch):
    """--tokenizer(argv) 在 --model(argv) 之前尝试。"""
    import anomaly_middleware.token_resolver as tr

    calls = []

    def fake_from_pretrained(path, **kwargs):
        calls.append(path)
        if path == "/argv/tok":
            return FakeTok({1: "x"})
        raise FileNotFoundError("nope")

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    monkeypatch.setattr(
        tr, "parse_vllm_argv",
        lambda: tr.VllmArgvInfo(model="/argv/model", tokenizer="/argv/tok"),
    )
    monkeypatch.setattr(tr, "_scan_hf_cache_candidates", lambda hint: [], raising=False)

    tok = tr.acquire_tokenizer_sync()
    assert isinstance(tok, FakeTok)
    assert calls[0] == "/argv/tok"  # tokenizer(argv) 优先


def test_acquire_tokenizer_sync_argv_model_when_no_tokenizer(monkeypatch):
    """无 --tokenizer → --model(argv) 尝试。"""
    import anomaly_middleware.token_resolver as tr

    calls = []

    def fake_from_pretrained(path, **kwargs):
        calls.append(path)
        if path == "/argv/model":
            return FakeTok({1: "x"})
        raise FileNotFoundError("nope")

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    monkeypatch.setattr(
        tr, "parse_vllm_argv",
        lambda: tr.VllmArgvInfo(model="/argv/model", tokenizer=None),
    )
    monkeypatch.setattr(tr, "_scan_hf_cache_candidates", lambda hint: [], raising=False)

    tok = tr.acquire_tokenizer_sync()
    assert isinstance(tok, FakeTok)
    assert calls[0] == "/argv/model"  # model(argv)


def test_acquire_tokenizer_sync_cache_scan_fallback(monkeypatch):
    """裸名 from_pretrained 失败 → HF 缓存扫描补全 repo id → 命中。

    复现线上：vLLM --model Qwen3-0.6B（裸名），HF 缓存键为 Qwen/Qwen3-0.6B。
    """
    import anomaly_middleware.token_resolver as tr

    calls = []

    def fake_from_pretrained(path, **kwargs):
        calls.append(path)
        if path == "Qwen3-0.6B":
            raise FileNotFoundError("not in cache under bare name")
        assert path == "Qwen/Qwen3-0.6B"
        return FakeTok({151667: "你好"})

    def fake_scan(hint):
        return ["Qwen/Qwen3-0.6B"] if hint == "Qwen3-0.6B" else []

    monkeypatch.setattr(tr, "_from_pretrained", fake_from_pretrained)
    monkeypatch.setattr(
        tr, "parse_vllm_argv",
        lambda: tr.VllmArgvInfo(model="Qwen3-0.6B", tokenizer=None),
    )
    monkeypatch.setattr(tr, "_scan_hf_cache_candidates", fake_scan, raising=False)

    tok = tr.acquire_tokenizer_sync()  # 无 explicit → argv model → cache scan
    assert isinstance(tok, FakeTok)
    assert "Qwen3-0.6B" in calls  # argv model 先试（失败）
    assert "Qwen/Qwen3-0.6B" in calls  # 缓存扫描补全后命中


def test_acquire_tokenizer_sync_all_fail_raises(monkeypatch):
    """全部失败 → raise RuntimeError（提示设置 VLLM_ANOMALY_TOKENIZER_MODEL）。"""
    import anomaly_middleware.token_resolver as tr

    def boom(_p, **_k):
        raise FileNotFoundError("nope")

    monkeypatch.setattr(tr, "_from_pretrained", boom)
    monkeypatch.setattr(tr, "parse_vllm_argv", lambda: None)
    monkeypatch.setattr(tr, "_scan_hf_cache_candidates", lambda hint: [], raising=False)

    with pytest.raises(RuntimeError, match="VLLM_ANOMALY_TOKENIZER_MODEL"):
        tr.acquire_tokenizer_sync(explicit="x")


def test_acquire_tokenizer_sync_no_candidates_raises(monkeypatch):
    """无 explicit 且无 argv → 无候选 → raise。"""
    import anomaly_middleware.token_resolver as tr

    monkeypatch.setattr(tr, "parse_vllm_argv", lambda: None)
    monkeypatch.setattr(tr, "_scan_hf_cache_candidates", lambda hint: [], raising=False)

    with pytest.raises(RuntimeError, match="VLLM_ANOMALY_TOKENIZER_MODEL"):
        tr.acquire_tokenizer_sync()


# --------------------------- trust_remote_code (Task 2) --------------------------- #
def test_from_pretrained_sets_trust_remote_code(monkeypatch):
    """_from_pretrained 默认补 trust_remote_code=True（Qwen/GLM 自定义 tokenizer）。"""
    import sys
    import types
    import anomaly_middleware.token_resolver as tr

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


def test_from_pretrained_respects_explicit_trust_remote_code(monkeypatch):
    """调用方显式传 False 时不覆盖。"""
    import sys
    import types
    import anomaly_middleware.token_resolver as tr

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


# --------------------------- 命令行发现（预热） --------------------------- #
def test_parse_vllm_argv_model_and_tokenizer():
    """`vllm serve <model> --tokenizer <path>` → 同时提取 model + tokenizer。"""
    argv = [
        "vllm", "serve", "/data/Qwen3-0.6B",
        "--tokenizer", "/data/tok",
        "--port", "8008", "--served-model-name", "Qwen3-0.6B",
        "--middleware", "anomaly_middleware.AnomalyMiddleware",
    ]
    info = parse_vllm_argv(argv)
    assert info is not None
    assert info.model == "/data/Qwen3-0.6B"
    assert info.tokenizer == "/data/tok"
    assert info.port == 8008


def test_parse_vllm_argv_model_only():
    """无 --tokenizer → model 即 tokenizer 路径。"""
    argv = ["vllm", "serve", "/data/Qwen3-0.6B", "--port", "8008"]
    info = parse_vllm_argv(argv)
    assert info is not None
    assert info.model == "/data/Qwen3-0.6B"
    assert info.tokenizer is None
    assert info.port == 8008


def test_parse_vllm_argv_tokenizer_eq_form():
    """--tokenizer=path 等号形式。"""
    argv = ["vllm", "serve", "/data/m", "--tokenizer=/data/tok", "--port=9000"]
    info = parse_vllm_argv(argv)
    assert info is not None
    assert info.model == "/data/m"
    assert info.tokenizer == "/data/tok"
    assert info.port == 9000


def test_parse_vllm_argv_host_eq_form():
    """--host=0.0.0.0 等号形式。"""
    argv = ["vllm", "serve", "/data/m", "--host=0.0.0.0", "--port=9000"]
    info = parse_vllm_argv(argv)
    assert info is not None
    assert info.host == "0.0.0.0"
    assert info.port == 9000


def test_parse_vllm_argv_non_serve_returns_none():
    assert parse_vllm_argv(["pytest", "foo", "bar"]) is None
    assert parse_vllm_argv(["pytest", "--port", "8008"]) is None


def test_parse_vllm_argv_value_flag_does_not_steal_model():
    """--served-model-name my-alias 中的 my-alias 不应被误识别为 model。"""
    argv = [
        "vllm", "serve", "/data/Qwen3-0.6B",
        "--served-model-name", "my-alias",
        "--port", "8008",
    ]
    info = parse_vllm_argv(argv)
    assert info is not None
    assert info.model == "/data/Qwen3-0.6B"  # 而非 "my-alias"
