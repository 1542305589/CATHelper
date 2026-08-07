"""配置与检测器路径解析（design §3.8 / spec §2.11 / §2.12）。

重构后检测器仅依赖 config.yaml（算法阈值）+ 运行时注入的 tk2cat 映射。
mtype_config.json / token2category 预生成文件已移除（运行时生成取代），
故路径解析仅返回 config.yaml 单路径：显式 env 覆盖 → vendored 固定路径 → None（降级）。
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Optional

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
    tokenizer_model: Optional[str] = None

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
            tokenizer_model=_env_str("VLLM_ANOMALY_TOKENIZER_MODEL"),
        )


def _file_exists(path: str) -> bool:
    return os.path.isfile(path)


def _vendored_config_path(base_dir: str) -> str:
    """包内 vendored 检测器算法默认值固定路径（design §2.2 / §3.8 tier 1）。

    `defaults/detector.yaml` 是检测器算法默认参数（vendored 资源）；
    与 `config.py` 运行时插件环境配置（PluginConfig.from_env）分离。
    """
    return os.path.join(base_dir, "defaults", "detector.yaml")


def resolve_config_path(
    config: PluginConfig, base_dir: Optional[str] = None
) -> Optional[str]:
    """解析检测器 detector.yaml 路径。

    顺序：
      1. 显式 env 覆盖（VLLM_ANOMALY_DETECTOR_CONFIG_PATH，文件须存在）。
      2. vendored 固定路径（defaults/detector.yaml，默认/主）。
      3. 都无 → None（触发降级）。
    """
    # 1. 显式覆盖
    if config.detector_config_path and _file_exists(config.detector_config_path):
        return config.detector_config_path

    # 2. vendored 固定路径
    if base_dir is None:
        base_dir = os.path.dirname(os.path.abspath(__file__))
    cfg = _vendored_config_path(base_dir)
    if _file_exists(cfg):
        return cfg

    # 3. 都无 → None（降级）
    return None
