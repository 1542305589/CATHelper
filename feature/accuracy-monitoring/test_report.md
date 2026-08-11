# accuracy-monitoring 测试报告

> **项目**: accuracy-monitoring — 推理精度异常检测工具
> **版本**: v0.1.0
> **日期**: 2026-08-11
> **测试执行**: OpenCode + Pytest（离线单测 + Mock E2E + 真实 vLLM 服务器 E2E）
> **部署模型**: Qwen3-0.6B（Atlas 910B4 × 1）

---

## 1. 测试概述

### 1.1 测试目标

依据 `docs/spec.md`（§2.1–§2.15 功能需求 + §3 契约 + §4 行为不变量 + §5 验收标准）与
`docs/design.md`，验证中间件的完整性与正确性：

- **请求拦截**：仅拦截 `/v1/chat/completions`、`/v1/completions`，其余路径/方法/非 HTTP 原样透传
- **强制采集**：强制注入 `logprobs`/`top_logprobs`/`return_tokens_as_token_ids` 并修正 Content-Length
- **客户端透明恢复**：`logprobs=null`/截断/文本还原 三级兜底，未设 `return_tokens_as_token_ids` 时**绝不泄漏 `token_id:`**
- **流式安全转发**：SSE 增量转发、跨块事件重组、`[DONE]`/keep-alive 透传、检测数据跨块累积
- **异常检测算法**：生僻字(1)/乱码(2)/重复(3)/NaN(4) 四类异常可检出，正常样本零误报
- **检测调度**：fire-and-forget、失败隔离计 error、串行化、空响应跳过、多候选不覆盖
- **监控概率采样**：0.0 透传 / 1.0 全检 / 0.3 概率注入
- **关联标识**：`x-anomaly-request-id` 响应头唯一
- **Prometheus 指标**：独立 registry + 按 `ill_type`/`model`/`choice_index` 上报 + 四 gauge
- **优雅降级**：配置非法/检测器不可用/路径缺失 → 永久透传 + 指标报零 + 日志事件
- **tokenizer 获取链**：env → argv(--tokenizer/--model) → model_hint → /v1/models → HF 缓存扫描
- **真实部署**：`vllm serve Qwen3-0.6B --middleware anomaly_middleware.AnomalyMiddleware` 全流程验证

### 1.2 测试结果汇总

| 指标 | 结果 |
|------|------|
| 测试总数 | **220** |
| 通过 | **220** |
| 失败 | **0** |
| 通过率 | **100%** |
| 单元测试 | **171** |
| Mock E2E（in-process ASGI） | **37** |
| 真实服务器 E2E（Qwen3-0.6B） | **12** |
| 检测错误（live 累计 44 次检测） | **0** |
| 发现 Bug | **0** |

---

## 2. 测试环境

| 项目 | 配置 |
|------|------|
| 操作系统 | Linux (aarch64) |
| NPU | Huawei Atlas 910B4 × 2（npu-smi 25.5.0.b060，device 2/5 可用） |
| Python | 3.11.15 |
| vLLM | 0.20.2（源码 editable：`/vllm-workspace/vllm`）+ vLLM-Ascend 0.20.2rc1 |
| torch / torch_npu | 2.10.0+cpu / 2.10.0 |
| transformers | 5.5.3 |
| numpy / PyYAML / prometheus_client / httpx / pytest | 1.26.4 / 6.0.3 / 0.25.0 / 0.28.1 / 9.1.1 |
| 测试模型 | Qwen3-0.6B（`/home/gyl/models/Qwen3-0.6B`，vocab_size=151643，max_model_len=40960） |
| 服务端口 | 8008（`run_server.sh`：`--middleware anomaly_middleware.AnomalyMiddleware`） |
| 服务启动 | `bash /vllm-workspace/vllm/run_server.sh`（ASCEND_RT_VISIBLE_DEVICES=0, tp=1） |
| root 权限 | 是 |

---

## 3. 功能点覆盖矩阵（spec §2 → 测试用例）

| spec 章节 | 功能点 | 测试覆盖 | 用例数 |
|-----------|--------|----------|:------:|
| §2.1 请求拦截 | 非目标路径/方法透传、非 HTTP 透传、GET /v1/models、GET chat | `test_middleware_helpers.py` + `test_e2e_sampling_metrics_degrade.py` | 5 |
| §2.2 强制采集 | chat/completions 注入、max(客户端,N)、Content-Length 修正 | `test_extractor.py` + `test_e2e_chat.py` + `test_e2e_completions.py` | 11 |
| §2.3 响应恢复 | null/截断/文本还原/保留 token_id/n>1 循环 | `test_extractor.py` + `test_e2e_chat.py` + `test_e2e_completions.py` + `test_e2e_live.py` | 20 |
| §2.4 流式 | 增量转发、跨块重组、[DONE]/keep-alive/CRLF、无 DONE flush、非 JSON 透传、多 data 行、附加字段、多分块 | `test_sse.py` + `test_e2e_streaming.py` + `test_e2e_live.py` | 27 |
| §2.5 异常检测 | 四类异常检出、空响应跳过、n>1 不覆盖、长序列重复 | `test_detector_illtypes.py` + `test_e2e_detection.py` | 10 |
| §2.6 失败隔离 | 检测异常不影响客户端、计 error、错误响应状态/body 保留 | `test_detector_runner.py` + `test_e2e_sampling_metrics_degrade.py` + `test_middleware_helpers.py` | 5 |
| §2.7 串行化 | 单 worker 串行、并发 5 请求全部检测 | `test_detector_runner.py` + `test_e2e_detection.py` | 3 |
| §2.8 监控概率 | 0.0/1.0/0.3 概率注入 | `test_e2e_sampling_metrics_degrade.py` | 3 |
| §2.9 关联标识 | 响应头唯一（Mock + live） | `test_e2e_chat.py` + `test_e2e_live.py` | 2 |
| §2.10 指标 | 200 + content-type、独立 registry、choice_index、四 gauge、下游无路由 | `test_metrics.py` + `test_e2e_sampling_metrics_degrade.py` + `test_e2e_live.py` | 12 |
| §2.11 env 配置 | 默认值、覆盖、非法值降级、边界回退、tokenizer_model | `test_config.py` + `test_middleware_preheat.py` | 14 |
| §2.12 配置路径 | 存在/缺失 → 降级 | `test_config.py` + `test_e2e_sampling_metrics_degrade.py` | 3 |
| §2.13 优雅降级 | 检测器不可用 → 永久透传 + 指标报零 | `test_e2e_sampling_metrics_degrade.py` + `test_middleware_helpers.py` + `test_detector_runner.py` | 4 |
| §2.14 插件部署 | `--middleware` 单参构造 | `test_e2e_live.py`（真实部署） | 2 |
| §2.15 TokenTextResolver | resolve/缓存、7 级获取链、降级分流、trust_remote_code、argv 解析 | `test_token_resolver.py` + `test_token_categorizer.py` + `test_middleware_preheat.py` | 38 |
| §3 契约 | 请求体聚合/重放、scope 拷贝、响应 start/body 处理、终端幂等 | `test_middleware_helpers.py` | 8 |

> 覆盖结论：**spec.md 全部 16 个功能需求章节均有对应测试**，覆盖检测算法输出路径（四类异常）、
> 真实服务器 E2E、错误状态透传、SSE 多行/多分块、completions n>1、并发检测等场景。

---

## 4. 单元测试（离线，共 171）

### 4.1 数据层：请求快照 / 注入 / 抽取 / 恢复（test_extractor.py，30）

| 分组 | 覆盖 | 结果 |
|------|------|:----:|
| parse_token_id | `token_id:NNN`/纯数字/int/bool/非法 | PASS |
| save_original_params | chat/completions 默认值与 n/stream 快照 | PASS |
| inject_params | 注入字段、max(客户端,N) 双向、缺省 | PASS |
| extract_chat/completions | per-choice 抽取、topk_n 截断、None 位置容错 | PASS |
| strip_chat/completions | null/截断/文本还原/保留 token_id/n>1/resolver 三级兜底/§4.7 降级例外 | PASS |

### 4.2 检测算法（test_detector_illtypes.py + test_detector.py，29）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| test_detect_rare_character_with_vocab / filtered_out / no_vocab_top1 | 生僻字 ill_type=1（带词表类别判定 + 无词表 top1 降级 + 类别过滤假阳性） | PASS |
| test_detect_garbled_no_vocab_ratio_above/below_thresh | 乱码 ill_type=2（logp 区间 (-6,-5) 区分 rare/garbled、占比 0.25>0.2 触发 / 0.15 不触发） | PASS |
| test_detect_repetition_trajectory_only / short_sequence | 重复 ill_type=3（>single_window_thresh 窗口；短序列不误报） | PASS |
| test_detect_nan_value / inf_value | NaN/Inf ill_type=4 | PASS |
| test_run_multi_request_isolation | 多请求并行，单异常不串扰 | PASS |
| 配置校验 / 词表注入 / topk_n | 检测器配置、词表注入、topk_n | PASS |
| 辅助：get_ngrams/get_distinct_n/sliding_window/乱码状态 | 窗口与状态机 | PASS |

### 4.3 检测调度（test_detector_runner.py，9）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| run_sync / run_async | 正常执行 | PASS |
| construction_failure / unusable | 构造失败 → 永久 unusable 快速失败 | PASS |
| schedule_detection（正常/异常） | fire-and-forget、error 计数、done_callback 出集 | PASS |
| serialized_single_worker / topk_n / set_vocabulary | 串行化、topk_n 注入、词表懒注入 | PASS |

### 4.4 指标（test_metrics.py，10）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| test_metrics_content_type | 端点 200 + content-type | PASS |
| test_record_detection_normal_only_requests / anomaly_choice_index | detected_total 按 ill_type/model/choice_index 上报 | PASS |
| test_record_detection_nan_type / repetition_type | 异常类型标签 | PASS |
| test_record_detection_accumulates_requests_per_request | 每请求累计 requests_total | PASS |
| test_record_error | error 计数 | PASS |
| test_record_detection_unknown_model_label | 未知 model 标签兜底 | PASS |
| test_registry_isolated_from_default | 独立 registry，不影响全局 | PASS |
| test_record_detection_does_not_raise_on_bad_input | 非法输入容错 | PASS |

### 4.5 tokenizer 解析（test_token_resolver.py + test_token_categorizer.py，38）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| test_resolve_returns_text_and_caches / unknown_id / decode_raises / none_id | resolve 缓存、未知/异常/None 返回 None | PASS |
| test_acquire_tokenizer_*（from_pretrained_local / models_fallback / root_preferred / all_fail / unreachable / no_server / cache_scan / explicit_first / argv_tokenizer / argv_model / logs_error） | tokenizer 7 级获取链（env/argv/root/served/cache-scan 优先级与降级） | PASS |
| test_from_pretrained_sets_trust_remote_code / respects_explicit | trust_remote_code 默认与显式覆盖 | PASS |
| test_parse_vllm_argv_*（model_and_tokenizer / model_only / tokenizer_eq_form / host_eq_form / non_serve / value_flag / server_backward_compat / server_host_eq_form / server_non_serve） | argv 解析（--flag= 与 --flag value、值型 flag 防误认、host/port） | PASS |
| test_poll_model_root / gives_up_on_timeout | /v1/models 轮询（成功/超时） | PASS |
| test_generate_tk2cat_backend_decoder / fallback_highlevel_decode / skips_undecodable / no_decode_path_raises / keys_are_strings | tk2cat 生成（backend 优先/high-level 兜底/undecodable 跳过/无 decode 路径报错） | PASS |
| test_get_decode_fn_prefers_backend / falls_back_to_highlevel / none_when_both_missing / safe_decode_* | decode 函数选择与安全调用 | PASS |

### 4.6 中间件分派与助手（test_middleware_helpers.py，19）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| _read_all_body | 单块/多块/disconnect | PASS |
| _make_replay_receive | 首次合成 body、二次不返回空 http.request | PASS |
| _patch_scope_content_length | 改写/补加/浅拷贝隔离 | PASS |
| 分派 | 非 HTTP/GET metrics/GET models/GET chat/disabled 透传 | PASS |
| 错误响应透传 | 400+错误 JSON → 状态码/消息保留、不调度检测；500+非 JSON → 原样透传 | PASS |
| 构造期降级 | env 非法（top_logprobs=0）→ config.enabled=False | PASS |
| 终端 body 幂等 | 终端后重复 body 忽略，不二次调度 | PASS |
| _ensure_resolver | acquire 缓存、失败返回 None | PASS |

### 4.7 SSE 流式处理器（test_sse.py，15）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| [DONE]/keep-alive/非 JSON/CRLF | 原样透传 | PASS |
| 跨块重组 | 半事件缓冲、补齐后单条输出 | PASS |
| 多块累积 + 每块恢复 | 检测数据不受客户端 M 截断、n=3 按 choice.index 独立成组 | PASS |
| 多 data 行 | 非法 payload 原样透传 | PASS |
| 附加字段 | event:/id: 保留 + 恢复生效 | PASS |
| 多分块 | 逐字节喂入，未完整不输出 | PASS |
| flush 无 DONE | 排空残余检测 | PASS |

### 4.8 配置（test_config.py，13）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| test_config_defaults / env_override | 默认值与 env 覆盖 | PASS |
| test_config_invalid_top_logprobs / invalid_top_logprobs_high | top_logprobs 1-20 校验（0/21 拒绝） | PASS |
| test_config_invalid_monitor_rate / monitor_rate_boundaries_valid | monitor_rate 校验（1.5 拒绝 / 0.0、1.0 边界合法） | PASS |
| test_resolve_config_path_default / missing_returns_none | detector 路径存在/缺失 | PASS |
| test_tokenizer_model_default_none / env | tokenizer_model env | PASS |
| test_config_workers_zero_falls_back_to_default | workers=0→1 回退 | PASS |
| test_config_enabled_invalid_string_defaults_true | enabled 非法串→默认 True | PASS |
| test_config_metrics_path_empty_uses_default | metrics_path 空白→默认 | PASS |

---

## 5. Mock E2E 测试（in-process ASGI，共 37）

用 `httpx.ASGITransport` 挂载中间件 + 模拟下游 FakeVLLM，覆盖真实请求-响应全链路。

### 5.1 chat 非流式（test_e2e_chat.py，8）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| injection_and_restore_no_logprobs | 注入字段 + Content-Length + logprobs=null + 关联头 | PASS |
| request_id_unique | 唯一关联头（uuid4 hex） | PASS |
| restore_truncate_decode | top_logprobs=3 截断 + 解码文本 | PASS |
| keep_token_ids_when_requested | rtati=True 原样保留 | PASS |
| inject_max_client_vs_n | 客户端 top=5/N=20→20、top=10/N=5→10 | PASS |
| detect_truncate_n_vs_client | 客户端 10 / 检测 4 / 返回 10 | PASS |
| resolver_text_no_leak / no_resolver_fallback | 真实 vLLM 破损 bytes 形态：resolver 还原 / 降级回退 token_id | PASS |

### 5.2 completions 非流式（test_e2e_completions.py，6）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| injection_and_restore_no_logprobs | 注入字段 + Content-Length + logprobs=null + 无泄漏 | PASS |
| inject_max | max(客户端,N) 注入 | PASS |
| restore_token_ids_kept | rtati=True 原样保留 | PASS |
| no_resolver_fallback_to_token_id | 无 resolver 降级回退 token_id | PASS |
| restore_text_with_resolver | resolver 还原真实文本 | PASS |
| no_topk_no_resolver_no_leak | 无 topk/无 resolver 时无泄漏 | PASS |

### 5.3 流式（test_e2e_streaming.py，9）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| chat_stream_incremental_and_done | 增量转发 + [DONE] 保留 | PASS |
| chat_stream_cross_chunk_reassembly | 跨块重组 | PASS |
| chat_stream_detection_after_done | 流式结束检测 | PASS |
| completions_stream_restore_no_token_id | 流式恢复无泄漏 | PASS |
| chat_stream_detection_full_topk_not_client_m | 检测数据不随客户端 M 截断 | PASS |
| completions_stream_n3_choice_index_preserved | n=3 choice_index 保留 | PASS |
| stream_no_buffering_done_present | 无缓冲透传 | PASS |
| chat_stream_resolver_per_chunk_no_leak / completions_stream_resolver_text_per_chunk | resolver 逐块还原无泄漏 | PASS |

### 5.4 检测（test_e2e_detection.py，5）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| 正常 / NaN | ill_type=4 检出 | PASS |
| n=3 多候选不覆盖 | 多候选独立 | PASS |
| 空响应跳过 | 空响应不检测 | PASS |
| 并发 5 请求 | 全部检测、零错误、pending 清空 | PASS |

### 5.5 采样/降级/隔离（test_e2e_sampling_metrics_degrade.py，9）

| 测试 | 覆盖 | 结果 |
|------|------|:----:|
| metrics_endpoint | 端点 200 + 指标存在 | PASS |
| monitor_rate_zero_passthrough / one_all_injected / partial_with_patched_random | rate 0.0/1.0/0.3（patch random） | PASS |
| degrade_when_paths_unresolvable | 路径不可解析降级 | PASS |
| detection_error_isolation | 检测异常隔离，不影响客户端 | PASS |
| disabled_master_switch_passthrough | 总开关 False 透传 | PASS |
| non_target_passthrough_no_injection | 非目标路径不注入 | PASS |
| non_json_body_passthrough | 非 JSON body 透传 | PASS |

---

## 6. 真实服务器 E2E（Qwen3-0.6B，Atlas 910B4）

> 部署：`bash /vllm-workspace/vllm/run_server.sh`
> → `vllm serve /home/gyl/models/Qwen3-0.6B --port 8008 --served-model-name Qwen3-0.6B --tensor-parallel-size 1 --middleware anomaly_middleware.AnomalyMiddleware`
> 启动日志确认：`anomaly_middleware` 预热完成，tk2cat 已加载（vocab_size=151643）——resolver 可用、检测带词表。
> `tests/test_e2e_live.py`（12 用例），服务不可达时自动 skip。

| 编号 | 测试 | 覆盖 | 结果 |
|:----:|------|------|:----:|
| L1 | server_serves_expected_model | `/v1/models` 暴露 Qwen3-0.6B（插件部署生效） | **PASS** |
| L2 | metrics_endpoint_live | `/anomaly/metrics` 200 + 指标存在 | **PASS** |
| L3 | chat_no_logprobs_transparent | chat 未请求 → logprobs=null + 无泄漏 + 关联头 | **PASS** |
| L4 | chat_logprobs_truncate_and_no_leak | top_logprobs=3 截断 + 每项真实文本 + 无 `token_id:` | **PASS** |
| L5 | chat_return_tokens_as_token_ids_kept | rtati=True → 原样保留 `token_id:` | **PASS** |
| L6 | chat_request_ids_unique | 3 请求关联头互异 | **PASS** |
| L7 | chat_stream_incremental_and_no_leak | 流式增量 + [DONE] + 无泄漏 | **PASS** |
| L8 | chat_stream_logprobs_truncated | 流式逐块截断到 3 | **PASS** |
| L9 | completions_no_logprobs_transparent | completions 未请求 → null + 无泄漏 | **PASS** |
| L10 | completions_logprobs_text_no_leak | logprobs=3 → tokens/top_logprobs 真实文本 | **PASS** |
| L11 | completions_stream_no_leak | completions 流式 + 关联头 + [DONE] + 无泄漏 | **PASS** |
| L12 | detection_runs_without_errors | 4 类请求（chat/completions、流/非流）→ requests+3、**errors=0** | **PASS** |

**live 累计**：服务器检测 44 次请求，`vllm_anomaly_detection_errors_total = 0`，零检测错误、零泄漏回归。

---

## 7. 测试文件清单

| 文件 | 用例数 | 层级 | 职责 |
|------|:------:|------|------|
| tests/test_extractor.py | 30 | Tier 0 | 快照/注入/抽取/恢复（含 n>1、降级例外） |
| tests/test_token_resolver.py | 28 | Tier 0 | resolver + tokenizer 7 级获取链 + argv 解析 |
| tests/test_middleware_helpers.py | 19 | Tier 0 | ASGI 助手 + 分派 + 错误透传 + 降级 + 终端幂等 |
| tests/test_detector_illtypes.py | 18 | Tier 0 | 四类异常检出 + 假阳性控制 + 窗口状态机 |
| tests/test_sse.py | 15 | Tier 0 | SSE 跨块重组/透传/多行/附加字段/多分块 |
| tests/test_config.py | 13 | Tier 0 | env 配置校验 + 边界回退 + 路径解析 |
| tests/test_detector.py | 11 | Tier 0 | 检测器配置/词表注入/topk_n |
| tests/test_token_categorizer.py | 10 | Tier 0 | 分类函数 + generate_tk2cat 降级链 |
| tests/test_metrics.py | 10 | Tier 0 | 指标记录/渲染/独立 registry |
| tests/test_detector_runner.py | 9 | Tier 0 | 懒构造/串行化/调度/异常隔离 |
| tests/test_e2e_streaming.py | 9 | Tier 1 | Mock 流式全链路 |
| tests/test_e2e_sampling_metrics_degrade.py | 9 | Tier 1 | 采样/降级/隔离/指标 |
| tests/test_e2e_chat.py | 8 | Tier 1 | Mock chat 非流式 |
| tests/test_middleware_preheat.py | 8 | Tier 0 | 预热线程 + tk2cat 注入 + 竞态补调 |
| tests/test_e2e_completions.py | 6 | Tier 1 | Mock completions 非流式 |
| tests/test_e2e_detection.py | 5 | Tier 1 | 检测执行（含并发） |
| tests/test_e2e_live.py | 12 | Tier 2 | 真实 vLLM 服务器 E2E（Qwen3-0.6B） |

---

## 8. 结论

全量 **220** 项测试（171 单元 + 37 Mock E2E + 12 真实服务器 E2E）全部通过，零失败、零检测错误。

- **spec.md 16 个功能需求章节全部有测试覆盖**，§5 验收标准逐项可追溯（见 §3 矩阵）。
- **真实部署验证通过**：`vllm serve Qwen3-0.6B --middleware anomaly_middleware.AnomalyMiddleware`
  在 Atlas 910B4 上完成 chat/completions 流式与非流式的注入、恢复、无泄漏、关联头与指标全链路验证，
  tokenizer 预热成功（vocab_size=151643），live 累计 44 次检测 **0 错误**。
- **未发现 Bug**，代码逻辑正确，v0.1.0 可用。

### 已知限制（设计约束，非缺陷）

- live E2E 依赖本地可加载的 Qwen3-0.6B（`local_files_only`）；服务不可达时用例自动 skip，不阻塞离线单测。
- `test_e2e_live.py` 使用真实模型推理，未刻意构造生僻字/乱码/重复输入（真实模型难以稳定触发），四类异常检出性由确定性单测（§4.2）覆盖。
- Mock E2E 的检测结果为 `[[is_ill, ill_type]]`，仅验证正常/NaN 两种真实路径；repetition 需 ≥1024 token 序列，在单测层覆盖。

**测试结论：全部通过，功能点覆盖完整，无已知功能缺陷。**


