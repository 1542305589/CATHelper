# 推理精度异常检测中间件 技术规格说明书(SPEC)

## 1. 目的与范围

本文档规定基于 vllm 的在线精度异常检测中间件 `vllm_anomaly_middleware` 的功能行为、输入输出契约与验收标准。
中间件通过 vLLM 的 `--middleware` 插件部署，监控推理异常现象，具体功能包括：拦截推理请求、
强制采集 logprobs 和 token_id、后台运行算法异常检测、不影响客户端请求响应状态返回、
并通过独立 Prometheus 端点暴露检测结果。整个过程客户端无感知。

适用范围：vLLM 的 `POST /v1/chat/completions` 与 `POST /v1/completions` 在线推理请求端点，
流式与非流式在线推理请求均覆盖。

## 2. 功能需求

### 2.1 请求拦截

中间件仅拦截 `POST /v1/chat/completions` 与 `POST /v1/completions`。
所有其他 HTTP 请求（任意方法或路径）和目标路径上的非 POST 方法均原样转发给下游应用（保持原先vllm处理方式一致）。

**验收**
- `GET /v1/models` → 原样转发。
- `GET /v1/chat/completions` → 原样转发。

### 2.2 强制 logprobs、token_id 采集

- 对每个被拦截请求，缓存客户端原始 logprobs 等相关采集参数，供响应恢复；

- 无视客户端请求原值，对请求体强制注入检测所需参数：chat 设 `logprobs=true`、
`top_logprobs=<N>`、`return_tokens_as_token_ids=true`；completions 设 `logprobs=<N>`、
`return_tokens_as_token_ids=true`，其中 N 默认设置为20。请求 `Content-Length` 修正为新 body 长度。


**验收**
- chat 请求下，客户端未带 logprobs 的  → 转发 body 含 `logprobs=true`/`top_logprobs=<N>`/
  `return_tokens_as_token_ids=true`，`Content-Length` 反映新长度。
- chat 请求下，客户端配置 `logprobs=True` 和 `top_logprobs=5` 且配置 N=20 → 转发 `top_logprobs=20`，原 5 被内部保留用于恢复。
- chat 请求下，客户端配置 `logprobs=True` 和 `top_logprobs=10` 且配置 N=5 → 转发 `top_logprobs=10`，原 10 被内部保留用于恢复。
- completions 请求下，客户端未带 logprobs 的  → 转发 body 含 `logprobs=<N>`/
  `return_tokens_as_token_ids=true`，`Content-Length` 反映新长度。
- completions 请求下，客户端配置 `logprobs=5`且配置 N=20 → 转发 `logprobs=20`，原 5 被内部保留用于恢复。
- completions 请求下，客户端配置 `logprobs=10`且配置 N=5 → 转发 `logprobs=10`，原 10 被内部保留用于恢复。
- chat 和 completions 请求下，客户端未带`return_tokens_as_token_ids=True`的 → 转发 body 含`return_tokens_as_token_ids=True`，原默认参数`return_tokens_as_token_ids=False` 被内部保留用于恢复。

### 2.3 客户端透明响应恢复

响应处理后恢复为客户端原始请求被满足时的形态：
- 客户端未请求 `logprobs`/`top_logprobs`  → `choice.logprobs` 置 null(vllm默认关闭)。
- 客户端请求 `logprobs=N`/`top_logprobs=N` → 各 top-logprobs 列表截断至 N(vllm默认关闭)。
- 客户端未请求`return_tokens_as_token_ids=True` → 恢复响应中不得出现任何 `token_id:` 前缀字符串；token 文本从响应 `bytes`/text 字段解码；无法解码处置 null 而非 `token_id:` 字符串(vllm默认关闭)。
- 适用于 chat 与 completions，流式与非流式。

**验收**
- chat 客户端未请求 采集 logprobs 和 token_id → 恢复后 `choice.logprobs=null`，全文无 `token_id:`。
- chat 客户端设置`logprobs=true`、`top_logprobs=3` → 截断 `top_logprobs`，取前 3 项数据，
  每项 `token` 为解码文本（非 `token_id:`）。
- chat 客户端设置`logprobs=true`、`top_logprobs=3`和`return_tokens_as_token_ids=True` →  截断 `top_logprobs`，取前 3 项数据，且每项都包含 `token_id:`。
- completions 客户端未请求 采集 logprobs 和 token_id → 恢复后 `choice.logprobs=null`，全文无 `token_id:`。
- completions 客户端设置`logprobs=3` → 截断 `top_logprobs`，取前 3 项数据，
  每项 `token` 为解码文本（非 `token_id:`）。
- completions 客户端设置`logprobs=3`、`return_tokens_as_token_ids=True` →  截断 `top_logprobs`，取前 3 项数据，且每项原样保留 `token_id:`。
- chat 和 completions 客户端设置`logprobs=10`，而推理服务环境变量设置top-logprobs 数 N=4  → body 内 top-logprobs 的数量取二者最大值 10，推理请求输出的每个 token 有 10 项数据，送至检测截断前 4 项数据，返回给客户端 10 项数据。


### 2.4 流式安全转发

对 `text/event-stream` 响应，保持流式响应原始机制，对 chunk 结果进行处理，缓存 logprobs 和 token_id 数据，转换为用户原始响应格式逐事件增量转发，不缓冲整流。终端 `data: [DONE]` 保留。
跨 body 块的半事件先重组再处理。


**验收**
- 多块 SSE 流 → 客户端随处理增量收到恢复块 + 终端 `data: [DONE]`，中间件不先全缓冲。
- 一条 SSE 事件被拆到两块 → 重组后处理，客户端收到一条完整事件。
- 流式响应中，处理 chunk 数据 → 中间件缓存整条 logprobs 和 token_id 数据用于检测，客户端输出格式参考2.3章节验收

### 2.5 异常检测执行

对每个被选中检测的请求：
- 针对单请求单输出，对输出 choice 抽取 top-k logprobs（token-id→logprob 映射）与
token-id 序列，提交检测后端；

- 针对单请求多候选输出的情况，循环抽取输出choice中的top-k logprobs（token-id→logprob 映射）与 token-id 序列，数据用列表存储，送入检测，检测结果分别上报，不能被覆盖。

检测结果按 `ill_type` 分类：
0=normal,1=rare_character,2=garbled,3=repetition,4=nan_value。
检测仅在响应全部发送给客户端后调度（fire-and-forget），客户端不等待检测。


**验收**
- 非流式请求 2 请求被选中 → 每个推理结果 choice 抽取 `(topk_logprobs, tokens)` 提交检测，
  返回每 choice 一个 `[is_ill, ill_type]`。
- 空响应（无生成 token，如错误或空 completion）→ 不提交检测，情况记录至日志。
- 流式响应结束后，用跨块缓存的 logprobs 和 token_id 数据提交检测，客户端不等检测。
- 客户端请求中设置 `n=3`（vllm 默认 n=1）→ 单独处理该请求，将 3 份数据提交检测，若有多份数据检出异常，分别上报


### 2.6 检测失败隔离

抽取、调度或检测期间任何异常须被捕获并记录，不得改变客户端响应、状态码或头部
（响应在检测时已发完）。检测失败计为 detection-error 指标。

**验收**
- 检测器运行中抛异常 → 客户端响应不受影响（已发完），计 detection-error，
  后续请求正常处理。

### 2.7 检测后端串行化

多请求下检测调用串行，单请求多候选输出下逻辑上并行调用检测（检测算法内部串行）。

**验收**
- 两被选中请求同时完成 → 其检测调用串行（一者等另一者），而非同实例并发。
- 单请求多候选输出 → 整合多候选输出数据，一起送入检测算法。

### 2.8 检测采样

支持可配置采样率 ∈[0.0,1.0]。请求以等于采样率的概率被选中检测。未选中请求直接透传，不对用户请求做处理。1.0 表示全检测，0.0 表示不检测。
默认 1.0。

**验收**
- 采样率 0.0 → 不提交检测，请求直接透传。
- 采样率 1.0 → 每请求都提交检测。
- 采样率 0.3  → 10 个请求里大概有 3 个请求会修改 body，其余请求直接透传

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
- 下游无 `/anomaly/metrics` 路由 → 中间件上报不会报错。

### 2.11 环境变量配置

全部运行时配置从环境变量读取（带合理默认），因构造除 app 外无参数。至少可配：
总开关；top-logprobs 数；指标路径；检测采样率；检测 worker 数；检测器 config/mtype/tk2cat
路径显式覆盖。

**验收**
- 总开关设置为 False → 不注入不检测（纯透传），但指标端点仍可达报零值计数。
- 设置top-logprobs 数为 5  → 推理结果每个token至少包含 5 个 logporbs 和 token_id


### 2.12 检测器数据路径解析

解析检测后端配置文件（`config.yaml`、`mtype_config.json`）与 `token2category/` 表。
顺序：优先 response_anomaly 算法检测文件夹已经固定在本项目下，固定路径方式调用；其次外部可导入的
`response_anomaly`/`msprobe.response_anomaly`；再次由用户指定 response_anomaly 路径，由中间件将 response_anomaly 代码拷贝到项目固定路径下。


**验收**
- 三种方式均要实现 → 使用时按上述顺序获取，若均未获取，将详细信息写入日志，降级为透传方式。

### 2.13 优雅降级——检测功能不可用

检测后端或其配置无法加载/解析 → 记录一次日志，对所有后续请求纯透传（不注入、不检测）。
指标端点仍可达报零值。降级绝不改变客户端响应。

**验收**
- 首个会被检测的请求上检测后端不可导入或路径不可解析 → 记录一次，停止注入与检测，
  后续请求原样转发；`GET /anomaly/metrics` 仍 200 报零值。

### 2.14 middleware 插件部署

仅加 `--middleware <module.path>.<ClassName>` 即可部署。构造函数仅接受一个参数
（被包装的 ASGI app）且无 kwargs，其中 ASGI 为 Asynchronous Server Gateway Interface。部署不要求 entry-point 注册、插件白名单或特定
vLLM 插件接口——仅需 vLLM 支持 `--middleware`。

**验收**
- `--middleware vllm_anomaly_middleware.AnomalyMiddleware` → 中间件以
  `AnomalyMiddleware(app)` 实例化，进程生命期内拦截目标请求。

## 3. 输入输出契约

### 3.1 构造与调用

- `AnomalyMiddleware(app)`：`app` 为下游 ASGI 可调用。
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
- 标签：`ill_type`(0-4)、`model`(来自请求 `model`，缺失用 `"unknown"`)。

## 4. 行为约束与不变量

1. **透明无条件**： `enabled=True` 和采样共同作用，决定请求是否注入、响应恢复和检测。
2. **降级即透传**：`enabled=False`（master 开关 off 或检测器不可构造）→ 不读 body、
   不注入、不拦截；指标端点独立可达。
3. **top_logprobs 跨请求恒定**：默认 20，可配 1-20，但运行期不可变。
   理由：检测器 `topk` 首次锁定后不复位。
4. **model_configs 即 model_name**：传模型名字符串；禁止传 `mtype_config` 的 dict 条目
   （会静默退化为无词表检测）。
5. **检测串行**：多请求检测调用不并发，单请求多候选输出物理上并发，检测内部串行。
6. **检测不阻塞客户端**：检测在响应全发后调度，fire-and-forget，异常全捕获。
7. **绝不泄漏 `token_id:`**：若用户请求未设置`return_tokens_as_token_ids=True`,恢复后响应不得含 `token_id:` 前缀；无文本处置 null。
8. **流式不缓冲**：纯 ASGI，SSE 增量转发，不缓冲整流，保持流式响应原始机制。
9. **指标隔离**：独立 CollectorRegistry，不与下游 `/metrics` 混用。

## 5. 验收标准总览

- 非目标路径/方法透传不改 body。
- 注入改 body 与请求 `Content-Length`；恢复按原始参数 null/截断/文本还原。
- chat 和 completions 无 `token_id:` 泄漏，未请求 return_tokens_as_token_ids 时 应将 token_id 转为 token。
- 流式增量转发 + 跨块事件重组 + `[DONE]`/keep-alive 透传 + logprobs 和 token_id 缓存+ 流式推理结束后调用检测。
- 采样 0.0 不检测/1.0 全检测。
- `x-anomaly-request-id` 头存在且唯一。
- 内联 metrics 200 + 正确 content-type + Prometheus 文本；下游无路由也作答，不报错。
- 降级：检测器不可构造 → 永久透传 + 指标报零 + 不改客户端响应 + 日志事件记录。
- 检测器异常 → 计 error，客户端不受影响，异常情况详细记录至日志。
- 单插件部署，构造 `(app)` 无 kwargs。
