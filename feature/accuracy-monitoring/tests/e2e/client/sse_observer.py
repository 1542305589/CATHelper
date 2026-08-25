from __future__ import annotations

import json


class SSEObserver:
    def __init__(self):
        self.events: list = []
        self.raw_chunks: list = []
        self.has_done: bool = False
        self._buffer = b""

    def feed(self, chunk: bytes) -> list:
        self.raw_chunks.append(chunk)
        self._buffer += chunk
        parsed = []
        while b"\n\n" in self._buffer:
            block, self._buffer = self._buffer.split(b"\n\n", 1)
            for line in block.split(b"\n"):
                if not line.startswith(b"data: "):
                    continue
                data = line[len(b"data: "):].strip()
                if data == b"[DONE]":
                    self.has_done = True
                    parsed.append({"done": True})
                else:
                    try:
                        evt = json.loads(data)
                    except json.JSONDecodeError:
                        evt = {"raw": data.decode("utf-8", errors="replace")}
                    self.events.append(evt)
                    parsed.append(evt)
        return parsed

    def assert_has_done(self) -> None:
        if not self.has_done:
            raise AssertionError("SSE stream did not end with data: [DONE]")
