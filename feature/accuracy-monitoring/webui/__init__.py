"""推理精度异常监控 Web 界面（独立 Web 服务，多 vLLM 实例聚合）。

设计见 docs/2026-08-11-anomaly-monitoring-webui-design.md。
与 anomaly_middleware 完全解耦：仅通过轮询各实例 `/anomaly/metrics` 获取指标。
"""

__version__ = "0.1.0"
