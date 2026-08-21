from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio


@pytest.mark.P2
@pytest.mark.full
@pytest.mark.nightly
@pytest.mark.inject
async def test_multi_choice_n3(
    vllm_service_b, injector, http_client, metrics_client, served_name, anomaly_data,
):
    if not injector.health_check():
        pytest.skip("injector infrastructure not available")

    # 3 个 choice：choice0/2 正常、choice1 生僻字异常（n=3 独立上报，不覆盖）
    rare_lp = anomaly_data["rare_character"]["logprobs"][0]
    rare_ti = anomaly_data["rare_character"]["token_ids"][0]
    normal_lp = [[-0.1 - j * 0.001 for j in range(20)] for _ in range(3)]
    normal_ti = [[1 + j for j in range(20)] for _ in range(3)]
    payload = {
        "logprobs": [normal_lp, rare_lp, normal_lp],
        "token_ids": [normal_ti, rare_ti, normal_ti],
    }
    injector.set_override("run_async", payload, count=1)

    before = metrics_client.get_counter("vllm_anomaly_requests_total")
    before_c1 = metrics_client.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="1"}}'
    )
    before_c0 = metrics_client.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="0"}}'
    )
    before_c2 = metrics_client.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="2"}}'
    )

    resp = await http_client.chat(
        messages=[{"role": "user", "content": "Hello"}],
        n=3,
    )
    assert resp.status_code == 200
    body = resp.json()
    assert len(body["choices"]) == 3

    # requests_total 按请求计数（+1，非按候选）
    metrics_client.wait_for(
        "vllm_anomaly_requests_total",
        lambda v: v - before >= 1,
        timeout=15.0,
    )
    after = metrics_client.get_counter("vllm_anomaly_requests_total")
    assert after - before == 1

    # 异常候选按 choice_index="1" 独立计数，choice 0/2 不覆盖
    metrics_client.wait_for(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="1"}}',
        lambda v: v - before_c1 >= 1,
        timeout=15.0,
    )
    after_c1 = metrics_client.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="1"}}'
    )
    assert after_c1 - before_c1 == 1
    after_c0 = metrics_client.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="0"}}'
    )
    assert after_c0 - before_c0 == 0
    after_c2 = metrics_client.get_counter(
        f'vllm_anomaly_detected_total{{ill_type="1",model="{served_name}",choice_index="2"}}'
    )
    assert after_c2 - before_c2 == 0

    gauge = metrics_client.get_gauge(
        f'vllm_anomaly_last_rare_character{{model="{served_name}"}}'
    )
    assert gauge == 1