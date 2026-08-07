"""config 单元测试：PluginConfig.from_env 校验 + resolve_config_path（spec §2.11 §2.12）。

重构后路径解析仅返回 config.yaml 单路径（mtype_config / token2category fallback 已移除）。
"""
from __future__ import annotations

import os

import pytest

from vllm_anomaly_middleware.config import PluginConfig, resolve_config_path


def test_config_defaults(monkeypatch):
    for k in list(os.environ):
        if k.startswith("VLLM_ANOMALY"):
            monkeypatch.delenv(k, raising=False)
    c = PluginConfig.from_env()
    assert c.enabled is True
    assert c.top_logprobs == 20
    assert c.metrics_path == "/anomaly/metrics"
    assert c.sample_rate == 1.0
    assert c.detector_workers == 1


def test_config_env_override(monkeypatch):
    monkeypatch.setenv("VLLM_ANOMALY_ENABLED", "0")
    monkeypatch.setenv("VLLM_ANOMALY_TOP_LOGPROBS", "5")
    monkeypatch.setenv("VLLM_ANOMALY_SAMPLE_RATE", "0.3")
    monkeypatch.setenv("VLLM_ANOMALY_METRICS_PATH", "/x/m")
    monkeypatch.setenv("VLLM_ANOMALY_DETECTOR_WORKERS", "2")
    c = PluginConfig.from_env()
    assert c.enabled is False
    assert c.top_logprobs == 5
    assert c.sample_rate == 0.3
    assert c.metrics_path == "/x/m"
    assert c.detector_workers == 2


def test_config_invalid_top_logprobs(monkeypatch):
    monkeypatch.setenv("VLLM_ANOMALY_TOP_LOGPROBS", "0")
    with pytest.raises(ValueError):
        PluginConfig.from_env()


def test_config_invalid_top_logprobs_high(monkeypatch):
    monkeypatch.setenv("VLLM_ANOMALY_TOP_LOGPROBS", "21")
    with pytest.raises(ValueError):
        PluginConfig.from_env()


def test_config_invalid_sample_rate(monkeypatch):
    monkeypatch.setenv("VLLM_ANOMALY_SAMPLE_RATE", "1.5")
    with pytest.raises(ValueError):
        PluginConfig.from_env()


def test_resolve_config_path_vendored_default():
    c = PluginConfig()
    path = resolve_config_path(c)
    assert path is not None
    assert os.path.isfile(path) and path.endswith("detector.yaml")


def test_resolve_config_path_explicit_override(tmp_path):
    cfg = tmp_path / "c.yaml"
    cfg.write_text("window_size: 128\nstride: 64\n")
    c = PluginConfig(detector_config_path=str(cfg))
    path = resolve_config_path(c)
    assert path == str(cfg)


def test_resolve_config_path_explicit_missing_falls_to_vendored(tmp_path):
    # 显式路径不存在 → 回退 vendored
    c = PluginConfig(detector_config_path=str(tmp_path / "nope.yaml"))
    path = resolve_config_path(c)
    assert path is not None
    assert path.endswith("detector.yaml")  # vendored


def test_resolve_config_path_vendored_missing_returns_none(tmp_path):
    # base_dir 指向空目录（无 vendored）→ None（降级）
    c = PluginConfig()
    path = resolve_config_path(c, base_dir=str(tmp_path))
    assert path is None


# --------------------------- tokenizer_model --------------------------- #
def test_tokenizer_model_default_none(monkeypatch):
    monkeypatch.delenv("VLLM_ANOMALY_TOKENIZER_MODEL", raising=False)
    cfg = PluginConfig.from_env()
    assert cfg.tokenizer_model is None


def test_tokenizer_model_env(monkeypatch):
    monkeypatch.setenv("VLLM_ANOMALY_TOKENIZER_MODEL", "/data/Qwen3.0.6B")
    cfg = PluginConfig.from_env()
    assert cfg.tokenizer_model == "/data/Qwen3.0.6B"
