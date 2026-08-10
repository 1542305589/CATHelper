#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# SPDX-FileCopyrightText: Copyright contributors to the CATHelper project
"""Unit tests for the CATMonitor fault subscriber (Phase C).

Run: python3 -m unittest test_catmonitor_fault_sub -v

Covers the pure logic: NPU->DP mapping, fault-type parsing, and event
handling with a mocked vLLM REST API (no real vLLM / DCMI / CATMonitor
needed). The HTTP webhook round-trip is exercised end-to-end with a mock
CATMonitor REST server and a mock vLLM.
"""

import json
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

from catmonitor_fault_sub import (
    CatMonitorFaultSubscriber,
    FaultSubscriberConfig,
    build_npu_to_dp_ranks,
    parse_fault_types,
    parse_npu_ids,
)


class TestParsing(unittest.TestCase):
    def test_build_npu_to_dp_ranks_single_device_per_die(self):
        m = build_npu_to_dp_ranks([0, 1, 2, 3], 1, [0, 1, 2, 3], 4)
        self.assertEqual(m, {0: [0], 1: [1], 2: [2], 3: [3]})

    def test_build_npu_to_dp_ranks_dual_die_a3(self):
        # A3: 8 DIE x 2 physical cards = 16 cards; DIE 5 hosts cards 10, 11.
        m = build_npu_to_dp_ranks(
            list(range(8)), 2, list(range(16)), 16
        )
        self.assertEqual(m[5], [10, 11])
        self.assertEqual(m[0], [0, 1])
        self.assertEqual(m[7], [14, 15])
        self.assertEqual(len(m), 8)

    def test_build_npu_to_dp_ranks_filters_unused_dies(self):
        # dp-size=8 with visible 0-15 -> only physical cards 0-7 are used,
        # i.e. DIE 0-3; DIE 4-7 get no ranks.
        m = build_npu_to_dp_ranks(list(range(8)), 2, list(range(16)), 8)
        self.assertEqual(m, {0: [0, 1], 1: [2, 3], 2: [4, 5], 3: [6, 7]})

    def test_build_npu_to_dp_ranks_respects_npu_ids_scope(self):
        m = build_npu_to_dp_ranks([5], 2, list(range(16)), 16)
        self.assertEqual(m, {5: [10, 11]})

    def test_build_npu_to_dp_ranks_dp_size_out_of_range(self):
        with self.assertRaises(ValueError):
            build_npu_to_dp_ranks([0], 2, list(range(16)), 20)

    def test_parse_npu_ids_range(self):
        self.assertEqual(parse_npu_ids("0-3"), [0, 1, 2, 3])

    def test_parse_npu_ids_list(self):
        self.assertEqual(parse_npu_ids("0,1,5"), [0, 1, 5])

    def test_parse_fault_types_ok(self):
        self.assertEqual(
            parse_fault_types("card_drop,hbm_uce"),
            ["card_drop", "hbm_uce"],
        )

    def test_parse_fault_types_unknown_exits(self):
        with self.assertRaises(SystemExit):
            parse_fault_types("bogus_type")


class TestEventHandler(unittest.TestCase):
    def _make_subscriber(self):
        cfg = FaultSubscriberConfig(
            vllm_host="localhost",
            vllm_port=8006,
            catmonitor_host="localhost",
            catmonitor_rest_port=9101,
            callback_host="127.0.0.1",
            callback_port=0,  # unused; we call _handle_event directly
            advertise_url="http://127.0.0.1:9102/fault_event",
            npu_ids=[0, 1, 2, 3],
        )
        npu_to_dp = build_npu_to_dp_ranks([0, 1, 2, 3], 1, [0, 1, 2, 3], 4)
        return CatMonitorFaultSubscriber(cfg, npu_to_dp)

    def test_card_drop_triggers_scale_down_after_pause(self):
        sub = self._make_subscriber()
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True) as wait_pause,
        ):
            sub._handle_event(
                {"type": "card_drop", "npu_id": "2", "recovered": False}
            )
            wait_pause.assert_called_once_with([2])
            instructions = [c.args[0] for c in apply.call_args_list]
            self.assertEqual(instructions, ["scale_down"])

    def test_unknown_npu_skipped(self):
        sub = self._make_subscriber()
        with mock.patch.object(sub, "_vllm_apply") as apply:
            sub._handle_event(
                {"type": "card_drop", "npu_id": "99", "recovered": False}
            )
            apply.assert_not_called()

    def test_recovery_triggers_retry(self):
        sub = self._make_subscriber()
        with mock.patch.object(sub, "_vllm_apply", return_value=True) as apply:
            # Track the fault first (non-recovered event), then recovery.
            sub._handle_event(
                {"type": "roce_link_down", "npu_id": "1", "recovered": False}
            )
            sub._handle_event(
                {"type": "roce_link_down", "npu_id": "1", "recovered": True}
            )
            apply.assert_called_once()
            self.assertEqual(apply.call_args.args[0], "retry")

    def test_recovery_untracked_benign_fault_skips_retry(self):
        # Benign npu_error_code is ignored (never tracked); its recovery must
        # not send retry to healthy engines.
        sub = self._make_subscriber()
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True) as wait_pause,
        ):
            sub._handle_event(
                {
                    "type": "npu_error_code",
                    "npu_id": "1",
                    "recovered": False,
                    "detail": {"error_codes": "0x80f38003"},
                }
            )
            sub._handle_event(
                {"type": "npu_error_code", "npu_id": "1", "recovered": True, "detail": {}}
            )
            apply.assert_not_called()
            wait_pause.assert_not_called()

    def test_recovery_of_acted_npu_error_code_sends_retry(self):
        sub = self._make_subscriber()
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True) as wait_pause,
        ):
            sub._handle_event(
                {
                    "type": "npu_error_code",
                    "npu_id": "1",
                    "recovered": False,
                    "detail": {"error_codes": "0x12345678"},
                }
            )
            sub._handle_event(
                {"type": "npu_error_code", "npu_id": "1", "recovered": True, "detail": {}}
            )
            instructions = [c.args[0] for c in apply.call_args_list]
            self.assertEqual(instructions, ["scale_down", "retry"])

    def test_npu_error_code_all_benign_ignored(self):
        sub = self._make_subscriber()
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True) as wait_pause,
        ):
            sub._handle_event(
                {
                    "type": "npu_error_code",
                    "npu_id": "1",
                    "recovered": False,
                    "detail": {"error_codes": "0x80f38003"},
                }
            )
            apply.assert_not_called()
            wait_pause.assert_not_called()

    def test_npu_error_code_ignore_is_case_insensitive(self):
        sub = self._make_subscriber()
        with mock.patch.object(sub, "_vllm_apply", return_value=True) as apply:
            sub._handle_event(
                {
                    "type": "npu_error_code",
                    "npu_id": "1",
                    "recovered": False,
                    "detail": {"error_codes": "0X80F38003"},
                }
            )
            apply.assert_not_called()

    def test_npu_error_code_mixed_codes_scale_down(self):
        sub = self._make_subscriber()
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True) as wait_pause,
        ):
            sub._handle_event(
                {
                    "type": "npu_error_code",
                    "npu_id": "1",
                    "recovered": False,
                    "detail": {"error_codes": "0x80f38003,0x12345678"},
                }
            )
            wait_pause.assert_called_once_with([1])
            instructions = [c.args[0] for c in apply.call_args_list]
            self.assertEqual(instructions, ["scale_down"])

    def test_npu_error_code_non_benign_scale_down(self):
        sub = self._make_subscriber()
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True) as wait_pause,
        ):
            sub._handle_event(
                {
                    "type": "npu_error_code",
                    "npu_id": "1",
                    "recovered": False,
                    "detail": {"error_codes": "0x12345678"},
                }
            )
            wait_pause.assert_called_once_with([1])
            instructions = [c.args[0] for c in apply.call_args_list]
            self.assertEqual(instructions, ["scale_down"])

    def test_npu_error_code_empty_ignore_list_scales_down(self):
        cfg = FaultSubscriberConfig(
            vllm_host="localhost",
            vllm_port=8006,
            catmonitor_host="localhost",
            catmonitor_rest_port=9101,
            callback_host="127.0.0.1",
            callback_port=0,
            advertise_url="http://127.0.0.1:9102/fault_event",
            npu_ids=[0, 1, 2, 3],
            ignore_error_codes=[],
        )
        sub = CatMonitorFaultSubscriber(cfg, build_npu_to_dp_ranks([0, 1, 2, 3], 1, [0, 1, 2, 3], 4))
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True) as wait_pause,
        ):
            sub._handle_event(
                {
                    "type": "npu_error_code",
                    "npu_id": "1",
                    "recovered": False,
                    "detail": {"error_codes": "0x80f38003"},
                }
            )
            wait_pause.assert_called_once_with([1])
            self.assertEqual(apply.call_count, 1)

    def test_roce_link_down_not_recovered_log_only(self):
        sub = self._make_subscriber()
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True),
        ):
            sub._handle_event(
                {"type": "roce_link_down", "npu_id": "2", "recovered": False}
            )
            apply.assert_not_called()

    def test_duplicate_persistent_fault_skipped(self):
        sub = self._make_subscriber()
        with (
            mock.patch.object(sub, "_vllm_apply", return_value=True) as apply,
            mock.patch.object(sub, "_wait_for_pause", return_value=True),
        ):
            sub._handle_event(
                {"type": "card_drop", "npu_id": "3", "recovered": False}
            )
            sub._handle_event(
                {"type": "card_drop", "npu_id": "3", "recovered": False}
            )
            # First event does wait_for_pause+scale_down (1 call); duplicate adds 0.
            self.assertEqual(apply.call_count, 1)


class TestWebhookRoundTrip(unittest.TestCase):
    """End-to-end: a fake CATMonitor POSTs an event to the subscriber's
    webhook; the subscriber calls a fake vLLM. Verifies the full plumbing."""

    @classmethod
    def setUpClass(cls):
        # Fake vLLM: records /fault_tolerance/apply calls.
        cls.vllm_calls = []

        class _VLLMHandler(BaseHTTPRequestHandler):
            def log_message(self, fmt, *args):
                pass

            def do_GET(self):
                # _wait_for_pause() polls /fault_tolerance/status; report all
                # DP ranks paused so the flow proceeds to scale_down.
                body = json.dumps(
                    {"dp_ranks": [
                        {"id": 0, "status": "paused"},
                        {"id": 1, "status": "paused"},
                        {"id": 2, "status": "paused"},
                        {"id": 3, "status": "paused"},
                    ]}
                ).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                body = self.rfile.read(length)
                TestWebhookRoundTrip.vllm_calls.append(json.loads(body))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"status":"ok"}')

        cls._vllm = ThreadingHTTPServer(("127.0.0.1", 0), _VLLMHandler)
        cls.vllm_port = cls._vllm.server_address[1]
        threading.Thread(target=cls._vllm.serve_forever, daemon=True).start()

        # Fake CATMonitor REST: just acknowledge subscription creation so the
        # subscriber can register; we then POST the event ourselves.
        class _CatmonHandler(BaseHTTPRequestHandler):
            def log_message(self, fmt, *args):
                pass

            def do_POST(self):
                self.send_response(201)
                self.end_headers()
                self.wfile.write(b'{"id":"sub-test-1"}')

            def do_DELETE(self):
                self.send_response(204)
                self.end_headers()

        cls._catmon = ThreadingHTTPServer(("127.0.0.1", 0), _CatmonHandler)
        cls.catmon_port = cls._catmon.server_address[1]
        threading.Thread(target=cls._catmon.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls._vllm.shutdown()
        cls._catmon.shutdown()

    def test_event_webhook_to_vllm(self):
        cfg = FaultSubscriberConfig(
            vllm_host="127.0.0.1",
            vllm_port=self.vllm_port,
            catmonitor_host="127.0.0.1",
            catmonitor_rest_port=self.catmon_port,
            callback_host="127.0.0.1",
            callback_port=0,
            advertise_url="http://127.0.0.1:1/fault_event",  # port set after start
            npu_ids=[0, 1, 2, 3],
        )
        sub = CatMonitorFaultSubscriber(cfg, build_npu_to_dp_ranks([0, 1, 2, 3], 1, [0, 1, 2, 3], 4))
        sub.start(block=False)
        # Give the HTTP server + registration a moment.
        time.sleep(0.3)
        try:
            cb_port = sub._server.server_address[1]
            # POST a fault event as CATMonitor would.
            import requests

            requests.post(
                f"http://127.0.0.1:{cb_port}/fault_event",
                json={"type": "card_drop", "npu_id": "2", "recovered": False},
                timeout=5,
            )
            time.sleep(0.3)
            # vLLM should have received scale_down after pause completes.
            instructions = [c["instruction"] for c in self.vllm_calls]
            self.assertEqual(instructions, ["scale_down"])
            # The scale_down payload should exclude DP rank 2 (NPU 2 -> rank 2).
            scale_call = next(c for c in self.vllm_calls if c["instruction"] == "scale_down")
            self.assertEqual(scale_call["params"]["exclude_dp_ranks"], [2])
        finally:
            sub.stop()


if __name__ == "__main__":
    unittest.main(verbosity=2)
