"""webui 测试辅助：构造 /anomaly/metrics 文本 + 本地假上游/Webhook 服务。

与 middleware 的 `_helpers.py` 平级，但仅服务于 webui 测试（tests/test_webui_*.py）。
"""
from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, List, Optional, Tuple

from prometheus_client import (
    CollectorRegistry,
    Counter,
    Gauge,
    Histogram,
    generate_latest,
)

METRIC_NAMES = {
    "requests": "vllm_anomaly_requests_total",
    "detected": "vllm_anomaly_detected_total",
    "errors": "vllm_anomaly_detection_errors_total",
    "duration": "vllm_anomaly_detection_duration_seconds",
    "rare": "vllm_anomaly_last_rare_character",
    "garbled": "vllm_anomaly_last_garbled",
    "repetition": "vllm_anomaly_last_repetition",
    "nan": "vllm_anomaly_last_nan_value",
}


def make_metrics_text(
    requests: int = 0,
    detections: Optional[List[Tuple[int, str, str, int]]] = None,
    errors: int = 0,
    duration_count: int = 0,
) -> str:
    """用真实 prometheus_client 构造指标文本（对应 middleware metrics.py）。

    detections: [(ill_type(int), model, choice_index(str), count), ...]
    """
    reg = CollectorRegistry()
    r = Counter("vllm_anomaly_requests_total", "req", registry=reg)
    d = Counter(
        "vllm_anomaly_detected_total",
        "det",
        ["ill_type", "model", "choice_index"],
        registry=reg,
    )
    e = Counter("vllm_anomaly_detection_errors_total", "err", registry=reg)
    h = Histogram("vllm_anomaly_detection_duration_seconds", "dur", registry=reg)
    rare = Gauge("vllm_anomaly_last_rare_character", "rare", ["model"], registry=reg)
    garbled = Gauge("vllm_anomaly_last_garbled", "garbled", ["model"], registry=reg)
    repetition = Gauge("vllm_anomaly_last_repetition", "rep", ["model"], registry=reg)
    nan = Gauge("vllm_anomaly_last_nan_value", "nan", ["model"], registry=reg)

    r.inc(requests)
    e.inc(errors)
    seen_types = set()
    for ill, model, choice, count in detections or []:
        d.labels(ill_type=str(ill), model=model, choice_index=str(choice)).inc(count)
        seen_types.add(ill)
    for model in {m for (_, m, _, _) in detections or []}:
        rare.labels(model=model).set(1 if 1 in seen_types else 0)
        garbled.labels(model=model).set(1 if 2 in seen_types else 0)
        repetition.labels(model=model).set(1 if 3 in seen_types else 0)
        nan.labels(model=model).set(1 if 4 in seen_types else 0)
    for _ in range(duration_count):
        h.observe(0.05)
    return generate_latest(reg).decode()


def build_webui_config_dict(
    instances: Optional[List[Dict[str, Any]]] = None,
    alerts: Optional[List[Dict[str, Any]]] = None,
    auth: Optional[Dict[str, Any]] = None,
    **overrides,
) -> Dict[str, Any]:
    """构造一份合法的 webui.yaml dict（测试用，默认无实例无规则）。"""
    data = {
        "server": {"host": "127.0.0.1", "port": 9090},
        "auth": auth or {"username": "admin", "password": "test123", "token_ttl_hours": 1},
        "poll": {"interval_seconds": 3, "http_timeout_seconds": 1},
        "instances": instances or [],
        "store": {
            "event_capacity": 1000,
            "alert_capacity": 200,
            "raw_trend_window_seconds": 3600,
            "trend_bucket_seconds": 60,
            "trend_horizon_seconds": 86400,
        },
        "webhooks": {"default": ""},
        "alerts": alerts or [],
    }
    data.update(overrides)
    return data


def write_yaml(path, data: Dict[str, Any]) -> None:
    import yaml

    with open(path, "w", encoding="utf-8") as f:
        yaml.safe_dump(data, f, allow_unicode=True, sort_keys=False)


# ------------------------------------------------------------------ #
# 本地假上游：/anomaly/metrics 每次 GET 计数器递增
# ------------------------------------------------------------------ #
class _MetricsHandler(BaseHTTPRequestHandler):
    upstream: "FakeUpstream"

    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path.rstrip("/").endswith("/anomaly/metrics"):
            text = self.upstream.render()
            body = text.encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            body = b"not found"
            self.send_response(404)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)


class FakeUpstream:
    """本地假 vLLM 实例：计数器递增的 /anomaly/metrics 服务。

    step_detections: 每轮增量 [(ill_type, model, choice_index, inc), ...]
    """

    def __init__(
        self,
        step_requests: int = 1,
        step_detections: Optional[List[Tuple[int, str, str, int]]] = None,
        fail_after: Optional[int] = None,
    ) -> None:
        self._requests = 0
        self._detections: Dict[Tuple[int, str, str], int] = {}
        self.step_requests = step_requests
        self.step_detections = step_detections or []
        self.fail_after = fail_after
        self.polls = 0
        self.handler_cls = type(
            "Handler",
            (_MetricsHandler,),
            {"upstream": self},
        )
        self._server: Optional[ThreadingHTTPServer] = None
        self._thread: Optional[threading.Thread] = None

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}"

    def start(self) -> "FakeUpstream":
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), self.handler_cls)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()

    def render(self) -> str:
        self.polls += 1
        if self.fail_after is not None and self.polls > self.fail_after:
            raise RuntimeError("upstream forced failure")
        self._requests += self.step_requests
        dets = []
        for ill, model, choice, inc in self.step_detections:
            key = (ill, model, choice)
            self._detections[key] = self._detections.get(key, 0) + inc
            dets.append((ill, model, choice, self._detections[key]))
        return make_metrics_text(self._requests, dets)


# ------------------------------------------------------------------ #
# 本地 Webhook 接收器
# ------------------------------------------------------------------ #
class _WebhookHandler(BaseHTTPRequestHandler):
    sink: "WebhookSink"

    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        try:
            self.sink.record(json.loads(body.decode("utf-8")))
        except Exception:
            pass
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()


class WebhookSink:
    def __init__(self) -> None:
        self.received: List[Dict[str, Any]] = []
        self.handler_cls = type("WH", (_WebhookHandler,), {"sink": self})
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), self.handler_cls)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}/hook"

    def record(self, payload: Dict[str, Any]) -> None:
        self.received.append(payload)

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
