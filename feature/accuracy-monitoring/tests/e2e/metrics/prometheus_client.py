from __future__ import annotations

import re
import time
import urllib.request


_NAME_RE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})?$")
_LINE_RE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(.+)$")
_LABEL_RE = re.compile(r'(\w+)="([^"]*)"')


class PrometheusClient:
    def __init__(self, metrics_url: str):
        self._url = metrics_url

    def _fetch(self) -> str:
        with urllib.request.urlopen(self._url, timeout=10) as r:
            return r.read().decode("utf-8")

    @staticmethod
    def _parse_spec(metric: str):
        m = _NAME_RE.match(metric.strip())
        if not m:
            raise ValueError(f"invalid metric spec: {metric}")
        name = m.group(1)
        labels = {}
        if m.group(3):
            for pair in _LABEL_RE.finditer(m.group(3)):
                labels[pair.group(1)] = pair.group(2)
        return name, labels

    @staticmethod
    def _labels_match(want: dict, got: dict) -> bool:
        for k, v in want.items():
            if got.get(k) != v:
                return False
        return True

    def _query(self, metric: str, cast) -> float:
        name, labels = self._parse_spec(metric)
        text = self._fetch()
        total = 0.0
        for line in text.splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            m = _LINE_RE.match(line)
            if not m or m.group(1) != name:
                continue
            line_labels = {}
            if m.group(2):
                for pair in _LABEL_RE.finditer(m.group(2)[1:-1]):
                    line_labels[pair.group(1)] = pair.group(2)
            if not self._labels_match(labels, line_labels):
                continue
            try:
                total += cast(m.group(3).strip().split()[0])
            except (ValueError, IndexError):
                continue
        return total

    def get_counter(self, metric: str) -> int:
        return int(self._query(metric, float))

    def get_gauge(self, metric: str) -> float:
        return self._query(metric, float)

    def _get_value(self, metric: str) -> float:
        return self._query(metric, float)

    def wait_for(self, metric: str, predicate, timeout: float = 10.0) -> None:
        deadline = time.time() + timeout
        last_val = None
        while time.time() < deadline:
            try:
                last_val = self._get_value(metric)
            except Exception:
                last_val = None
            if last_val is not None and predicate(last_val):
                return
            time.sleep(0.5)
        raise AssertionError(
            f"timeout waiting for {metric}: last value={last_val}"
        )

    def snapshot(self) -> str:
        return self._fetch()
