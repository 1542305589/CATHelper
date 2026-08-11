"""anomaly_middleware：vLLM 推理精度异常检测 ASGI 中间件。

短路径 `anomaly_middleware.AnomalyMiddleware` 与长路径
`anomaly_middleware.middleware.AnomalyMiddleware` 均可解析。
部署：`vllm serve <model> --middleware anomaly_middleware.AnomalyMiddleware`
"""
from .middleware import AnomalyMiddleware, ResponseInterceptor, RequestContext

__all__ = ["AnomalyMiddleware", "ResponseInterceptor", "RequestContext"]
__version__ = "0.1.0"
