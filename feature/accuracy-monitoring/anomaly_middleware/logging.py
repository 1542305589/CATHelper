"""共享彩色日志配置。

所有模块统一通过 `get_logger()` 获取 logger（名称固定为 anomaly_middleware），
保证全局只有一份 handler 与格式；info=绿、warning=黄、error/debug 分级着色，
logger 名称用蓝色。依赖 colorlog，Windows 终端 / Win11 / VS Code 均支持 ANSI。

注意：保持 propagate=True，pytest caplog 依赖记录传播到 root handler。
"""
from __future__ import annotations

import logging
import threading

import colorlog

_LOGGER_NAME = "anomaly_middleware"

_log_colors = {
    "DEBUG": "light_black",
    "INFO": "green",
    "WARNING": "yellow",
    "ERROR": "red",
    "CRITICAL": "bold_red",
}

_cache: dict = {}
_lock = threading.Lock()


def get_logger(level: int = logging.DEBUG) -> logging.Logger:
    """幂等获取共享 logger；首次调用时挂载彩色 StreamHandler。

    同一进程内重复调用不重复添加 handler。
    """
    with _lock:
        if _LOGGER_NAME in _cache:
            return _cache[_LOGGER_NAME]
        logger = logging.getLogger(_LOGGER_NAME)
        logger.setLevel(level)
        logger.propagate = False
        if not logger.handlers:
            handler = colorlog.StreamHandler()
            handler.setFormatter(
                colorlog.ColoredFormatter(
                    "%(log_color)s%(levelname)s%(reset)s %(asctime)s %(blue)s[%(name)s]%(reset)s %(message)s",
                    datefmt="%Y-%m-%d %H:%M:%S",
                    log_colors=_log_colors
                )
            )
            logger.addHandler(handler)
        _cache[_LOGGER_NAME] = logger
        return logger