from __future__ import annotations

import json
import os

from .compare import normalize_response

CHAT_VARIANTS = {
    "v1": {},
    "v2": {"logprobs": True, "top_logprobs": 5},
    "v3": {"logprobs": True, "top_logprobs": 5, "return_tokens_as_token_ids": True},
}

COMPLETIONS_VARIANTS = {
    "v1": {},
    "v2": {"logprobs": 5},
    "v3": {"logprobs": 5, "return_tokens_as_token_ids": True},
}

_INTERFACES = ("chat_nonstream", "chat_stream", "completions_nonstream", "completions_stream")


def _normalize_event(event: dict) -> dict:
    if event.get("done"):
        return event
    event = dict(event)
    event.pop("id", None)
    event.pop("created", None)
    return event


class BaselineStore:
    """File-based baselines captured on the no-mw service and later compared
    against the middleware service (design §3.4, TC-001 12-group spec).

    Each interface (chat/completions x stream/non-stream) stores three
    request variants (v1/v2/v3) as separate files.
    """

    def __init__(self, dir_path: str):
        self._dir = dir_path

    @classmethod
    def for_run(cls, report_dir: str) -> "BaselineStore":
        return cls(os.path.join(report_dir, "baseline"))

    @property
    def dir(self) -> str:
        return self._dir

    def store_chat_nonstream(self, variant: str, payload: dict) -> None:
        self._write("chat_nonstream", variant, normalize_response(payload))

    def store_chat_stream(self, variant: str, events: list) -> None:
        self._write("chat_stream", variant, [_normalize_event(e) for e in events])

    def store_completions_nonstream(self, variant: str, payload: dict) -> None:
        self._write("completions_nonstream", variant, normalize_response(payload))

    def store_completions_stream(self, variant: str, events: list) -> None:
        self._write("completions_stream", variant, [_normalize_event(e) for e in events])

    def load_chat_nonstream(self, variant: str) -> dict:
        return self._read("chat_nonstream", variant)

    def load_chat_stream(self, variant: str) -> list:
        return self._read("chat_stream", variant)

    def load_completions_nonstream(self, variant: str) -> dict:
        return self._read("completions_nonstream", variant)

    def load_completions_stream(self, variant: str) -> list:
        return self._read("completions_stream", variant)

    def _path(self, interface: str, variant: str) -> str:
        return os.path.join(self._dir, f"{interface}_{variant}.json")

    def _write(self, interface: str, variant: str, data) -> None:
        os.makedirs(self._dir, exist_ok=True)
        with open(self._path(interface, variant), "w", encoding="utf-8") as f:
            json.dump(data, f, ensure_ascii=False, indent=2)

    def _read(self, interface: str, variant: str):
        path = self._path(interface, variant)
        if not os.path.exists(path):
            raise FileNotFoundError(
                f"baseline missing: {path} (run test_baseline_collection first)"
            )
        with open(path, encoding="utf-8") as f:
            return json.load(f)
