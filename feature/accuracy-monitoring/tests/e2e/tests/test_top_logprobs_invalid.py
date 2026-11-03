from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.fail_fast
@pytest.mark.parametrize("bad_topk", [0, 21])
async def test_top_logprobs_invalid(vllm_service_factory, model_yaml, bad_topk):
    # TC-026 前置：配置 VLLM_ANOMALY_TOP_LOGPROBS=0 与 =21（非法区间 1-20 之外）。
    # PluginConfig.from_env() 在中间件初始化时校验（env.py: 1<=top_logprobs<=20），
    # 非法值 → 服务启动即中断（fail-fast），根本到不了推理阶段。
    service = vllm_service_factory({
        "model": model_yaml,
        "middleware": True,
        "env": {"VLLM_ANOMALY_TOP_LOGPROBS": str(bad_topk)},
        "with_injector": False,
        "expect_fail": True,
        "fail_fast_timeout": float(model_yaml.get("startup_timeout_sec", 600)),
    })
    assert service.proc is not None
    assert service.proc.poll() is not None