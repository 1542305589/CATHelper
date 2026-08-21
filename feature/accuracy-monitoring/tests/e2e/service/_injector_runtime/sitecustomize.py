"""运行期注入钩子。仅在 vLLM 子进程启动、且 VLLM_ANOMALY_E2E_INJECTOR 已设时生效。
生产环境永不设此 env，永不加载此文件 → 零生产影响。"""
import os
import sys

_INJECTOR_URL = os.environ.get("VLLM_ANOMALY_E2E_INJECTOR")

if _INJECTOR_URL:
    try:
        sys.path.insert(0, os.path.dirname(__file__))
        from _injector_patch import (
            patch_detector_runner,
            patch_extractors,
            patch_sse_processor,
        )
        from anomaly_middleware import extractor, middleware
        from anomaly_middleware.detector_runner import DetectorRunner
        if not getattr(DetectorRunner, "_e2e_patched", False):
            patch_detector_runner(DetectorRunner, _INJECTOR_URL)
            DetectorRunner._e2e_patched = True
        if not getattr(extractor, "_e2e_sse_patched", False):
            patch_sse_processor(extractor.SSEStreamProcessor, _INJECTOR_URL)
            extractor._e2e_sse_patched = True
        if not getattr(extractor, "_e2e_extract_patched", False):
            patch_extractors(extractor, middleware, _INJECTOR_URL)
            extractor._e2e_extract_patched = True
    except Exception as e:
        print(f"[sitecustomize] injector patch failed: {e}", file=sys.stderr)
