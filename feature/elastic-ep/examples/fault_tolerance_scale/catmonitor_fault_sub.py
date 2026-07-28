#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""CATMonitor fault-event subscriber for the Elastic-EP fault manager.

This module replaces the old DCMI-polling path in ``scale_down_demo.py``.
Instead of calling ``libdcmi.so`` directly, it subscribes to CATMonitor's
fault-subscription API:

  1. On start it POSTs a subscription to CATMonitor REST ``/faultsub/
     subscriptions``, declaring which fault types / NPUs to receive and the
     callback URL CATMonitor should POST events back to.
  2. It runs a lightweight ``http.server.ThreadingHTTPServer`` that receives
     ``POST /fault_event`` (JSON ``FaultEvent``) from CATMonitor.
  3. On each event it maps the NPU id to a DP rank (deployment topology is
     local knowledge) and issues ``pause`` / ``scale_down`` (or ``retry`` for
     recovery) to vLLM's ``/fault_tolerance/apply`` REST API.
  4. On stop it DELETEs its subscription so CATMonitor stops pushing.

Only the Python standard library + ``requests`` are used (no DCMI / ZMQ
dependency for this path). The vLLM engine-health ZMQ SUB path stays in
``scale_down_demo.py`` unchanged.
"""

from __future__ import annotations

import json
import sys
import threading
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Dict, List, Optional

import requests

# Fault types emitted by CATMonitor's FaultDetector (see features/faultsub/
# event.go). The subscriber can ask for a subset via --fault-types.
ALL_FAULT_TYPES = [
    "card_drop",
    "npu_health",
    "npu_error_code",
    "hbm_uce",
    "ddr_uce",
    "roce_link_down",
    "driver_unhealthy",
]

# Fault types that warrant an irreversible scale_down (hardware gone).
SCALE_DOWN_TYPES = {"card_drop", "npu_health", "hbm_uce", "ddr_uce"}


def parse_fault_types(raw: str) -> List[str]:
    """Parse a comma-separated fault-type list, validating against the set
    CATMonitor can emit. Exits (SystemExit) on an unknown type."""
    types = [t.strip() for t in raw.split(",") if t.strip()]
    unknown = [t for t in types if t not in ALL_FAULT_TYPES]
    if unknown:
        sys.stderr.write(
            f"Error: unknown fault type(s): {unknown}. "
            f"Available: {','.join(ALL_FAULT_TYPES)}\n"
        )
        raise SystemExit(2)
    return types


def parse_npu_ids(raw: str) -> List[int]:
    """Parse ``0-3`` (range) or ``0,1,5`` (list) into an int list."""
    if "-" in raw and "," not in raw:
        lo, hi = raw.split("-", 1)
        return list(range(int(lo), int(hi) + 1))
    return [int(x) for x in raw.split(",") if x.strip() != ""]


@dataclass
class FaultSubscriberConfig:
    vllm_host: str = "localhost"
    vllm_port: int = 8006
    catmonitor_host: str = "localhost"
    catmonitor_rest_port: int = 9101
    callback_host: str = "0.0.0.0"
    callback_port: int = 9102
    advertise_url: str = "http://localhost:9102/fault_event"
    fault_types: List[str] = field(
        default_factory=lambda: [
            "card_drop",
            "npu_error_code",
            "hbm_uce",
            "roce_link_down",
        ]
    )
    npu_ids: List[int] = field(default_factory=lambda: list(range(16)))
    debounce_ms: int = 0
    min_severity: str = "warning"
    recovery_timeout: int = 120


class CatMonitorFaultSubscriber:
    """Subscribes to CATMonitor fault events and drives vLLM fault tolerance.

    NPU id -> DP rank mapping is supplied by the caller (deployment topology
    is local knowledge; CATMonitor does not know about vLLM DP ranks).
    """

    def __init__(self, cfg: FaultSubscriberConfig, npu_to_dp: Dict[int, int]):
        self.cfg = cfg
        self.npu_to_dp = npu_to_dp
        self._server: Optional[ThreadingHTTPServer] = None
        self._thread: Optional[threading.Thread] = None
        self._subscription_id: Optional[str] = None
        self._active_faults: Dict[str, str] = {}  # npu_id -> fault type (dedup)
        self._lock = threading.Lock()

    # ---- vLLM control ----

    def _vllm_apply(self, instruction: str, params: dict, timeout: int = 300) -> bool:
        url = f"http://{self.cfg.vllm_host}:{self.cfg.vllm_port}/fault_tolerance/apply"
        payload = {"instruction": instruction, "params": params}
        try:
            resp = requests.post(url, json=payload, timeout=timeout)
            print(
                f"[faultsub] {instruction} -> {resp.status_code}: {resp.text[:200]}"
            )
            return resp.status_code == 200
        except requests.RequestException as exc:
            print(f"[faultsub] {instruction} request failed: {exc}")
            return False

    def pause(self, exclude_dp_ranks: List[int]) -> bool:
        return self._vllm_apply(
            "pause",
            {"timeout": self.cfg.recovery_timeout, "exclude_engine_index": exclude_dp_ranks},
        )

    def scale_down(self, exclude_dp_ranks: List[int]) -> bool:
        return self._vllm_apply(
            "scale_down",
            {"timeout": self.cfg.recovery_timeout, "exclude_dp_ranks": exclude_dp_ranks},
        )

    def retry(self, dp_ranks: Optional[List[int]] = None) -> bool:
        params: dict = {"timeout": self.cfg.recovery_timeout}
        if dp_ranks:
            params["exclude_dp_ranks"] = dp_ranks
        return self._vllm_apply("retry", params)

    # ---- event handling ----

    def _handle_event(self, event: dict) -> None:
        ev_type = event.get("type", "")
        npu_id = str(event.get("npu_id", ""))
        recovered = bool(event.get("recovered", False))
        dp_rank = self.npu_to_dp.get(int(npu_id), -1)
        print(
            f"[faultsub] event type={ev_type} npu={npu_id} dp={dp_rank} "
            f"recovered={recovered} detail={event.get('detail')}"
        )
        if dp_rank < 0:
            print(f"[faultsub] NPU {npu_id} not in dp map, skip")
            return

        if recovered:
            # A transient fault (e.g. network flash) cleared -> retry.
            with self._lock:
                self._active_faults.pop(npu_id, None)
            self.retry([dp_rank])
            return

        # Dedup: a persistent fault may be re-sent by CATMonitor on restart;
        # only act when newly seen.
        with self._lock:
            prev = self._active_faults.get(npu_id)
            if prev == ev_type:
                print(f"[faultsub] duplicate {ev_type} for NPU {npu_id}, skip")
                return
            self._active_faults[npu_id] = ev_type

        if ev_type in SCALE_DOWN_TYPES:
            self.pause([dp_rank])
            self.scale_down([dp_rank])
        else:
            # npu_error_code / roce_link_down (not recovered): pause and let
            # the operator / engine health path decide retry vs scale_down.
            self.pause([dp_rank])

    # ---- HTTP callback server ----

    def _make_handler(self):
        subscriber = self

        class _Handler(BaseHTTPRequestHandler):
            def log_message(self, fmt, *args):  # silence default stderr noise
                pass

            def do_POST(self):
                if self.path != "/fault_event":
                    self.send_error(404)
                    return
                try:
                    length = int(self.headers.get("Content-Length", 0))
                    body = self.rfile.read(length) if length else b"{}"
                    event = json.loads(body.decode("utf-8") or "{}")
                except (ValueError, json.JSONDecodeError) as exc:
                    self.send_error(400, f"bad json: {exc}")
                    return
                try:
                    subscriber._handle_event(event)
                except Exception as exc:  # never let a handler crash the server
                    print(f"[faultsub] handler error: {exc}")
                self.send_response(200)
                self.end_headers()

        return _Handler

    # ---- subscription lifecycle ----

    def _rest_url(self, path: str) -> str:
        return (
            f"http://{self.cfg.catmonitor_host}:{self.cfg.catmonitor_rest_port}{path}"
        )

    def _register(self) -> Optional[str]:
        body = {
            "types": list(self.cfg.fault_types),
            "components": ["npu"],
            "npu_ids": [str(n) for n in self.cfg.npu_ids],
            "delivery": "webhook",
            "endpoint": self.cfg.advertise_url,
            "debounce_ms": self.cfg.debounce_ms,
            "min_severity": self.cfg.min_severity,
        }
        try:
            resp = requests.post(
                self._rest_url("/faultsub/subscriptions"), json=body, timeout=10
            )
            if resp.status_code != 201:
                print(
                    f"[faultsub] register failed {resp.status_code}: {resp.text[:300]}"
                )
                return None
            return resp.json().get("id")
        except requests.RequestException as exc:
            print(f"[faultsub] register request failed: {exc}")
            return None

    def _unregister(self) -> None:
        if not self._subscription_id:
            return
        try:
            requests.delete(
                self._rest_url(f"/faultsub/subscriptions/{self._subscription_id}"),
                timeout=10,
            )
            print(f"[faultsub] unregistered {self._subscription_id}")
        except requests.RequestException:
            pass

    # ---- public lifecycle ----

    def start(self, *, block: bool = False) -> None:
        self._server = ThreadingHTTPServer(
            (self.cfg.callback_host, self.cfg.callback_port), self._make_handler()
        )
        self._thread = threading.Thread(
            target=self._server.serve_forever, name="FaultSubServer", daemon=True
        )
        self._thread.start()
        print(
            f"[faultsub] listening {self.cfg.callback_host}:{self.cfg.callback_port} "
            f"({self.cfg.advertise_url})"
        )

        # Retry registration: CATMonitor may start after this process.
        for _ in range(30):
            sid = self._register()
            if sid:
                self._subscription_id = sid
                print(f"[faultsub] subscribed as {sid}")
                break
            time.sleep(2)
        if not self._subscription_id:
            print("[faultsub] WARNING: not registered with CATMonitor")

        if block:
            try:
                while True:
                    time.sleep(3600)
            except KeyboardInterrupt:
                self.stop()

    def stop(self) -> None:
        self._unregister()
        if self._server:
            self._server.shutdown()
            self._server.server_close()
        if self._thread:
            self._thread.join(timeout=5)
        print("[faultsub] stopped")


def build_npu_to_dp(npu_ids: List[int]) -> Dict[int, int]:
    """Map each NPU id (by position) to its DP rank, mirroring the original
    demo's ``dp_to_npu`` / ``npu_to_dp`` construction."""
    return {npu_id: rank for rank, npu_id in enumerate(npu_ids)}
