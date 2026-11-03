from __future__ import annotations

from openai import OpenAI


class OpenAISdkClient:
    def __init__(self, base_url: str, served_name: str):
        # OpenAI SDK 会在 base_url 后拼接 /chat/completions；
        # 传入的 base_url 不含 /v1 时补全，否则请求打到 /chat/completions 返回 404。
        base = (base_url or "").rstrip("/")
        if base and not base.endswith("/v1"):
            base += "/v1"
        self._client = OpenAI(base_url=base, api_key="e2e-test")
        self._served_name = served_name

    def chat(self, messages: list, **kwargs) -> dict:
        resp = self._client.chat.completions.create(
            model=self._served_name, messages=messages, **kwargs
        )
        return resp.model_dump()

    def chat_stream(self, messages: list, **kwargs) -> list:
        chunks = []
        stream = self._client.chat.completions.create(
            model=self._served_name, messages=messages, stream=True, **kwargs
        )
        for chunk in stream:
            chunks.append(chunk.model_dump())
        return chunks
