"""配置与检测器路径解析（design §3.8 / spec §2.11 / §2.12）。

全部运行时配置来自环境变量；构造除 app 外无参数，故配置走 env。
路径解析：vendored 固定路径为主（用户明确路径固定），显式 env 覆盖可选，
external 导入兜底；均无则返回 None 触发降级。
"""
from __future__ import annotations

import importlib
import os
from dataclasses import dataclass
from typing import Optional, Tuple

METRICS_PATH_DEFAULT = "/anomaly/metrics"
TOP_LOGPROBS_DEFAULT = 20
SAMPLE_RATE_DEFAULT = 1.0
DETECTOR_WORKERS_DEFAULT = 1

_TRUE = {"1", "true", "yes", "on"}
_FALSE = {"0", "false", "no", "off"}


def _env_bool(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    if raw is None or raw.strip() == "":
        return default
    low = raw.strip().lower()
    if low in _TRUE:
        return True
    if low in _FALSE:
        return False
    return default


def _env_int(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if raw is None or raw.strip() == "":
        return default
    return int(raw.strip())


def _env_float(name: str, default: float) -> float:
    raw = os.environ.get(name)
    if raw is None or raw.strip() == "":
        return default
    return float(raw.strip())


def _env_str(name: str) -> Optional[str]:
    raw = os.environ.get(name)
    if raw is None or raw.strip() == "":
        return None
    return raw.strip()


@dataclass
class PluginConfig:
    """中间件运行配置（env 读取 + 校验）。不可变快照。"""

    enabled: bool = True
    top_logprobs: int = TOP_LOGPROBS_DEFAULT
    metrics_path: str = METRICS_PATH_DEFAULT
    sample_rate: float = SAMPLE_RATE_DEFAULT
    detector_workers: int = DETECTOR_WORKERS_DEFAULT
    detector_config_path: Optional[str] = None
    mtype_config_path: Optional[str] = None
    tk2cat_path: Optional[str] = None

    @classmethod
    def from_env(cls) -> "PluginConfig":
        top_logprobs = _env_int("VLLM_ANOMALY_TOP_LOGPROBS", TOP_LOGPROBS_DEFAULT)
        sample_rate = _env_float("VLLM_ANOMALY_SAMPLE_RATE", SAMPLE_RATE_DEFAULT)
        # 校验（§3.8）：top_logprobs∈[1,20]、sample_rate∈[0.0,1.0]
        if not isinstance(top_logprobs, int) or not (1 <= top_logprobs <= 20):
            raise ValueError(
                f"VLLM_ANOMALY_TOP_LOGPROBS 必须为 1-20 整数, 当前值: {top_logprobs}"
            )
        if not (0.0 <= sample_rate <= 1.0):
            raise ValueError(
                f"VLLM_ANOMALY_SAMPLE_RATE 必须为 0.0-1.0, 当前值: {sample_rate}"
            )
        workers = _env_int("VLLM_ANOMALY_DETECTOR_WORKERS", DETECTOR_WORKERS_DEFAULT)
        if workers < 1:
            workers = DETECTOR_WORKERS_DEFAULT
        return cls(
            enabled=_env_bool("VLLM_ANOMALY_ENABLED", True),
            top_logprobs=top_logprobs,
            metrics_path=_env_str("VLLM_ANOMALY_METRICS_PATH") or METRICS_PATH_DEFAULT,
            sample_rate=sample_rate,
            detector_workers=workers,
            detector_config_path=_env_str("VLLM_ANOMALY_DETECTOR_CONFIG_PATH"),
            mtype_config_path=_env_str("VLLM_ANOMALY_MTYPE_CONFIG_PATH"),
            tk2cat_path=_env_str("VLLM_ANOMALY_TK2CAT_PATH"),
        )


def _file_exists(path: str) -> bool:
    return os.path.isfile(path)


def _dir_exists(path: str) -> bool:
    return os.path.isdir(path)


def _vendored_paths(base_dir: str) -> Tuple[str, str, str]:
    """包内 response_anomaly 固定路径（design §2.2 / §3.8 tier 1）。"""
    ra = os.path.join(base_dir, "response_anomaly")
    return (
        os.path.join(ra, "configs", "config.yaml"),
        os.path.join(ra, "configs", "mtype_config.json"),
        os.path.join(ra, "token2category"),
    )


def _external_paths() -> Optional[Tuple[str, str, str]]:
    """外部可导入 response_anomaly / msprobe.response_anomaly（§3.8 tier 2 兜底）。"""
    for modname in ("response_anomaly", "msprobe.response_anomaly"):
        try:
            mod = importlib.import_module(modname)
        except Exception:
            continue
        # 跳过 vendored 自身（避免把 vendored 子包当 external）
        if mod.__file__ is None:
            continue
        d = os.path.dirname(os.path.abspath(mod.__file__))
        return (
            os.path.join(d, "configs", "config.yaml"),
            os.path.join(d, "configs", "mtype_config.json"),
            os.path.join(d, "token2category"),
        )
    return None


def resolve_detector_paths(
    config: PluginConfig, base_dir: Optional[str] = None
) -> Optional[Tuple[str, str, str]]:
    """解析检测后端三路径：config.yaml / mtype_config.json / token2category/。

    顺序：
      1. 显式 env 覆盖（三者齐备且存在）。
      2. vendored 固定路径（默认/主）。
      3. external 导入兜底。
      4. 都无 → None（触发降级）。
    """
    # 1. 显式覆盖
    if (
        config.detector_config_path
        and config.mtype_config_path
        and config.tk2cat_path
    ):
        if (
            _file_exists(config.detector_config_path)
            and _file_exists(config.mtype_config_path)
            and _dir_exists(config.tk2cat_path)
        ):
            return (
                config.detector_config_path,
                config.mtype_config_path,
                config.tk2cat_path,
            )

    # 2. vendored 固定路径
    if base_dir is None:
        base_dir = os.path.dirname(os.path.abspath(__file__))
    cfg, mtype, tk2 = _vendored_paths(base_dir)
    if _file_exists(cfg) and _file_exists(mtype) and _dir_exists(tk2):
        return (cfg, mtype, tk2)

    # 3. external 导入兜底
    ext = _external_paths()
    if ext is not None:
        cfg2, mtype2, tk22 = ext
        if _file_exists(cfg2) and _file_exists(mtype2) and _dir_exists(tk22):
            return ext

    # 4. 都无 → None（降级）
    return None
