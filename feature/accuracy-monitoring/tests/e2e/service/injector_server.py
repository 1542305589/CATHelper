from __future__ import annotations

import json
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer


class InjectorServer:
    def __init__(self, host: str = "127.0.0.1", port: int = 9999):
        self.host = host
        self._port = port
        self._lock = threading.Lock()
        self._queue: dict[str, list[tuple[dict, int]]] = {}
        self._used_count = 0
        self._server: HTTPServer | None = None
        self._thread: threading.Thread | None = None

    @property
    def port(self) -> int:
        return self._port

    @property
    def url(self) -> str:
        return f"http://{self.host}:{self._port}"

    def set_override(self, kind: str, payload: dict, count: int = 1) -> None:
        with self._lock:
            self._queue.setdefault(kind, []).append((payload, count))

    def clear(self) -> None:
        with self._lock:
            self._queue.clear()

    def get_state(self) -> dict:
        with self._lock:
            queue_snapshot = {
                kind: [{"payload": p, "remaining": c} for p, c in items]
                for kind, items in self._queue.items()
            }
            return {"queue": queue_snapshot, "used_count": self._used_count}

    def health_check(self) -> bool:
        try:
            with urllib.request.urlopen(f"{self.url}/health", timeout=2) as r:
                return r.status == 200
        except Exception:
            return False

    def _handle_inject(self, body: dict) -> tuple[int, dict | None]:
        with self._lock:
            self._used_count += 1
            kind = body.get("kind")
            q = self._queue.get(kind, [])
            for i, (payload, cnt) in enumerate(q):
                if cnt > 0:
                    if cnt - 1 <= 0:
                        q.pop(i)
                    else:
                        q[i] = (payload, cnt - 1)
                    return 200, payload
            return 404, None

    def start(self) -> None:
        server = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: object) -> None:
                pass

            def _send(self, status: int, body: dict | None = None) -> None:
                data = b"" if body is None else json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                if data:
                    self.wfile.write(data)

            def do_GET(self) -> None:
                if self.path == "/health":
                    self._send(200, {"status": "ok"})
                elif self.path == "/state":
                    self._send(200, server.get_state())
                else:
                    self._send(404)

            def do_POST(self) -> None:
                length = int(self.headers.get("Content-Length", 0) or 0)
                raw = self.rfile.read(length) if length else b""
                try:
                    body = json.loads(raw) if raw else {}
                except Exception:
                    self._send(400)
                    return
                if self.path == "/inject":
                    status, payload = server._handle_inject(body)
                    if status == 200:
                        self._send(200, payload)
                    else:
                        self._send(404)
                elif self.path == "/override":
                    server.set_override(
                        body.get("kind"),
                        body.get("payload", {}),
                        int(body.get("count", 1)),
                    )
                    self._send(200, {"status": "ok"})
                elif self.path == "/clear":
                    server.clear()
                    self._send(200, {"status": "ok"})
                else:
                    self._send(404)

        self._server = HTTPServer((self.host, self._port), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._server = None
        if self._thread is not None:
            self._thread.join(timeout=5)
            self._thread = None
