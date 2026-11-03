"""包裹 DetectorRunner.run_async / SSE 处理器 / 抽取函数，使其可经 sidecar 注错。
不修改源码，仅在运行期类对象上替换方法。"""
import json
import urllib.request

import numpy as np


def _inject_once(injector_url, kind):
    """向 sidecar 请求一次注入；命中返回 payload dict，否则返回 None。"""
    try:
        req = urllib.request.Request(
            f"{injector_url}/inject",
            data=json.dumps({"kind": kind}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=2) as r:
            if r.status != 200:
                return None
            return json.loads(r.read())
    except Exception:
        return None


def patch_detector_runner(DetectorRunner, injector_url):
    real_run_async = DetectorRunner.run_async

    async def _wrapped_run_async(self, logprobs_list, token_ids_list):
        payload = _inject_once(injector_url, "run_async")
        if payload is None:
            return await real_run_async(self, logprobs_list, token_ids_list)

        lp = [np.array(c, dtype=np.float32) for c in payload["logprobs"]]
        ti = [np.array(c, dtype=np.int32) for c in payload["token_ids"]]
        return await real_run_async(self, lp, ti)

    DetectorRunner.run_async = _wrapped_run_async


def patch_sse_processor(SSEStreamProcessor, injector_url):
    """keep-alive/注释/retry 事件注入：在首个 feed 前预置注入字节，验证原样透传。"""
    real_feed = SSEStreamProcessor.feed

    def _wrapped_feed(self, chunk):
        if not getattr(self, "_e2e_ka_injected", False):
            self._e2e_ka_injected = True
            payload = _inject_once(injector_url, "sse_keepalive")
            if payload is not None:
                prefix = payload.get("bytes", "").encode()
                if prefix:
                    chunk = prefix + chunk
        return real_feed(self, chunk)

    SSEStreamProcessor.feed = _wrapped_feed


def patch_extractors(extractor, middleware, injector_url):
    """抽取函数注空：命中时返回空数组（模拟空响应），验证空数据不检测。"""
    real_chat = extractor.extract_chat_response
    real_comp = extractor.extract_completions_response

    def _wrap(real):
        def wrapped(data, n_detect):
            payload = _inject_once(injector_url, "extract_empty")
            if payload is not None:
                return []
            return real(data, n_detect)

        return wrapped

    wrapped_chat = _wrap(real_chat)
    wrapped_comp = _wrap(real_comp)
    extractor.extract_chat_response = wrapped_chat
    extractor.extract_completions_response = wrapped_comp
    # middleware 以 from .extractor import ... 绑定引用，需同步替换模块内名称
    middleware.extract_chat_response = wrapped_chat
    middleware.extract_completions_response = wrapped_comp
