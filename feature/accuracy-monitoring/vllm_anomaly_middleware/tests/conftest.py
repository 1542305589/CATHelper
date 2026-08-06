"""pytest fixtures：httpx ASGI 客户端工厂 + 检测任务 drain 助手。"""
from __future__ import annotations

import asyncio
import os
import sys

import httpx
import pytest

# 确保父目录在 sys.path（包可导入）；tests 目录由 pytest 自动加入
_PARENT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _PARENT not in sys.path:
    sys.path.insert(0, _PARENT)

from vllm_anomaly_middleware import AnomalyMiddleware  # noqa: E402
from vllm_anomaly_middleware.config import PluginConfig  # noqa: E402
from _helpers import FakeVLLM  # noqa: E402


@pytest.fixture
async def client_factory():
    """返回工厂函数 make(response_fn, **cfg) -> (client, fake, mw)。

    用 mw.config 直接覆盖，避免依赖 env；每个 mw 独立 runner。
    """
    created_clients = []
    created_mws = []

    def _make(
        response_fn,
        *,
        top_logprobs: int = 20,
        sample_rate: float = 1.0,
        enabled: bool = True,
        workers: int = 1,
        metrics_path: str = "/anomaly/metrics",
    ):
        fake = FakeVLLM(response_fn)
        mw = AnomalyMiddleware(fake)
        mw.config = PluginConfig(
            enabled=enabled,
            top_logprobs=top_logprobs,
            metrics_path=metrics_path,
            sample_rate=sample_rate,
            detector_workers=workers,
        )
        mw._runner = None
        mw._runner_inited = False
        client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=mw), base_url="http://test"
        )
        created_clients.append(client)
        created_mws.append(mw)
        return client, fake, mw

    yield _make
    for c in created_clients:
        await c.aclose()
    for mw in created_mws:
        mw.shutdown()


async def drain(mw, timeout: float = 10.0) -> None:
    """等待所有 fire-and-forget 检测任务完成（用于 e2e 断言 metrics）。"""
    tasks = list(mw._pending_tasks)
    if not tasks:
        return
    await asyncio.wait_for(
        asyncio.gather(*tasks, return_exceptions=True), timeout
    )
