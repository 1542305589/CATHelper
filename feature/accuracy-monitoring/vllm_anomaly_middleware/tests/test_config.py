"""config 单元测试：PluginConfig.from_env 校验 + resolve_detector_paths 三方式（spec §2.11 §2.12）。"""
from __future__ import annotations

import os

import pytest

from vllm_anomaly_middleware.config import PluginConfig, resolve_detector_paths


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


def test_resolve_paths_vendored_default():
    c = PluginConfig()
    paths = resolve_detector_paths(c)
    assert paths is not None
    cfg, mtype, tk2 = paths
    assert os.path.isfile(cfg) and cfg.endswith("config.yaml")
    assert os.path.isfile(mtype) and mtype.endswith("mtype_config.json")
    assert os.path.isdir(tk2) and tk2.endswith("token2category")


def test_resolve_paths_explicit_override(tmp_path):
    # 构造一份显式路径
    cfg = tmp_path / "c.yaml"
    cfg.write_text("window_size: 128\nstride: 64\n")
    mtype = tmp_path / "m.json"
    mtype.write_text("{}")
    tk2 = tmp_path / "tk"
    tk2.mkdir()
    c = PluginConfig(
        detector_config_path=str(cfg),
        mtype_config_path=str(mtype),
        tk2cat_path=str(tk2),
    )
    paths = resolve_detector_paths(c)
    assert paths == (str(cfg), str(mtype), str(tk2))


def test_resolve_paths_explicit_missing_falls_to_vendored(tmp_path):
    # 显式路径不存在 → 回退 vendored
    c = PluginConfig(
        detector_config_path=str(tmp_path / "nope.yaml"),
        mtype_config_path=str(tmp_path / "nope.json"),
        tk2cat_path=str(tmp_path / "notdir"),
    )
    paths = resolve_detector_paths(c)
    assert paths is not None
    assert paths[0].endswith("config.yaml")  # vendored


def test_resolve_paths_vendored_missing_no_external_returns_none(tmp_path, monkeypatch):
    # base_dir 指向空目录（无 vendored）且模拟无 external → None（降级）
    import vllm_anomaly_middleware.config as cfgmod

    monkeypatch.setattr(cfgmod, "_external_paths", lambda: None)
    c = PluginConfig()
    paths = resolve_detector_paths(c, base_dir=str(tmp_path))
    assert paths is None
