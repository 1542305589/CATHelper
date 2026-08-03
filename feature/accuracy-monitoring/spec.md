# 推理精度异常检测中间件 技术规格说明书(SPEC)

## 1. 目的与范围

本文档规定基于vllm的在线精度异常检测中间件`vllm_anomaly_middleware`的功能行为、输入输出契约与验收标准。
中间件通过 vLLM 的 `--middleware` 插件部署，对客户端透明地：拦截推理请求、
强制采集 logprobs和token_id、后台运行算法异常检测、不影响客户端请求响应状态返回、
并通过独立 Prometheus 端点暴露检测结果。

适用范围：vLLM 的 `POST /v1/chat/completions` 与 `POST /v1/completions` 在线推理请求端点，
流式与非流式推理请求均覆盖。

## 2. 功能需求

### 2.1 请求拦截

中间件仅拦截 `POST /v1/chat/completions` 与 `POST /v1/completions`。
所有其他 HTTP 请求（任意方法或路径）原样转发给下游应用——同一 scope、receive、send，
不读取或修改 body。目标路径上的非 POST 方法也透传（保持原先vllm处理方式一致）。

**验收**
- `GET /v1/models` → 原样转发，body 不读不改。
- `GET /v1/chat/completions` → 原样转发；仅 POST 上的两个目标路径被拦截。

### 2.2 强制 logprobs、token_id采集

对每个被拦截请求，覆盖请求体强制检测所需参数：chat 设 `logprobs=true`、
`top_logprobs=<N>`、`return_tokens_as_token_ids=true`；completions 设 `logprobs=<N>`、
`return_tokens_as_token_ids=true`——无视客户端原值。覆盖前快照客户端原始 logprobs 相关
参数，供响应恢复。请求 `Content-Length` 修正为新 body 长度。

**验收**
- 客户端未带 logprobs 的 chat 请求 → 转发 body 含 `logprobs=true`/`top_logprobs=<N>`/
  `return_tokens_as_token_ids=true`，`Content-Length` 反映新长度。
- 客户端 `top_logprobs=5` 且配置 N=20 → 转发 `top_logprobs=20`，原 5 被内部保留用于恢复。

### 2.3 客户端透明响应恢复

响应处理后恢复为客户端原始请求被满足时的形态：
- 客户端未请求 `logprobs` → `choice.logprobs` 置 null。(vllm默认关闭)
- 客户端请求 `top_logprobs=N` → 各 top-logprobs 列表截断至 N。（vllm默认关闭）
- 恢复响应中不得出现任何 `token_id:` 前缀字符串；token 文本从响应 `bytes`/text 字段
  解码；无法解码处置 null 而非 `token_id:` 字符串。（vllm默认关闭）
- 适用于 chat 与 completions，流式与非流式。

**验收**
- chat 客户端未请求 logprobs → 恢复后 `choice.logprobs=null`，全文无 `token_id:`。
- chat 客户端 `logprobs=true`/`top_logprobs=3` → 各 `top_logprobs` 至多 3 项，
  每项 `token` 为解码文本（非 `token_id:`）。
- completions 客户端未设 `return_tokens_as_token_ids` → `tokens` 列表有 `bytes`/text
  处为解码文本，无处为 null——绝不出现 `token_id:`。
- completions 客户端已设 `return_tokens_as_token_ids` → 原样保留 `token_id:NNN`。

### 2.4 流式安全转发

对 `text/event-stream` 响应，逐事件增量转发，不缓冲整流。终端 `data: [DONE]` 保留。
跨 body 块的半事件先重组再处理。

**验收**
- 多块 SSE 流 → 客户端随处理增量收到恢复块 + 终端 `data: [DONE]`，中间件不先全缓冲。
- 一条 SSE 事件被拆到两块 → 重组后处理，客户端收到一条完整事件。

### 2.5 异常检测执行

对每个被选中检测的请求，per choice 抽取 top-k logprobs（token-id→logprob 映射）与
token-id 序列，提交检测后端。各 choice 独立检测。结果按 `ill_type` 分类：
0=normal,1=rare_character,2=garbled,3=repetition,4=nan_value。
检测仅在响应全部发送给客户端后调度（fire-and-forget），客户端不等待检测。

**验收**
- 非流式 chat 2 choices 且被选中 → per choice 抽取 `(topk_logprobs, tokens)` 提交检测，
  返回每 choice 一个 `[is_ill, ill_type]`。
- 空响应（无生成 token，如错误或空 completion）→ 不提交检测。
- 流式响应结束后，用跨块累积的 per-choice 数据调度检测**恰好一次**，客户端不等检测。
- 请求 `model` 命中 `mtype_config.json` 已知模型 → 检测走词表路径；不命中 → 退化为
  top1 阈值路径且不报错。

### 2.6 检测失败隔离

抽取、调度或检测期间任何异常须被捕获并记录，不得改变客户端响应、状态码或头部
（响应在检测时已发完）。检测失败计为 detection-error 指标。

**验收**
- 检测器运行中抛异常 → 客户端响应不受影响（已发完），计 detection-error，
  后续请求正常处理。

### 2.7 检测后端串行化

检测后端每次调用突变实例态，故并发检测调用须串行，不得在同一实例上并发执行。

**验收**
- 两被选中请求同时完成 → 其检测调用串行（一者等另一者），而非同实例并发。

### 2.8 检测采样

支持可配置采样率 ∈[0.0,1.0]。请求以等于采样率的概率被选中检测。未选中请求仍注入
logprobs 并恢复响应（透明无条件），但不提交检测。1.0 表示全检测，0.0 表示不检测。
默认 1.0。

**验收**
- 采样率 0.0 → 不提交检测，但 logprobs 仍注入、响应仍恢复。
- 采样率 1.0 → 每请求都提交检测。

### 2.9 请求关联标识

为每个被拦截请求生成唯一关联标识，经 `x-anomaly-request-id` 响应头返回给客户端。
同一标识关联该请求的检测结果以供追踪。

**验收**
- 任意被拦截请求 → 响应含 `x-anomaly-request-id` 头，值为唯一。

### 2.10 Prometheus 指标暴露

在可配置 HTTP 路径（默认 `/anomaly/metrics`）响应 `GET`，内联作答不涉及下游路由。
指标使用与应用默认 registry 隔离的 registry。至少包含：已处理请求计数；
按 `ill_type` 与 `model` 标签的检测结果计数；检测错误计数；检测耗时直方图；
按 `ill_type` 与 `model` 标签的最近结果 gauge。

**验收**
- `GET /anomaly/metrics` → HTTP 200，`Content-Type: text/plain; version=0.0.4; charset=utf-8`，
  body 为 Prometheus 文本暴露。
- 下游无 `/anomaly/metrics` 路由 → 中间件仍直接作答。

### 2.11 环境变量配置

全部运行时配置从环境变量读取（带合理默认），因构造除 app 外无参数。

至少可配：总开关；top-logprobs 数；指标路径；检测采样率；检测 worker 数；检测器 config/mtype/tk2cat
路径显式覆盖。

**验收**
- 总开关置假 → 不注入不检测（纯透传），但指标端点仍可达报零值计数。

### 2.12 检测器数据路径解析

解析检测后端配置文件（`config.yaml`、`mtype_config.json`）与 `token2category/` 表。
顺序：显式环境变量覆盖优先；其次包内 vendored 拷贝；再次外部可导入的
`response_anomaly`/`msprobe.response_anomaly`。

**验收**
- 三个路径 env 全设 → 用之，不自动发现。
- 无路径 env 且 vendored 与外部包并存 → 用 vendored。
- vendored 默认路径下 `ILLDetector` 可构造（不抛 ImportError/NameError），
  `.run(...)` 返回 `[[is_ill, ill_type]]` 形状。

### 2.13 优雅降级——检测功能不可用

检测后端或其配置无法加载/解析 → 记录一次日志，对所有后续请求纯透传（不注入、不检测）。
指标端点仍可达报零值。降级绝不改变客户端响应。

**验收**
- 首个会被检测的请求上检测后端不可导入或路径不可解析 → 记录一次，停止注入与检测，
  后续请求原样转发；`GET /anomaly/metrics` 仍 200 报零值。

### 2.14 单标志部署

仅加 `--middleware <module.path>.<ClassName>` 即可部署。构造函数仅接受一个参数
（被包装的 ASGI app）且无 kwargs。部署不要求 entry-point 注册、插件白名单或特定
vLLM 插件接口——仅需 vLLM 支持 `--middleware`。

**验收**
- `--middleware vllm_anomaly_middleware.AnomalyMiddleware` → 中间件以
  `AnomalyMiddleware(app)` 实例化，进程生命期内拦截目标请求。

## 3. 输入输出契约

### 3.1 构造与调用

- `AnomalyMiddleware(app)`：`app` 为下游 ASGI(Asynchronous Server Gateway Interface) 可调用。
- `async def __call__(self, scope, receive, send)`：纯 ASGI。

### 3.2 请求侧

- 读请求体：聚合所有 `http.request` 消息至 `more_body=False`（处理 `http.disconnect`）。
- 重放 receive：合成单条 `http.request`(body, more_body=False)，二次读返回空 body。
- 请求 scope：浅拷贝并改写 `content-length` header。

### 3.3 响应侧

- `http.response.start`：注入 `x-anomaly-request-id`；流式立即发；非流式缓冲后发（并 patch 响应 content-length）。
- `http.response.body`：流式增量转发；非流式缓冲至 `more_body=False` 再处理。
- 终端 body 后置 `_finished`，忽略后续终端消息。

### 3.4 检测数据

- 非流式：`extract_*_response(data) -> list[(list[dict[int,float]], list[int])]`（per choice）。
- 流式：`SSEStreamProcessor.get_detection_data() -> (topk_all, tokens_all, configs_all)`。
- `model_configs = [request.model]*n`（model_name 字符串列表）。
- 调度：`schedule_detection(runner, topk, tokens, configs, *, request_id, model)`。

### 3.5 指标

- `render_metrics() -> bytes`；`METRICS_CONTENT_TYPE = "text/plain; version=0.0.4; charset=utf-8"`。
- 标签：`ill_type`(0..4 名)、`model`(来自请求 `model`，缺失用 `"unknown"`)。

## 4. 行为约束与不变量

1. **透明无条件**：注入与响应恢复在 `enabled=True` 时总发生，不受采样影响；
   采样只门控检测提交。
2. **降级即透传**：`enabled=False`（master 开关 off 或检测器不可构造）→ 不读 body、
   不注入、不拦截；指标端点独立可达。
3. **top_logprobs 跨请求恒定**：默认 20，可配 1..20，但运行期不可变。
   理由：检测器 `topk` 首次锁定后不复位。
4. **model_configs 即 model_name**：传模型名字符串；禁止传 `mtype_config` 的 dict 条目
   （会静默退化为无词表检测）。
5. **检测串行**：单实例检测调用不并发（单 worker + 锁）。
6. **检测不阻塞客户端**：检测在响应全发后调度，fire-and-forget，异常全捕获。
7. **绝不泄漏 `token_id:`**：恢复后响应不得含 `token_id:` 前缀；无文本处置 null。
8. **流式不缓冲**：纯 ASGI，SSE 增量转发，不缓冲整流。
9. **指标隔离**：独立 CollectorRegistry，不与下游 `/metrics` 混。
10. **终端幂等**：重复终端 body 消息不重复发送/不重复调度检测。

## 5. 验收标准总览

- 非目标路径/方法透传不改 body。
- 注入改 body 与请求 `Content-Length`；恢复按原始参数 null/截断/文本还原。
- completions 无 `token_id:` 泄漏（未请求 token-ids 时 nulling）。
- 流式增量转发 + 跨块事件重组 + `[DONE]`/keep-alive 透传 + 流后恰一次检测。
- 采样 0.0 不检测/1.0 全检测，注入恢复均发生。
- `x-anomaly-request-id` 头存在且唯一。
- 内联 metrics 200 + 正确 content-type + Prometheus 文本；下游无路由也作答。
- 降级：检测器不可构造 → 永久透传 + 指标报零 + 不改客户端响应。
- 检测器异常 → 计 error，客户端不受影响。
- vendored 检测器可构造（不抛 ImportError/NameError）。
- 单标志部署，构造 `(app)` 无 kwargs。
