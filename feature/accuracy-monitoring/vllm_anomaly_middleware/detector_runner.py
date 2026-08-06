"""检测运行器（design §4.4 / §6.1 / §6.2 / §6.3）。

两段式懒加载：
- 廉价阶段（请求路径，_ensure_runner）：仅 ThreadPoolExecutor + Lock + 路径，无 numpy/文件 I/O。
- 重阶段（worker 线程内，首次检测）：懒构造 ILLDetector（numpy + 多 MB JSON），不阻塞 event loop。
检测串行化：单 worker 线程池 + threading.Lock；run_sync 在锁内调 detector.run。
检测任务：fire-and-forget asyncio.create_task，异常全捕获计 error，不影响客户端。
"""
from __future__ import annotations

import asyncio
import logging
import threading
from concurrent.futures import ThreadPoolExecutor
from typing import List, Optional, Set

from .metrics import Metrics

logger = logging.getLogger("vllm_anomaly_middleware")


class DetectorRunner:
    def __init__(
        self,
        config_path: str,
        mtype_path: str,
        tk2cat_path: str,
        max_workers: int = 1,
    ) -> None:
        self._config_path = config_path
        self._mtype_path = mtype_path
        self._tk2cat_path = tk2cat_path
        self._executor = ThreadPoolExecutor(max_workers=max(1, max_workers))
        self._lock = threading.Lock()
        self._detector = None  # worker 线程内懒构造
        self._unusable = False
        self._unusable_reason: Optional[str] = None

    def _get_detector(self):
        """在 run_sync 内（锁内、worker 线程）调用。失败则标记 unusable，后续快速失败。"""
        if self._unusable:
            raise RuntimeError(f"detector unusable: {self._unusable_reason}")
        if self._detector is None:
            try:
                from .response_anomaly.detector import ILLDetector

                self._detector = ILLDetector(
                    self._config_path, self._mtype_path, self._tk2cat_path
                )
            except Exception as exc:  # 构造失败（numpy/配置损坏等）
                self._unusable = True
                self._unusable_reason = f"{type(exc).__name__}: {exc}"
                logger.error("ILLDetector 构造失败, 后续检测将快速失败计 error: %s", exc)
                raise
        return self._detector

    def run_sync(
        self,
        topk: List[List[dict]],
        tokens: List[List[int]],
        model_configs: List,
    ):
        with self._lock:
            detector = self._get_detector()
            return detector.run(topk, tokens, model_configs)

    async def run_async(
        self,
        topk: List[List[dict]],
        tokens: List[List[int]],
        model_configs: List,
    ):
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(
            self._executor, self.run_sync, topk, tokens, model_configs
        )

    def shutdown(self) -> None:
        try:
            self._executor.shutdown(wait=False, cancel_futures=True)
        except TypeError:
            # Python<3.9 无 cancel_futures
            self._executor.shutdown(wait=False)


def schedule_detection(
    runner: DetectorRunner,
    topk: List[List[dict]],
    tokens: List[List[int]],
    model_configs: List,
    *,
    request_id: str,
    model: str,
    metrics: Metrics,
    pending_tasks: Set,
) -> asyncio.Task:
    """fire-and-forget 检测任务。异常全捕获计 error，不影响客户端。"""

    async def _run() -> None:
        with metrics.detection_duration.time():
            try:
                results = await runner.run_async(topk, tokens, model_configs)
                metrics.record_detection(results, model)
            except Exception as exc:
                logger.error(
                    "检测失败 request_id=%s model=%s: %s", request_id, model, exc
                )
                metrics.record_error()

    task = asyncio.create_task(_run())
    pending_tasks.add(task)

    def _done(t: asyncio.Task) -> None:
        pending_tasks.discard(t)
        # 吞掉未检索异常，避免 "Task exception was never retrieved"
        try:
            t.exception()
        except asyncio.CancelledError:
            pass
        except Exception:
            pass

    task.add_done_callback(_done)
    return task
