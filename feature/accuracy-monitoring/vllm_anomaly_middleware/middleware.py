"""AnomalyMiddleware + RequestContext + ResponseInterceptor + ASGI 助手（design §4.1/§4.2, spec §3）。

纯 ASGI 中间件（非 Starlette BaseHTTPMiddleware），SSE 增量转发，不缓冲整流。
检测 fire-and-forget，客户端永远不等待检测。
"""
from __future__ import annotations

import json
import logging
import random
import threading
import uuid
from dataclasses import dataclass
from typing import Awaitable, Callable, List, Optional, Set, Tuple

from .config import PluginConfig, resolve_detector_paths
from .detector_runner import DetectorRunner, schedule_detection
from .extractor import (
    OriginalParams,
    SSEStreamProcessor,
    extract_chat_response,
    extract_completions_response,
    inject_params,
    save_original_params,
    strip_chat_response,
    strip_completions_response,
)
from .metrics import METRICS_CONTENT_TYPE, Metrics

logger = logging.getLogger("vllm_anomaly_middleware")

TARGET_PATHS = ("/v1/chat/completions", "/v1/completions")


@dataclass
class RequestContext:
    orig: OriginalParams
    is_chat: bool
    model: str
    request_id: str
    will_detect: bool
    top_logprobs: int


# --------------------------------------------------------------------------- #
# ASGI 请求助手（§3.1 / §3.2）
# --------------------------------------------------------------------------- #
async def _read_all_body(receive: Callable[[], Awaitable[dict]]) -> bytes:
    """聚合所有 http.request body 至 more_body=False；遇 http.disconnect 中止。"""
    body = bytearray()
    while True:
        msg = await receive()
        t = msg.get("type")
        if t == "http.request":
            chunk = msg.get("body", b"") or b""
            if chunk:
                body.extend(chunk)
            if not msg.get("more_body", False):
                break
        elif t == "http.disconnect":
            break
        # 其它消息忽略
    return bytes(body)


def _make_replay_receive(
    original_receive: Callable[[], Awaitable[dict]], body: bytes
) -> Callable[[], Awaitable[dict]]:
    """首次返回合成单条 http.request(body, more_body=False)；后续委托原始 receive()。

    绝不返回空 body 的 http.request（vLLM 会重复处理请求）。
    """
    sent = False

    async def receive():
        nonlocal sent
        if not sent:
            sent = True
            return {"type": "http.request", "body": body, "more_body": False}
        return await original_receive()

    return receive


def _patch_scope_content_length(scope: dict, length: int) -> dict:
    """浅拷贝 scope + 拷贝 headers，改写/补 content-length。"""
    new_scope = dict(scope)
    new_headers = [list(h) for h in scope.get("headers", [])]
    cl = str(length).encode("latin-1")
    found = False
    for h in new_headers:
        if h and len(h) >= 2 and h[0].lower() == b"content-length":
            h[1] = cl
            found = True
            break
    if not found:
        new_headers.append([b"content-length", cl])
    new_scope["headers"] = new_headers
    return new_scope


def _is_chat_path(path: str) -> bool:
    return path == "/v1/chat/completions"


# --------------------------------------------------------------------------- #
# ResponseInterceptor（§4.2）
# --------------------------------------------------------------------------- #
class ResponseInterceptor:
    def __init__(
        self,
        send: Callable[[dict], Awaitable[None]],
        *,
        ctx: RequestContext,
        runner: Optional[DetectorRunner],
        metrics: Metrics,
        pending_tasks: Set,
    ) -> None:
        self._send = send
        self._ctx = ctx
        self._runner = runner
        self._metrics = metrics
        self._pending_tasks = pending_tasks
        self._is_streaming = False
        self._start_msg: Optional[dict] = None
        self._body_buf = bytearray()
        self._sse: Optional[SSEStreamProcessor] = None
        self._finished = False
        self._detection_scheduled = False
        self._detection_results: List[Tuple[List, List]] = []

    async def __call__(self, message: dict) -> None:
        t = message.get("type")
        if t == "http.response.start":
            await self._on_start(message)
        elif t == "http.response.body":
            await self._on_body(message)
        else:
            await self._send(message)

    async def _on_start(self, message: dict) -> None:
        headers = [list(h) for h in message.get("headers", [])]
        headers.append(
            [b"x-anomaly-request-id", self._ctx.request_id.encode("latin-1")]
        )
        msg = dict(message)
        msg["headers"] = headers
        ct = b""
        for h in headers:
            if h and len(h) >= 2 and h[0].lower() == b"content-type":
                ct = (h[1] or b"").lower()
                break
        self._is_streaming = b"text/event-stream" in ct
        if self._is_streaming:
            self._sse = SSEStreamProcessor(
                self._ctx.is_chat, self._ctx.orig, self._ctx.top_logprobs
            )
            await self._send(msg)  # 流式立即发 start
        else:
            self._start_msg = msg  # 非流式缓冲，发 body 前 patch CL 再发

    async def _on_body(self, message: dict) -> None:
        if self._finished:
            return  # 防重复终端 body
        body = message.get("body", b"") or b""
        more = message.get("more_body", False)
        if self._is_streaming:
            await self._on_body_streaming(body, more)
        else:
            await self._on_body_nonstreaming(body, more)

    async def _on_body_streaming(self, body: bytes, more: bool) -> None:
        out = self._sse.feed(body)
        if more:
            if out:
                await self._send(
                    {"type": "http.response.body", "body": out, "more_body": True}
                )
            return
        # 终端块
        tail = self._sse.flush()
        final = (out or b"") + tail
        await self._send(
            {"type": "http.response.body", "body": final, "more_body": False}
        )
        self._finished = True
        self._maybe_schedule_detection()

    async def _on_body_nonstreaming(self, body: bytes, more: bool) -> None:
        self._body_buf.extend(body)
        if more or self._finished:
            return
        final = self._process_complete()
        await self._send_start(final)
        await self._send(
            {"type": "http.response.body", "body": final, "more_body": False}
        )
        self._finished = True
        self._maybe_schedule_detection()

    def _process_complete(self) -> bytes:
        raw = bytes(self._body_buf)
        try:
            data = json.loads(raw)
        except Exception:
            return raw  # 非 JSON（错误页）→ 原样透传，不注入检测
        if not isinstance(data, dict):
            return raw
        if self._ctx.is_chat:
            self._detection_results = extract_chat_response(
                data, self._ctx.top_logprobs
            )
            strip_chat_response(data, self._ctx.orig)
        else:
            self._detection_results = extract_completions_response(
                data, self._ctx.top_logprobs
            )
            strip_completions_response(data, self._ctx.orig)
        return json.dumps(data, ensure_ascii=False).encode("utf-8")

    async def _send_start(self, final: bytes) -> None:
        msg = self._start_msg
        if msg is None:
            msg = {"type": "http.response.start", "status": 200, "headers": []}
        headers = [list(h) for h in msg.get("headers", [])]
        cl = str(len(final)).encode("latin-1")
        found = False
        for h in headers:
            if h and len(h) >= 2 and h[0].lower() == b"content-length":
                h[1] = cl
                found = True
                break
        if not found:
            headers.append([b"content-length", cl])
        out = dict(msg)
        out["headers"] = headers
        await self._send(out)

    def _maybe_schedule_detection(self) -> None:
        if (
            not self._ctx.will_detect
            or self._detection_scheduled
            or self._runner is None
        ):
            return
        self._detection_scheduled = True
        try:
            topk, tokens, configs = self._get_detection_inputs()
        except Exception as exc:
            logger.error("获取检测数据失败: %s", exc)
            return
        if not tokens or not any(tokens):
            return  # 空响应不检测
        try:
            schedule_detection(
                self._runner,
                topk,
                tokens,
                configs,
                request_id=self._ctx.request_id,
                model=self._ctx.model,
                metrics=self._metrics,
                pending_tasks=self._pending_tasks,
            )
        except RuntimeError as exc:
            # 无运行事件循环等：记录后跳过（不影响客户端）
            logger.warning("无法调度检测任务: %s", exc)

    def _get_detection_inputs(
        self,
    ) -> Tuple[List[List[dict]], List[List[int]], List[str]]:
        if self._is_streaming:
            return self._sse.get_detection_data()
        topk_all = [r[0] for r in self._detection_results]
        tokens_all = [r[1] for r in self._detection_results]
        n = max(len(topk_all), 1)
        configs = [self._ctx.model] * n
        return topk_all, tokens_all, configs


# --------------------------------------------------------------------------- #
# AnomalyMiddleware（§4.1）
# --------------------------------------------------------------------------- #
class AnomalyMiddleware:
    def __init__(self, app: Callable) -> None:
        self.app = app
        try:
            self.config = PluginConfig.from_env()
        except Exception as exc:
            logger.error("配置无效, 降级透传: %s", exc)
            self.config = PluginConfig(enabled=False)
        self.metrics = Metrics()
        self._runner: Optional[DetectorRunner] = None
        self._runner_lock = threading.Lock()
        self._runner_inited = False
        self._pending_tasks: Set = set()

    async def __call__(self, scope: dict, receive, send) -> None:
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return
        method = scope.get("method", "")
        path = scope.get("path", "")
        # 指标端点（内联，仅 GET）
        if method == "GET" and path == self.config.metrics_path:
            await self._serve_metrics(send)
            return
        # 降级或非目标 → 透传（不读 body）
        if not self.config.enabled:
            await self.app(scope, receive, send)
            return
        if method != "POST" or path not in TARGET_PATHS:
            await self.app(scope, receive, send)
            return
        # 采样：未中 → 纯透传（不读 body、不注入、不恢复、不检测）
        if random.random() >= self.config.sample_rate:
            await self.app(scope, receive, send)
            return
        # 选中：读 body
        raw = await _read_all_body(receive)
        try:
            body = json.loads(raw)
        except Exception:
            body = None
        if not isinstance(body, dict):
            # 非 dict/非 JSON → 原样重放透传
            replay = _make_replay_receive(receive, raw)
            await self.app(scope, replay, send)
            return
        # _ensure_runner（双检锁，廉价）；失败 → 本请求重放透传
        if not self._ensure_runner():
            replay = _make_replay_receive(receive, raw)
            await self.app(scope, replay, send)
            return
        is_chat = _is_chat_path(path)
        orig = save_original_params(body, is_chat)
        new_body = inject_params(body, is_chat, self.config.top_logprobs)
        new_scope = _patch_scope_content_length(scope, len(new_body))
        request_id = uuid.uuid4().hex
        model = body.get("model") or "unknown"
        ctx = RequestContext(
            orig=orig,
            is_chat=is_chat,
            model=model,
            request_id=request_id,
            will_detect=True,
            top_logprobs=self.config.top_logprobs,
        )
        replay = _make_replay_receive(receive, new_body)
        interceptor = ResponseInterceptor(
            send,
            ctx=ctx,
            runner=self._runner,
            metrics=self.metrics,
            pending_tasks=self._pending_tasks,
        )
        await self.app(new_scope, replay, interceptor)

    def _ensure_runner(self) -> bool:
        """双检锁构造 DetectorRunner（廉价：线程池+锁+路径）。失败 → 永久降级。"""
        if self._runner_inited:
            return self._runner is not None
        with self._runner_lock:
            if self._runner_inited:
                return self._runner is not None
            try:
                paths = resolve_detector_paths(self.config)
                if paths is None:
                    logger.error("检测器路径解析失败, 永久降级透传")
                    self.config.enabled = False
                    self._runner_inited = True
                    return False
                cfg, mtype, tk2 = paths
                self._runner = DetectorRunner(
                    cfg, mtype, tk2, self.config.detector_workers
                )
                self._runner_inited = True
                return True
            except Exception as exc:
                logger.error("runner 构造失败, 永久降级透传: %s", exc)
                self.config.enabled = False
                self._runner_inited = True
                return False

    async def _serve_metrics(self, send: Callable[[dict], Awaitable[None]]) -> None:
        body = self.metrics.render_metrics()
        headers = [
            [b"content-type", METRICS_CONTENT_TYPE.encode("latin-1")],
            [b"content-length", str(len(body)).encode("latin-1")],
        ]
        await send(
            {"type": "http.response.start", "status": 200, "headers": headers}
        )
        await send(
            {"type": "http.response.body", "body": body, "more_body": False}
        )

    def shutdown(self) -> None:
        for t in list(self._pending_tasks):
            t.cancel()
        self._pending_tasks.clear()
        if self._runner is not None:
            self._runner.shutdown()
