from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P1
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.fail_fast
async def test_detector_workers_invalid(vllm_service_factory, model_yaml):
    service = vllm_service_factory({
        "model": model_yaml,
        "middleware": True,
        "env": {"VLLM_ANOMALY_DETECTOR_WORKERS": "0"},
        "with_injector": False,
        "expect_fail": True,
        "fail_fast_timeout": float(model_yaml.get("startup_timeout_sec", 600)),
    })
    assert service.proc is not None
    assert service.proc.poll() is not None
