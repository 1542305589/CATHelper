"""异常信息本地保存（design §3 / spec 异常本地保存）。

AnomalyStore：异常编号分配 + pickle 落盘 + 内存镜像。
- 落盘开启时：首次保存读文件同步 max-key（防重启覆盖），内存镜像 _data 避免每次重读，
  磁盘写经 run_in_executor offload 到线程（不阻塞事件循环），asyncio.Lock 串行化写盘。
- 落盘关闭时：仅内存计数器累加（重启归零，无文件可覆盖）。

构造在 AnomalyMiddleware.__init__（enabled=True 时）；enabled=False 纯透传不构造。
"""
from __future__ import annotations

import asyncio
import os
import pickle
import re
from typing import Any, Dict, Optional

from .logging import get_logger

logger = get_logger()


def sanitize_model_name(name: Optional[str]) -> str:
    """把 served model 名净化为可作文件名的 basename。

    `Qwen/Qwen3-0.6B` -> `Qwen3-0.6B`；`/data/m/Qwen3` -> `Qwen3`；
    非法字符替换为 `_`；空 -> `anomalies`。
    """
    if not name:
        return "anomalies"
    base = re.split(r"[\\/]", name.strip())
    seg = base[-1] if base else name.strip()
    cleaned = re.sub(r"[^\w.\-]", "_", seg).strip("._")
    return cleaned or "anomalies"


class AnomalyStore:
    """异常编号分配 + pickle 落盘。

    Parameters
    ----------
    save_path : Optional[str]
        绝对路径。None -> 不落盘（仅计数器）。以 .pkl 结尾 -> 文件模式；
        否则 -> 文件夹模式（文件名 = <model_name>.pkl）。
    model_name : Optional[str]
        served model 名（文件夹模式默认文件名来源）。
    """

    def __init__(
        self,
        save_path: Optional[str],
        model_name: Optional[str] = None,
    ) -> None:
        self.enabled = save_path is not None
        self.file_path: Optional[str] = None
        self._counter: int = 0
        self._synced: bool = False
        self._data: Dict[int, Dict[str, Any]] = {}
        self._lock = asyncio.Lock()

        if not self.enabled:
            return

        assert save_path is not None
        if save_path.lower().endswith(".pkl"):
            # 文件模式：父目录必须预先存在；文件本身可不存在（首次保存创建）
            parent = os.path.dirname(save_path) or "."
            if not os.path.isdir(parent):
                raise FileNotFoundError(
                    f"保存的路径不存在: {parent}（VLLM_ANOMALY_SAVE_PATH 父目录）"
                )
            self.file_path = save_path
        else:
            # 文件夹模式：目录必须预先存在；文件首次保存创建
            if not os.path.isdir(save_path):
                raise FileNotFoundError(
                    f"保存的路径不存在: {save_path}（VLLM_ANOMALY_SAVE_PATH 目录）"
                )
            self.file_path = os.path.join(
                save_path, f"{sanitize_model_name(model_name)}.pkl"
            )

    def _sync_first(self) -> None:
        """首次读文件同步 max-key + 内存镜像（锁内、一次性）。"""
        self._synced = True
        if not self.file_path:
            return
        try:
            if os.path.exists(self.file_path):
                with open(self.file_path, "rb") as f:
                    d = pickle.load(f)
                if isinstance(d, dict):
                    self._data = d
                    keys = [
                        k for k in d.keys()
                        if isinstance(k, int) and not isinstance(k, bool)
                    ]
                    if keys:
                        self._counter = max(self._counter, max(keys))
                    logger.info(
                        "异常保存文件已载入, 已有 %d 条记录, 计数器同步至 %d",
                        len(self._data), self._counter,
                    )
        except Exception as exc:
            logger.warning(
                "读取异常保存文件失败, 从空开始: %s", exc
            )
            self._data = {}

    def _write_sync(self) -> None:
        """线程内写盘（_data 在锁保护下不会被并发修改）。"""
        assert self.file_path is not None
        with open(self.file_path, "wb") as f:
            pickle.dump(self._data, f)

    async def save(self, record: Dict[str, Any]) -> int:
        """分配异常编号 + 落盘（若开启）。返回异常编号。

        编号自增不回退；落盘失败仅向上抛（调用方 _run 捕获 log），编号已自增不回退。
        """
        async with self._lock:
            if self.enabled and not self._synced:
                self._sync_first()
            self._counter += 1
            aid = self._counter
            if self.enabled:
                self._data[aid] = record
                loop = asyncio.get_running_loop()
                await loop.run_in_executor(None, self._write_sync)
            return aid

    @property
    def counter(self) -> int:
        """当前已分配的最新异常编号（0 表示尚未分配）。"""
        return self._counter
