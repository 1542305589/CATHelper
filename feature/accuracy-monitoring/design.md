---
title: vLLM 异常检测中间件 功能设计
role: technical-design
---

# vLLM 异常检测中间件 功能设计

## 1. 概述

### 1.1 设计目标

构建一个纯 ASGI 中间件包 `vllm_anomaly_middleware`，通过 vLLM 的
`--middleware` 单标志部署：

```
vllm serve <model> --middleware vllm_anomaly_middleware.AnomalyMiddleware
```

中间件对客户端**透明**：拦截推理请求、强制采集 logprobs、在后台运行
侧信道异常检测、将响应**精确还原**为客户端原始请求所期望的形态，并通过
独立的 Prometheus 端点暴露检测结果。客户端完全感知不到中间件的存在。

### 1.2 设计原则

- **透明优先**：注入与响应恢复无条件发生；检测是否执行由采样率决定，
  但不影响客户端看到的响应。
- **流式不缓冲**：采用纯 ASGI（而非 Starlette `BaseHTTPMiddleware`），
  SSE 流增量转发，避免缓冲破坏流式语义。
- **检测不阻塞客户端**：检测在响应全部发送完毕后以 fire-and-forget
  方式调度，客户端永远不等待检测。
- **显式降级**：检测后端不可用时，记录一次日志后永久退化为纯透传，
  指标端点仍可达并报零值。
- **单一统一类**：一个中间件类持有全部不可变配置与状态，按请求构造
  轻量协作者，避免多层包装与跨类共享可变状态。

### 1.3 不做的事

- 不改写检测算法本体、不新增异常类型。
- 不捆绑 tokenizer 全量解码 `/v1/completions` 文本（契约是"不泄漏
  `token_id:` 前缀"，以 nulling 满足；可后续插件式扩展）。
- 不替换或干扰 vLLM 自带 `/metrics`。
- 不支持非 vLLM 的 ASGI 服务器（代码可移植，但仅针对 vLLM 请求/响应形态测试）。

## 2. 总体架构

### 2.1 部署形态

`--middleware <module.path>.<ClassName>` 由 vLLM 经 `importlib`/`getattr`
（点分隔）加载，因值为类而实例化为 `Cls(app)`——**无 kwargs、无启动钩子、
无路由注册钩子**。由此导出四条硬约束：

1. 构造签名固定为 `__init__(self, app)`；
2. 全部配置来自环境变量/磁盘文件；
3. 检测器必须懒构造；
4. 指标端点必须由中间件**内联**响应（不能用 `app.add_api_route`）。

### 2.2 模块组成

```
vllm_anomaly_middleware/
├── __init__.py            # 重导出 AnomalyMiddleware（短路径）
├── middleware.py          # 统一中间件类 + RequestContext + 请求助手 + ResponseInterceptor
├── config.py              # 环境变量配置 + 检测器路径解析
├── metrics.py             # 独立 CollectorRegistry + 指标记录/渲染
├── extractor.py           # 抽取/恢复（流式与非流式）+ SSEStreamProcessor
├── detector_runner.py     # 检测运行器（线程池+锁+懒构造+调度）
└── response_anomaly/      # vendored 检测算法（含 configs/ 与 token2category/）
    ├── detector.py
    ├── __init__.py
    ├── configs/{config.yaml, mtype_config.json}
    └── token2category/*.json
```

### 2.3 职责划分

| 组件 | 职责 |
|---|---|
| `AnomalyMiddleware` | 持有配置/runner/指标/待办任务集；`__call__` 分派：内联指标、降级透传、拦截注入、装拦截器、委托下游 |
| `RequestContext` | 单请求上下文：原始参数、model、关联 id、是否检测 |
| `ResponseInterceptor` | 包装 `send`：判流式/非流式、注入关联头、缓冲或增量处理、恢复响应、调度检测 |
| `SSEStreamProcessor` | 跨块事件重组、每块恢复、per-choice 累积检测数据 |
| `Extractor` | 纯函数：解析 token-id、抽取 per-choice (topk,tokens)、恢复响应（截断/null/文本还原） |
| `DetectorRunner` | 单 worker 线程池 + 锁；worker 内懒构造检测器；同步/异步执行 |
| `Config` | 环境变量读取与校验；检测器路径解析（env→vendored→外部） |
| `Metrics` | 独立 registry；计数/直方图/gauge；渲染文本暴露 |

## 3. 核心功能设计

### 3.1 请求拦截与参数注入

**拦截范围**：仅 `POST /v1/chat/completions` 与 `POST /v1/completions`。
其余请求（任意方法/路径）原样转发——同一 scope/receive/send，不读不改 body。
目标路径上的非 POST 方法也透传。

**强制注入**（覆盖请求体以加上检测所需参数）：
- chat：`logprobs=true`、`top_logprobs=<N>`、`return_tokens_as_token_ids=true`
- completions：`logprobs=<N>`（此处 logprobs 即数量）、`return_tokens_as_token_ids=true`

注入前**快照**客户端原始 logprobs 相关参数，供后续恢复。注入后**修正请求
`Content-Length`** 以匹配修改后的 body 长度（否则下游解析长度不匹配导致截断/挂起）。

**关键不变量**：`top_logprobs` 跨请求必须恒定（默认 20，可配置 1..20）。
原因见 §6.2（检测器 `topk` 首次锁定后不复位）。

### 3.2 响应抽取与恢复

**抽取**（供检测）：per choice 取 `(topk_logprobs: list[dict[int,float]],
tokens: list[int])`。
- chat：`choices[].logprobs.content[]`，每 entry 的 `token`（`"token_id:NNN"`）
  解析为 int，`top_logprobs[]` 每 entry 的 `token` 同理解析为 key、`logprob` 为 value。
- completions：`choices[].logprobs.tokens[]` 解析为 token-id 序列；
  `top_logprobs[]`（dict：token-id 字符串→logprob）解析为 dict[int,float]。

`parse_token_id` 兼容两种形态：`"token_id:NNN"` 与纯数字串（如 `"22"`），失败返回 -1。

**恢复**（供客户端，按原始参数）：
- 客户端未请求 `logprobs` → `choice.logprobs=null`。
- 客户端请求 `top_logprobs=N`（chat）/`logprobs=N`（completions）→ 截断到 N。
- 客户端未请求 `return_tokens_as_token_ids`：
  - chat：`token`/`top_logprobs[].token` 从 `bytes`（utf-8 字节列表）解码为文本；无 bytes 则移除该字段。
  - completions：无 tokenizer 可用，`tokens[]` 置为 `[null]*len`——**绝不**留 `token_id:` 前缀。
- 客户端**已**请求 `return_tokens_as_token_ids` → 原样保留 `token_id:NNN`（这正是客户端所要）。

### 3.3 流式响应处理（SSE）

**流式形态**：vLLM 流式每块只含**最新 token** 的 logprobs（delta 语义）。
设计要求**先缓存全部流式推理结果，再进行检测**。

- chat 流式：每块 `choices[].logprobs.content[]` 含本块新 token 的一个 entry
  （`token`/`logprob`/`bytes`/`top_logprobs[]`）。累积器对每块 content 的每个 entry 做 append。
- completions 流式：每块 `choices[].logprobs.tokens[]` 与 `top_logprobs[]`，append。
  - 防御：若某块呈现累积数组（位置与已累积重叠），采用 **latest-longest-wins**
    （取最长/最新数组覆盖），兼容可能的累积式。默认按 delta-append。

**SSE 处理状态机**（`SSEStreamProcessor`）：
- 跨块缓冲 `_buffer`，按 `\n\n` 切分完整事件；半事件留缓冲。
- `_process_event`：分离 `data:` 行与其它行（`event:`/`id:`/`retry:`/注释）；
  无 data 行（keep-alive）原样透传；`data: [DONE]` 原样透传；其余 `json.loads` 成功则
  捕获 model → `_extract_streaming`（per-choice append 累积）→ `_strip_streaming`
  （每块无状态恢复）→ 重序列化 `data: <json>\n` + 其它行 + `\n\n`。
- `flush()`：排空尾部残余（无 `\n\n`），处理之，输出则补 `\n\n`。
- `get_detection_data()`：按 choice index 升序返回 `(topk, tokens, model_name)`。

**双态并存**：转发是增量无状态的（每块即发），检测数据累积是有状态的（跨块 append）。
两者读写不同字段，互不干扰。`[DONE]`/keep-alive 永不参与累积。

**CRLF 兼容**：SSE 规范允许 `\r\n\r\n`；`_process_event` 内对每行 `rstrip(b"\r")` 兼容。

### 3.4 异常检测调度

- 检测在**响应全部发送完毕后**调度（fire-and-forget）：非流式在终端 body 发出后；
  流式在 `[DONE]`/`more_body=False` 后。
- 检测数据：非流式取 `extract_*_response` 的 per-choice 结果；流式取
  `SSEStreamProcessor.get_detection_data()`。
- **空响应不检测**：`tokens` 为空或全空时跳过。
- 检测任务防 GC：中间件持 `_pending_tasks: set`，入集，`done_callback` 出集；
  关闭时未完成任务随 event loop 取消（fire-and-forget 可接受）。
- 异常全捕获：检测协程内 try/except，失败计 `detection_errors_total`，不影响客户端。

### 3.5 检测采样

- 每被拦截请求抽 `random.random()`；`will_detect = rand < sample_rate`（默认 1.0，范围 0..1）。
- **注入与恢复始终发生**（透明无条件）；仅检测提交受 `will_detect` 门控。
- 理由：客户端若请求了 `logprobs`，非采样请求与其不应可观察不同；采样只控检测成本。
- 代价：非采样请求仍付 JSON 解析/改写成本——廉价可接受。

### 3.6 请求关联标识

- 每被拦截请求生成 `request_id = uuid.uuid4().hex`。
- 在 `http.response.start` 追加响应头
  `(b"x-anomaly-request-id", request_id)`，**然后再发给下游 send**。
  - 流式：start 立即发送→注入后再发。
  - 非流式：start 本就缓冲→发前注入（同时可 patch 响应 Content-Length）。
- `request_id` 传入检测任务用于日志，支持端到端追踪。

### 3.7 Prometheus 指标

- `__call__` 在最前拦截 `GET <metrics_path>`（默认 `/anomaly/metrics`），直接内联响应，
  不涉及下游路由。仅 GET 被拦截；POST 到该路径透传给下游。
- 独立 `CollectorRegistry`（与 vLLM 默认 `/metrics` 隔离）。
- 指标：
  - `vllm_anomaly_requests_total`（Counter）
  - `vllm_anomaly_detected_total`（Counter，labels `ill_type`,`model`）
  - `vllm_anomaly_detection_errors_total`（Counter）
  - `vllm_anomaly_detection_duration_seconds`（Histogram）
  - `vllm_anomaly_last_result`（Gauge，labels `ill_type`,`model`）
- `ill_type` 取值：0=normal,1=rare_character,2=garbled,3=repetition,4=nan_value。
- `normal`(0) 只增 requests，不计 detected。
- Content-Type：`text/plain; version=0.0.4; charset=utf-8`。
- `model` 标签来自请求体 `model` 字段；缺失用 `"unknown"`。

### 3.8 配置与路径解析

环境变量（带默认）：
- `VLLM_ANOMALY_ENABLED`（默认 1）
- `VLLM_ANOMALY_TOP_LOGPROBS`（默认 20，范围 1..20）
- `VLLM_ANOMALY_METRICS_PATH`（默认 `/anomaly/metrics`）
- `VLLM_ANOMALY_SAMPLE_RATE`（默认 1.0，范围 0..1）
- `VLLM_ANOMALY_DETECTOR_WORKERS`（默认 1）
- `VLLM_ANOMALY_DETECTOR_CONFIG_PATH` / `VLLM_ANOMALY_MTYPE_CONFIG_PATH` /
  `VLLM_ANOMALY_TK2CAT_PATH`（显式路径覆盖）

校验：`top_logprobs∈[1,20]`、`sample_rate∈[0.0,1.0]`。

检测器路径解析顺序：
1. 三个显式 env 全设 → 用之，不自动发现；
2. vendored 拷贝（按 `__file__` 定位 `response_anomaly/configs|token2category`）；
3. 外部可导入 `response_anomaly` / `msprobe.response_anomaly`；
4. 都没有 → `RuntimeError`。

### 3.9 降级机制

- 检测器/配置无法加载或解析 → 记录一次日志，置 `enabled=False` 永久降级。
- 降级后：`__call__` 早退透传（不读 body、不注入、不拦截），但指标端点仍可达报零值。
- 触发点：仅在首个 `will_detect=True` 请求上尝试构造 runner（见 §6.1）。
  `sample_rate=0` 则永不触发，runner 永不构造，指标报零。

## 4. 关键组件设计

### 4.1 AnomalyMiddleware（统一中间件类）

**构造** `__init__(self, app)`：建 `PluginConfig`；持有未初始化 runner 占位；
attach 指标助手；建 `_pending_tasks` set 与 `_runner_lock`/`_runner_inited`。
**不做重活**（无 numpy、无文件读）。

**`__call__(scope, receive, send)` 分派**：
1. 非 http scope → 透传 `self.app`。
2. `GET <metrics_path>` → 内联 `_serve_metrics`。
3. 非 POST、非目标路径、或 `enabled=False` → 透传。
4. 目标 POST：读 body、解析 JSON；非 dict/非 JSON → 透传。
5. `will_detect = random.random() < sample_rate`。
6. 若 `will_detect`：`_ensure_runner()`（双检锁）；失败→`enabled=False`→本请求透传。
7. 若仍 enabled：`save_original_params` → `inject_params` → patch 请求 CL →
   建 `RequestContext`(orig/model/request_id/will_detect) → 装 `ResponseInterceptor` →
   `await self.app(scope, replay_receive, interceptor)`。

**`_ensure_runner` 顺序前置**：runner 构造（廉价）在注入之前；失败则本请求纯透传，
避免半注入响应。重头检测器在 worker 线程内首次检测时构造（见 §6.1）。

**`_serve_metrics(send)`**：`render_metrics()` → 200 + 正确 content-type +
正确 content-length，作为完整 ASGI 响应发出。

### 4.2 ResponseInterceptor（响应拦截器）

**构造**：`(send, *, is_chat, orig_params, model, runner, request_id, will_detect)`。
状态：`_is_streaming`、`_start_msg`、`_body_buf`、`_sse`、`_finished`、`_detection_scheduled`、`_detection_results`。

**`__call__(message)` 分派**：
- `http.response.start` → `_on_start`：判 `content-type` 含 `text/event-stream`；
  注入 `x-anomaly-request-id`；流式建 `SSEStreamProcessor` 并立即 send(start)；
  非流式缓冲 `_start_msg`。
- `http.response.body` → `_on_body`。
- 其它 → 透传。

**`_on_body` 流式分支**：
```
out = _sse.feed(body)
if more_body:
    if out: send({type:"http.response.body", body:out, more_body:True})
else:
    if _finished: return          # 防重复
    tail = _sse.flush()
    send({type:"http.response.body", body:(out or b"")+tail, more_body:False})
    _finished = True
    _maybe_schedule_detection()
```

**`_on_body` 非流式分支**：
```
_body_buf.extend(body)
if more_body or _finished: return
final = _process_complete()       # extract+strip+reserialize；非 JSON 原样透传
_send_start(final)                # 注入关联头 + patch 响应 CL
send({type:"http.response.body", body:final, more_body:False})
_finished = True
_maybe_schedule_detection()
```

**`_process_complete`**：`json.loads(_body_buf)`；失败→返回原始 bytes（透传，不注入检测）；
成功→`extract_*_response` 得 per-choice 存 `_detection_results`，再 `strip_*_response`，
`json.dumps(...).encode()` 返回。

**`_send_start(final)`**：从 `_start_msg` patch headers：改写/补 `content-length`，
注入 `x-anomaly-request-id`，send。

**`_maybe_schedule_detection`**：
```
if not will_detect or _detection_scheduled or _runner is None: return
_detection_scheduled = True
try: topk, tokens, configs = _get_detection_inputs()
except: log; return
if not tokens or not any(tokens): return
schedule_detection(_runner, topk, tokens, configs, request_id=_request_id, model=_model)
```
`_get_detection_inputs`：非流式取 `_detection_results`；流式取 `_sse.get_detection_data()`。

### 4.3 SSEStreamProcessor

见 §3.3 状态机。要点：
- `feed(chunk)->bytes`：缓冲 + `\n\n` 切分 + `_process_event`。
- `flush()->bytes`：排空尾部。
- `get_detection_data()->(topk_all, tokens_all, configs_all)`。
- 累积（有状态）与每块恢复（无状态）并存。

### 4.4 DetectorRunner

**构造** `(config_path, mtype_path, tk2cat_path, max_workers=1)`：`ThreadPoolExecutor`
+ `threading.Lock`。

**`_get_detector()`**（worker 线程内懒构造）：首次调用时
`from .response_anomaly.detector import ILLDetector` 并构造（numpy 导入与多 MB JSON
加载不阻塞 event loop）。

**`run_sync(topk, tokens, model_configs)`**：`with self._lock: detector=_get_detector();
return detector.run(topk, tokens, model_configs)`。

**`run_async(...)`**：`loop.run_in_executor(self._executor, self.run_sync, ...)`。

**`schedule_detection(runner, topk, tokens, model_configs, *, request_id, model) -> Task`**：
内部 `_run()`：`detection_duration.time()` 计时 → `runner.run_async(...)` →
`record_detection(results, model)`；except → `record_error()`。

### 4.5 检测器契约要点（vendored）

- 批量入口：`run(topk: list[list[dict[int,float]]], tokens: list[list[int]],
  model_configs: list) -> list[[bool,int]]`。`model_configs` 是与 choice 平行的列表，
  第 i 项传给单请求检测的 `model_config`。
- **`model_configs` 即 model_name**：传 `[request.model]*n_choices` 字符串列表。
  检测器内部对 `mtype_config.json` 的 key（如 `qwen3-30b-a3b`/`deepseekv3`/`glm-4-7`）
  做去分隔符模糊匹配。Runner 无需自加载 mtype_config。
- **反面陷阱**：`mtype_config.json` 条目形如 `{"bos":..,"eos":[..]}`，**无 `model_name`**。
  若把该 dict 当 `model_config` 传入 → 匹配失败 → **静默退化为无词表检测**（生僻字词表路径
  被跳过）。故 `model_configs` 元素**必须**是模型名字符串。
- vendored `detector.py` 必修 3 处缺陷：缺 `import json`、缺 `import yaml`、
  `check_path_exists` 未定义（仅定义了 `check_path_exist`，构造时 `NameError`）。
- 实例态副作用：`topk` 首次 `run` 锁定后不复位（故 `top_logprobs` 必须跨请求恒定）；
  `_garbled_count` 每请求复位。单 worker + 锁同时保护两者。

### 4.6 Config / Metrics

- `config.py`：`PluginConfig`（env 读取+校验）；`resolve_detector_paths()` 纯函数，
  顺序见 §3.8。
- `metrics.py`：独立 registry；`record_detection(results, model)`、`record_error()`、
  `render_metrics()->bytes`、`METRICS_CONTENT_TYPE`。

## 5. 数据流

### 5.1 chat 非流式
```
client POST /v1/chat/completions (无 logprobs)
  → 读 body, parse JSON; will_detect (say True)
  → _ensure_runner (双检锁, 廉价); ok
  → save_original_params; inject(logprobs/top_logprobs/return_tokens_as_token_ids)
  → patch 请求 CL; 装 ResponseInterceptor
  → app(...) 返回 choices[].logprobs.content[] (含 bytes, token="token_id:NNN")
  → _on_start(缓冲); _on_body(缓冲到 more_body=False)
       _process_complete: extract→存检测数据; strip→logprobs=null, 文本从 bytes 还原
       _send_start(注入关联头 + patch 响应 CL); send(terminal body)
  → 调度检测(topk, tokens, [model]*n, request_id)
client 收到: logprobs=null, 无 token_id:, 带 x-anomaly-request-id
worker: run_sync→ILLDetector.run→[[is_ill,ill_type]]→record_detection
```

### 5.2 chat 流式
```
client POST ... (stream=true)
  → ... inject ... ResponseInterceptor
  → _on_start: content-type 含 text/event-stream → 建 SSEStreamProcessor
       注入关联头; send(start) 立即
  → _on_body(more_body=True, body=chunk1):
       _sse.feed → 增量 strip+转发; 同时 _extract_streaming append 累积
  → ... 多块 ...
  → _on_body(more_body=False): _sse.flush; send(terminal); _finished=True; 调度检测
client 收到: 增量恢复块 + data: [DONE] (原样透传) + 关联头
worker: 累积全部 token 的 ILLDetector.run → record_detection
```

### 5.3 completions
注入 `logprobs=<N>` + `return_tokens_as_token_ids=true`；恢复时若客户端未请求
`return_tokens_as_token_ids` → `tokens[]=[null]*len`（无 bytes 可解码，绝不留 `token_id:`）；
`top_logprobs` dict 截断到请求 N。

## 6. 并发与生命周期

### 6.1 两段式懒加载

- **廉价阶段**（请求路径，首个 `will_detect` 请求）：`_ensure_runner()` 构造
  `DetectorRunner`——仅线程池 + 锁，无 numpy/文件 I/O。失败（路径解析不出）则置
  `enabled=False` 永久降级。
- **重阶段**（worker 线程内，首次检测）：`_get_detector()` 懒构造 `ILLDetector`
  （numpy + 多 MB JSON），位于请求路径之外，不影响首请求延迟。
- 双检锁：`if _runner_inited: return; with lock: if _runner_inited: return; ...; finally: _runner_inited=True`。
  `finally` 保证无论成败只跑一次，避免每请求重试昂贵导入。GIL 下 bool 写入在锁内安全。

### 6.2 检测串行化

- 检测器 CPU-bound（numpy/FFT），且每次 `run` 突变实例态（`topk`/`_garbled_count`），
  不得阻塞 event loop、不得与自身并发。
- 单 worker 线程池自然串行；`threading.Lock` 为未来 `max_workers>1` 的 belt-and-suspenders。
- `run_sync` 在锁内调 `detector.run`。

### 6.3 检测任务生命周期

- fire-and-forget：`asyncio.create_task`，异常全捕获。
- 防 GC：`_pending_tasks` 持引用，`done_callback` 出集。
- 关闭：未完成任务随 loop 取消（结果丢失不影响客户端）。

## 7. 边界条件与异常处理

| 场景 | 处理 |
|---|---|
| 非 JSON 请求体 | 透传，不注入 |
| 非 dict 请求体 | 透传，不注入 |
| 非 JSON 响应体（错误页） | `_process_complete` 失败→原样透传，不注入检测 |
| 空响应（无 token） | 不调度检测 |
| 检测器抛异常 | 计 detection_error，不影响客户端（响应已发完） |
| 检测器不可构造 | 永久降级透传，指标报零 |
| 并发首请求竞态 | 双检锁，benign 序列化 |
| 下游多发终端 body | `_finished` 守卫，忽略后续，不重复调度 |
| 流式无 `[DONE]` 即断 | `flush` 排空残余，按已累积数据检测 |
| CRLF SSE | 行尾 `rstrip(b"\r")` 兼容 |
| metrics 路径被 app 路由占用 | 默认 `/anomaly/metrics` 避开 vLLM `/metrics`；可配置 |
| `Expect: 100-continue` | ASGI 不暴露，由 uvicorn/vLLM 处理，安全 |
| chunked 请求 | ASGI 已合并为完整 body，`_read_all_body` 正确 |

## 8. 部署

- 安装：`pip install -e D:\programs\new_codes`。
- 启动：`vllm serve <model> --middleware vllm_anomaly_middleware.AnomalyMiddleware`。
- 无需 entry-point 注册、无需 `VLLM_PLUGINS` 白名单、无需特定 vLLM 插件接口，
  仅需 vLLM 支持 `--middleware`。
- 短路径 `vllm_anomaly_middleware.AnomalyMiddleware` 与长路径
  `vllm_anomaly_middleware.middleware.AnomalyMiddleware` 均可解析（`__init__.py` 重导出）。
- 回滚：移除 `--middleware` 标志（服务器恢复默认行为），或重指向上游包。中间件无持久副作用。

## 9. 待实现期校准项

- vLLM 流式 chat chunk 的 logprobs 位置：按 `choices[].logprobs.content[]` 读取；
  若实际为 `choices[].delta.logprobs.content[]`，改读 `choice["delta"]["logprobs"]` 即可（单点改）。
- vLLM completions 流式 `tokens[]`/`top_logprobs[]` 是否为 delta；`latest-longest-wins` 兜底。
- 注入 `return_tokens_as_token_ids=true` 后，chat/completions 各 token 字段是否确为 `"token_id:NNN"`。
