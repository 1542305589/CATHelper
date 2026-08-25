"""config / ConfigManager 单元测试：校验、热重载、diff、mtime、原子写盘。"""
from __future__ import annotations

import os
import time

import pytest

from webui.config import (
    AlertRule,
    ConfigError,
    ConfigManager,
    InstanceConfig,
    WebUIConfig,
)
from tests._webui_helpers import build_webui_config_dict, write_yaml


def _file(tmp_path, **kwargs) -> str:
    path = str(tmp_path / "webui.yaml")
    write_yaml(path, build_webui_config_dict(**kwargs))
    return path


def _cfg(tmp_path, **kwargs) -> WebUIConfig:
    return WebUIConfig.from_yaml(_file(tmp_path, **kwargs))


# ------------------------------------------------------------------ #
# 校验
# ------------------------------------------------------------------ #
def test_load_valid(tmp_path):
    cfg = _cfg(
        tmp_path,
        instances=[{"name": "a", "url": "http://10.0.0.1:8000"}],
        alerts=[{"name": "r", "ill_type": "garbled", "threshold": 3}],
    )
    assert cfg.auth.username == "admin"
    assert cfg.auth.verify("admin", "test123")
    assert len(cfg.instances) == 1
    assert cfg.instances[0].url == "http://10.0.0.1:8000"
    assert len(cfg.alerts) == 1


def test_missing_auth_rejected(tmp_path):
    data = build_webui_config_dict()
    data["auth"] = {"username": "admin"}  # 无 password / password_hash
    with pytest.raises(ConfigError):
        WebUIConfig.from_dict(data)


def test_invalid_password_hash_rejected(tmp_path):
    data = build_webui_config_dict(auth={"username": "a", "password_hash": "not-hex"})
    with pytest.raises(ConfigError):
        WebUIConfig.from_dict(data)


def test_duplicate_instance_names_rejected(tmp_path):
    data = build_webui_config_dict(
        instances=[
            {"name": "a", "url": "http://x:1"},
            {"name": "a", "url": "http://y:2"},
        ]
    )
    with pytest.raises(ConfigError, match="实例名冲突"):
        WebUIConfig.from_dict(data)


def test_empty_instance_name_rejected(tmp_path):
    with pytest.raises(ConfigError):
        InstanceConfig.from_dict({"name": "  ", "url": "http://x:1"}, "instances")


def test_bad_url_rejected(tmp_path):
    with pytest.raises(ConfigError):
        InstanceConfig.from_dict({"name": "a", "url": "not-a-url"}, "instances")


def test_unknown_ill_type_rejected():
    with pytest.raises(ConfigError):
        AlertRule.from_dict({"name": "r", "ill_type": "bogus"}, "alerts")


def test_bad_threshold_rejected():
    with pytest.raises(ConfigError):
        AlertRule.from_dict({"name": "r", "threshold": 0}, "alerts")


def test_duplicate_rule_names_rejected(tmp_path):
    data = build_webui_config_dict(
        alerts=[{"name": "r"}, {"name": "r"}]
    )
    with pytest.raises(ConfigError, match="规则名冲突"):
        WebUIConfig.from_dict(data)


def test_store_validation(tmp_path):
    data = build_webui_config_dict()
    data["store"]["raw_trend_window_seconds"] = 100
    data["store"]["trend_horizon_seconds"] = 50
    with pytest.raises(ConfigError):
        WebUIConfig.from_dict(data)
    data["store"] = {
        "event_capacity": 10,
        "alert_capacity": 10,
        "raw_trend_window_seconds": 3600,
        "trend_bucket_seconds": 7200,  # bucket > raw
        "trend_horizon_seconds": 86400,
    }
    with pytest.raises(ConfigError):
        WebUIConfig.from_dict(data)


def test_poll_interval_bounds(tmp_path):
    data = build_webui_config_dict()
    data["poll"] = {"interval_seconds": 10}
    with pytest.raises(ConfigError):
        WebUIConfig.from_dict(data)


def test_rule_match_semantics():
    r = AlertRule(name="r", instance="*", model="*", ill_type="garbled")
    assert r.matches("a", "m", "garbled")
    assert not r.matches("a", "m", "rare_character")
    r2 = AlertRule(name="r2", instance="a", model="m1", ill_type=None)
    assert r2.matches("a", "m1", "nan_value")
    assert not r2.matches("a", "m2", "nan_value")
    assert not r2.matches("b", "m1", "nan_value")


# ------------------------------------------------------------------ #
# ConfigManager：加载 / 热重载 / mtime / 写盘
# ------------------------------------------------------------------ #
def test_load_and_unchanged_no_reload(tmp_path):
    path = _file(tmp_path)
    cm = ConfigManager(path)
    cfg = cm.load()
    assert cfg is cm.last_config
    assert cm.reload_if_changed() is None  # mtime 未变


def test_reload_detects_mtime_change(tmp_path):
    path = _file(tmp_path)
    cm = ConfigManager(path)
    cm.load()
    time.sleep(0.01)
    write_yaml(path, build_webui_config_dict(instances=[{"name": "x", "url": "http://x:1"}]))
    os.utime(path, None)
    new = cm.reload_if_changed()
    assert new is not None
    assert [i.name for i in new.instances] == ["x"]


def test_reload_invalid_keeps_old_config(tmp_path):
    path = _file(tmp_path, instances=[{"name": "a", "url": "http://x:1"}])
    cm = ConfigManager(path)
    cm.load()
    # 写入非法 yaml
    with open(path, "w", encoding="utf-8") as f:
        f.write("server: [broken\n")
    os.utime(path, None)
    assert cm.reload_if_changed() is None
    # 上次有效配置保留
    assert cm.last_config is not None
    assert [i.name for i in cm.last_config.instances] == ["a"]


def test_reload_unknown_ill_type_rejected_keeps_old(tmp_path):
    path = _file(tmp_path, instances=[{"name": "a", "url": "http://x:1"}])
    cm = ConfigManager(path)
    cm.load()
    time.sleep(0.01)
    data = build_webui_config_dict(
        instances=[{"name": "a", "url": "http://x:1"}],
        alerts=[{"name": "r", "ill_type": "garbage"}],
    )
    write_yaml(path, data)
    os.utime(path, None)
    assert cm.reload_if_changed() is None
    assert cm.last_config.alerts == ()


def test_write_instances_atomic_and_reloadable(tmp_path):
    path = _file(tmp_path, instances=[{"name": "old", "url": "http://x:1"}])
    cm = ConfigManager(path)
    cm.load()
    new_list = [
        InstanceConfig(name="a", url="http://10.0.0.1:8000"),
        InstanceConfig(name="b", url="http://10.0.0.2:8000", paused=True),
    ]
    cm.write_instances(new_list)
    cfg = cm.reload_now()
    assert [i.name for i in cfg.instances] == ["a", "b"]
    assert cfg.instances[1].paused is True
    # 文件实际内容已更新
    import yaml

    with open(path, encoding="utf-8") as f:
        data = yaml.safe_load(f)
    assert [i["name"] for i in data["instances"]] == ["a", "b"]
    assert data["instances"][1].get("paused") is True


def test_apply_same_config_idempotent(tmp_path):
    path = _file(tmp_path, instances=[{"name": "a", "url": "http://x:1"}])
    cm = ConfigManager(path)
    cm.load()
    new = cm.reload_now()
    assert new is not None
    assert cm.last_config.instances == new.instances
    # 重复 reload_now 是等价的（last_config 更新为相同快照）
    same = cm.reload_now()
    assert same.instances == new.instances


def test_auth_config_verify_plain_vs_hash():
    from webui.config import AuthConfig

    plain = AuthConfig(username="admin", secret="s3cret")
    assert plain.verify("admin", "s3cret")
    assert not plain.verify("admin", "wrong")
    assert not plain.verify("other", "s3cret")

    import hashlib

    h = hashlib.sha256(b"s3cret").hexdigest()
    hashed = AuthConfig(username="admin", password_hash=h)
    assert hashed.verify("admin", "s3cret")
    assert not hashed.verify("admin", "wrong")


def test_canonical_family_matching_helpers():
    from webui.collector import _canonical_family_name, _family_matches

    assert _canonical_family_name("vllm_anomaly_requests_total") == "vllm_anomaly_requests"
    assert _canonical_family_name("vllm_anomaly_last_garbled") == "vllm_anomaly_last_garbled"
    assert _family_matches("vllm_anomaly_requests", "vllm_anomaly_requests_total")
    assert _family_matches("vllm_anomaly_requests_total", "vllm_anomaly_requests_total")
    assert not _family_matches("vllm_anomaly_requests_created", "vllm_anomaly_requests_total")