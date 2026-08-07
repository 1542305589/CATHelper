"""pytest 配置：确保 `vllm_anomaly_middleware` 可被导入（无论是否 pip install）。

项目根目录即包目录（flat layout），故将项目“父目录”加入 sys.path，
使 `import vllm_anomaly_middleware` 在任意 CWD 下均可解析。
"""
import os
import sys

_THIS = os.path.dirname(os.path.abspath(__file__))
_PARENT = os.path.dirname(_THIS)
if _PARENT not in sys.path:
    sys.path.insert(0, _PARENT)

# 确保 vllm_anomaly_middleware 包可被解析（flat layout）
import vllm_anomaly_middleware  # noqa: F401,E402  (确保导入路径生效)
