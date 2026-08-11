"""pytest 配置：确保 anomaly_middleware 包可导入（无论是否 pip install）。

项目根目录即 pyproject.toml 所在目录；anomaly_middleware/ 为包目录。
将项目根目录加入 sys.path，使 `import anomaly_middleware` 在任意 CWD 下均可解析。
"""
import os
import sys

_ROOT = os.path.dirname(os.path.abspath(__file__))
if _ROOT not in sys.path:
    sys.path.insert(0, _ROOT)
