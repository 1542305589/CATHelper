"""检测运行器（design §4.4 / §6.1 / §6.2 / §6.3 / §6.5）。

两段式懒加载：
- 廉价阶段（请求路径，_ensure_runner）：仅 ThreadPoolExecutor + Lock + config 路径，无 numpy/文件 I/O。
- 重阶段（worker 线程内，首次检测）：懒构造 ILLDetector（numpy），不阻塞 event loop。
检测串行化：单 worker 线程池 + threading.Lock；run_sync 在锁内调 detector.run。
检测任务：fire-and-forget asyncio.create_task，异常全捕获计 error，不影响客户端。

重构后：DetectorRunner 仅持 config_path（无 mtype/tk2cat 路径）；topk 由 topk_n 参数传入；
tk2cat 映射由 set_vocabulary 注入（运行时生成），_get_detector 懒构造时同步注入。
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
        max_workers: int = 1,
        topk_n: Optional[int] = None,
    ) -> None:
        self._config_path = config_path
        self._topk_n = topk_n
        # 运行时 tk2cat 映射缓存（由 set_vocabulary 注入）
        self._tk2cat = None
        self._vocab_size: Optional[int] = None
        self._executor = ThreadPoolExecutor(max_workers=max(1, max_workers))
        self._lock = threading.Lock()
        self._detector = None  # worker 线程内懒构造
        self._unusable = False
        self._unusable_reason: Optional[str] = None

    def set_vocabulary(self, tk2cat, vocab_size) -> None:
        """注入运行时生成的 tk2cat 映射（幂等，覆盖）。供 middleware 预热/慢路径调用。"""
        self._tk2cat = tk2cat
        self._vocab_size = vocab_size
        # 已构造的检测器也同步注入（懒构造尚未发生时由 _get_detector 兜底注入）
        if self._detector is not None:
            self._detector.set_vocabulary(tk2cat, vocab_size)

    def _get_detector(self):
        """在 run_sync 内（锁内、worker 线程）调用。失败则标记 unusable，后续快速失败。"""
        if self._unusable:
            raise RuntimeError(f"detector unusable: {self._unusable_reason}")
        if self._detector is None:
            try:
                from .detector import ILLDetector

                self._detector = ILLDetector(self._config_path)
                if self._tk2cat is not None:
                    self._detector.set_vocabulary(self._tk2cat, self._vocab_size)
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
    ):
        with self._lock:
            detector = self._get_detector()
            return detector.run(topk, tokens, topk_n=self._topk_n)

    async def run_async(
        self,
        topk: List[List[dict]],
        tokens: List[List[int]],
    ):
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(
            self._executor, self.run_sync, topk, tokens
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
                results = await runner.run_async(topk, tokens)
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
