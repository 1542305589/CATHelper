from __future__ import annotations

import json
from typing import AsyncIterator

import httpx


class HttpClient:
    def __init__(self, base_url: str, served_name: str):
        self._client = httpx.AsyncClient(base_url=base_url, timeout=120.0)
        self._served_name = served_name
        self._last_request = None
        self._last_response = None

    async def chat(self, messages: list, **kwargs) -> httpx.Response:
        body = {"model": self._served_name, "messages": messages, **kwargs}
        self._last_request = body
        resp = await self._client.post("/v1/chat/completions", json=body)
        self._last_response = self._maybe_json(resp)
        return resp

    async def completions(self, prompt: str, **kwargs) -> httpx.Response:
        body = {"model": self._served_name, "prompt": prompt, **kwargs}
        self._last_request = body
        resp = await self._client.post("/v1/completions", json=body)
        self._last_response = self._maybe_json(resp)
        return resp

    async def chat_stream(self, messages: list, **kwargs) -> AsyncIterator[dict]:
        body = {"model": self._served_name, "messages": messages, "stream": True, **kwargs}
        self._last_request = body
        self._last_response = None
        async with self._client.stream("POST", "/v1/chat/completions", json=body) as resp:
            async for event in self._iter_sse(resp):
                self._last_response = event
                yield event

    async def chat_stream_raw(self, messages: list, **kwargs) -> AsyncIterator[bytes]:
        """流式原始字节（含 keep-alive/注释/retry 行），用于透传验证。"""
        body = {"model": self._served_name, "messages": messages, "stream": True, **kwargs}
        self._last_request = body
        self._last_response = None
        async with self._client.stream("POST", "/v1/chat/completions", json=body) as resp:
            async for chunk in resp.aiter_bytes():
                self._last_response = {"raw_chunk": chunk.decode("utf-8", errors="replace")}
                yield chunk

    async def completions_stream(self, prompt: str, **kwargs) -> AsyncIterator[dict]:
        body = {"model": self._served_name, "prompt": prompt, "stream": True, **kwargs}
        self._last_request = body
        self._last_response = None
        async with self._client.stream("POST", "/v1/completions", json=body) as resp:
            async for event in self._iter_sse(resp):
                self._last_response = event
                yield event

    async def get(self, path: str) -> httpx.Response:
        resp = await self._client.get(path)
        self._last_response = self._maybe_json(resp)
        return resp

    async def post_raw(self, path: str, content: bytes, headers: dict) -> httpx.Response:
        self._last_request = {"raw_content": content, "headers": headers}
        resp = await self._client.post(path, content=content, headers=headers)
        self._last_response = self._maybe_json(resp)
        return resp

    @property
    def last_request(self) -> dict:
        return self._last_request

    @property
    def last_response(self) -> dict:
        return self._last_response

    async def aclose(self) -> None:
        await self._client.aclose()

    @staticmethod
    def _maybe_json(resp: httpx.Response):
        try:
            return resp.json()
        except Exception:
            return {"status_code": resp.status_code, "text": resp.text}

    @staticmethod
    async def _iter_sse(resp: httpx.Response) -> AsyncIterator[dict]:
        buffer = b""
        async for chunk in resp.aiter_bytes():
            buffer += chunk
            while b"\n\n" in buffer:
                block, buffer = buffer.split(b"\n\n", 1)
                for line in block.split(b"\n"):
                    if not line.startswith(b"data: "):
                        continue
                    data = line[len(b"data: "):].strip()
                    if data == b"[DONE]":
                        yield {"done": True}
                    else:
                        try:
                            yield json.loads(data)
                        except json.JSONDecodeError:
                            yield {"raw": data.decode("utf-8", errors="replace")}
