from __future__ import annotations

import os
import shutil
import tempfile

import pytest

pytestmark = pytest.mark.asyncio


@pytest.fixture
def detector_missing_service(vllm_service_factory, model_yaml):
    workspace = tempfile.mkdtemp(prefix="e2e_no_detector_")
    try:
        mw_src = os.path.normpath(
            os.path.join(
                os.path.dirname(__file__), "..", "..", "..", "anomaly_middleware"
            )
        )
        shutil.copytree(mw_src, os.path.join(workspace, "anomaly_middleware"))

        service = vllm_service_factory(
            {
                "model": model_yaml,
                "middleware": True,
                "env": {},
                "with_injector": False,
                "expect_fail": True,
                "fail_fast_timeout": float(
                    model_yaml.get("startup_timeout_sec", 600)
                ),
                "workspace": workspace,
            }
        )
        yield service
    finally:
        shutil.rmtree(workspace, ignore_errors=True)


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.fail_fast
async def test_detector_yaml_missing(detector_missing_service):
    assert detector_missing_service.proc is not None
    assert detector_missing_service.proc.returncode is not None
    assert detector_missing_service.proc.returncode != 0
