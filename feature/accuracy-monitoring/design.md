# 推理精度异常检测中间件 设计文档(DESIGN)

## 1. 概述

### 1.1 设计目标

构建一个纯 ASGI（Asynchronous Server Gateway Interface） 中间件包 `anomaly_middleware`，通过 vLLM 的
`--middleware` 插件部署：

```
vllm serve <model> --middleware anomaly_middleware.AnomalyMiddleware
```

中间件对客户端**透明**：拦截推理请求、强制采集 logprobs和token_id、后台运行算法异常检测、不影响客户端请求响应状态返回、并通过独立 Prometheus 端点暴露检测结果。客户端完全感知不到中间件的存在。

### 1.2 设计原则

- **透明优先**：`enabled=True` 和异常监控概率共同作用，决定请求是否注入、响应恢复和检测。但不影响客户端看到的响应。
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
project_root/
├── pyproject.toml           # 项目配置（包名 anomaly_middleware）
├── conftest.py              # pytest 根配置（sys.path 设置）
├── configs/
│   └── detector.yaml        # 检测器算法默认参数
├── tests/                   # 单元测试 + 端到端测试
├── webui/                   # Web 精度可视化监控
├── docs/                    # 设计文档 + 规格 + README
└── anomaly_middleware/          # Python 包
    ├── __init__.py            # 重导出 AnomalyMiddleware / ResponseInterceptor / RequestContext
    ├── middleware.py          # 统一中间件类 + RequestContext + ResponseInterceptor + 预热
    ├── env.py                 # 处理环境变量
    ├── logging.py             # 日志格式
    ├── metrics.py             # 独立 CollectorRegistry + 指标记录/渲染
    ├── extractor.py           # 抽取/恢复（流式与非流式）+ SSEStreamProcessor
    ├── token_resolver.py      # TokenTextResolver + tokenizer 获取（argv/env/HTTP/缓存自动发现）+ parse_vllm_argv
    ├── token_categorizer.py   # token 分类纯函数 + 运行时 generate_tk2cat（§3.11）
    ├── detector.py            # ILLDetector 检测器本体（set_vocabulary + topk_n 参数）
    └── detector_runner.py     # DetectorRunner（线程池+锁+懒构造+调度+词表注入）
```

### 2.3 职责划分

| 组件 | 职责 |
|---|---|
| `AnomalyMiddleware` | 持有配置/runner/指标/待办任务集；`__call__` 分派：内联指标、降级透传、拦截注入、装拦截器、委托下游；预热线程 |
| `RequestContext` | 单请求上下文：原始参数、model、关联 id、是否检测 |
| `ResponseInterceptor` | 包装 `send`：判流式/非流式、注入关联头、缓冲或增量处理、恢复响应、调度检测 |
| `SSEStreamProcessor` | 跨块事件重组、每块恢复、per-choice 累积检测数据 |
| `Extractor` | 纯函数：解析 token-id、抽取 per-choice (topk,tokens)、恢复响应（截断/null/文本还原，经 `_token_text` 统一走 resolver 优先） |
| `DetectorRunner` | 单 worker 线程池 + 锁；worker 内懒构造检测器；`set_vocabulary` 注入词表；同步/异步执行；`topk_n` 参数 |
| `TokenTextResolver` | token_id(int)→单 token surface 文本（`decode([id])`）；进程级单例，懒构造，软降级；tokenizer 获取经 argv/env/HTTP/缓存自动发现 |
| `TokenCategorizer` | token 分类纯函数（`categorize_token`）+ 运行时 `generate_tk2cat(tokenizer)` 生成 `{token_id: category}` 映射（§3.11） |
| `ILLDetector` | 检测器本体：`set_vocabulary` 接受运行时映射；`topk_n` 参数消除首次锁定；`get_tk2cat` 返回映射或 (None,None) 降级 |
| `PluginConfig` | 环境变量读取与校验；检测器路径固定 `configs/detector.yaml` |
| `Metrics` | 独立 registry；计数/直方图/gauge；渲染文本暴露 |

## 3. 核心功能设计

### 3.1 请求拦截与参数注入

**拦截范围**：仅 `/v1/chat/completions` 与 `/v1/completions`。
所有其他 HTTP 请求（任意方法或路径）均原样转发给下游应用（保持原先vllm处理方式一致）。

**请求侧 ASGI 契约**：
1. **读请求体**：`_read_all_body(receive)` 循环 `receive()`，聚合全部 `http.request`
   body 至 `more_body=False`；遇 `http.disconnect` 中止。
2. **重放 receive**：`_make_replay_receive(original_receive, body, request_id)` 包装——
   首次调用返回合成单条 `{"type":"http.request","body":body,"more_body":False}`；
   **后续调用委托原始 `receive()`**（返回 `http.disconnect` 等真实后续消息）。
   ⚠ 二次读**禁止**返回空 body 的 `http.request`：vLLM 会将其视为新请求而重复
   处理/重复下发（实测缺陷）。透传与注入两条路径都要重放已消耗的 body。
3. **请求 scope**：`_patch_scope_content_length(scope, len)` 浅拷贝 scope，
   改写/补 `content-length` 为注入后新 body 长度。

**强制注入**（覆盖请求体以加上检测所需参数）：
- chat：`logprobs=true`、`top_logprobs=<注入值>`、`return_tokens_as_token_ids=true`
- completions：`logprobs=<注入值>`（此处 logprobs 即数量）、`return_tokens_as_token_ids=true`
- **注入值 = max(客户端原始值, N)**：客户端带 `top_logprobs`（chat）/
  `logprobs`（completions）=M 且 M>N → 注入 M，保证每 token 有 M 项数据（检测截断见
  §3.2）；否则注入 N。例：客户端 `top_logprobs=5`、N=20 → 注入 20；
  客户端 `top_logprobs=10`、N=5 → 注入 10（见 spec §2.2）。
- `return_tokens_as_token_ids` 始终注入 `true`；客户端未带 → 恢复其默认 `false`。

**快照**（`save_original_params`）：注入前缓存客户端原始 `logprobs`/
`top_logprobs`/`logprobs`、`return_tokens_as_token_ids` 与 **`n`** 等采集参数，供
§3.2 响应恢复。注入后修正请求 `Content-Length` 匹配新 body 长度（否则下游解析长度
不匹配导致截断/挂起）。

**关键不变量**：`top_logprobs` 跨请求必须恒定（默认 20，可配置 1-20）。
原因：保证每 token 的 top-logprobs 条目数一致，检测语义稳定（§6.2）。
`topk_n` 由参数传入检测器，不再依赖实例态锁定（§4.5）。

### 3.2 响应抽取与恢复

**抽取**（供检测）：每个 choice 取 `(topk_logprobs: list[dict[int,float]],
tokens: list[int])`。**已实测确认**：注入 `return_tokens_as_token_ids=true` 后
chat/completions 各 token 字段确为 `"token_id:NNN"`。
- chat：logprobs 位于 `choices[].logprobs.content[]`。每 entry 的
  `token` = `'token_id:NNN'`（解析为 int）；`top_logprobs[]` 为对象列表，每项
  `TopLogprob(token='token_id:1122', bytes=[22,33,55], logprob=-0.3)`——含独立
  `token` 字段（同解析为 key）与 `logprob` 值。
- completions：logprobs 位于 `choices[].logprobs`：
  `tokens[]`（`'token_id:NNN'` 字符串序列）解析为 token-id 序列；
  `token_logprobs[]` 为与 `tokens` 平行的 logprob 数值列表；
  `top_logprobs[]` 为与 `tokens` 平行的 dict 列表（token-id 字符串→logprob），
  解析为 dict[int,float]。

`parse_token_id` 兼容两种形态：`"token_id:NNN"` 与纯数字串（如 `"22"`），失败返回 -1。

**恢复**（供客户端，按原始参数，统一走 `_token_text` 规则，见 §3.10）：
- 客户端未请求 `logprobs` → `choice.logprobs=null`。
- 客户端请求 `logprobs=True`、`top_logprobs=n`（chat）/`logprobs=n`（completions）→ 截断到 n。
- 客户端未请求 `return_tokens_as_token_ids` → token_id→文本还原（§3.10）：
  - 统一规则 `_token_text(token_id, bytes, resolver, *, fallback_to_id=False)`：**resolver 优先** `decode([id])`；
    resolver 缺失/未解析 → 退回 `bytes`（仅当解码出真实文本且不含 `token_id:` 前缀）；都无 → `fallback_to_id=True`
    时回退 `token_id:NNN`（§4.7 降级例外），否则 null。
  - chat `content[].token` / `top_logprobs[].token`：resolver 覆盖所有字段；resolver 不可用时主 token 退回 bytes
    （三层第二层），top_logprobs 跳过破损 bytes（三层第二层失败）→ 触发例外时落 `token_id:NNN`（三层第三层），
    未触发时 null。
  - completions `tokens[]` / `top_logprobs[]`：无 bytes，resolver 可用时还原真实文本、不可用且触发例外时
    回退 `token_id:NNN`、未触发时 null；`top_logprobs[]` 重建为 `{文本或token_id:NNN:logprob}`。
- 客户端**已**请求 `return_tokens_as_token_ids` → 原样保留 `token_id:NNN`（这正是客户端所要）。

> **背景**：vLLM 在 `return_tokens_as_token_ids=true` 下，chat `top_logprobs` 的 `bytes` 填的是
> token_id 字符串本身的字节（非 token 真实字节）→ `_decode_bytes` 会泄漏 `token_id:`；
> completions 响应形态本就无 `bytes` → 仅靠 bytes 路径无法还原文本。故引入 tokenizer `decode([id])`
> 为统一文本来源，bytes 仅作 resolver 不可用时的兜底（带泄漏守卫）。

**检测截断与客户端截断分离**：注入值为 `max(客户端, N)` 时每 token 的 top-logprobs
条目数可能 > N。抽取检测数据（`extract_*_response`）时每 token **截断至 N**（检测器
`topk=N` 锁定，见 §6.2）；恢复给客户端时**截断至客户端请求值**。例：客户端 `logprobs=10`、
N=4 → 注入 10、每 token 10 项；送检测截前 4 项，返回客户端 10 项（见 spec §2.3）。

**多候选（`n>1`）**：客户端设置 `n` 被快照保留；抽取/恢复/检测按 choice 循环处理 n 份
候选，客户端输出逐 choice 套用上述规则（见 spec §2.3）。

### 3.3 流式响应处理（SSE）

**流式形态**（**已实测确认**）：vLLM 流式每块只含**最新 token** 的 logprobs 和 token_id。
设计要求**先缓存全部流式推理结果，再进行检测**。

- chat 流式：logprobs 位于 `choices[].logprobs.content[]`（**非 delta**），每块含本块
  新 token 的一个 entry（`token='token_id:NNN'`、`logprob`、`bytes`、`top_logprobs[]`
  对象列表，形态见 §3.2）。累积器对每块 content 的每个 entry 做 append。
- completions 流式：logprobs 位于 `choices[].logprobs`，每块 `tokens[]`/
  `token_logprobs[]`/`top_logprobs[]`（形态见 §3.2），append。
  - 防御：若某块 `tokens`/`top_logprobs` 呈现累积数组（位置与已累积重叠），采用
    **latest-longest-wins**（取最长/最新数组覆盖），兼容可能的累积式。默认按 delta-append。

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

### 3.5 异常监控概率

- 异常监控概率方法：每目标请求抽 `rand = random.random()`；
  `will_detect = rand < monitor_rate`（默认 1.0，范围 0-1）。
- 未选中（`will_detect=False`）→ **纯透传**：不读 body、不注入、不恢复、不检测，
  原样转发给下游（spec §2.8）。
- 选中 → 该请求完整走读 body→注入→恢复→检测链路。`monitor_rate=0` 永不注入不检测；
  `1.0` 全检测。


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
- `VLLM_ANOMALY_MONITOR_RATE`（默认 1.0，范围 0-1）：请求被异常监控的概率。
  `0` 永不注入不检测；`1.0` 全检测。
- `VLLM_ANOMALY_TOP_LOGPROBS`（默认 20，范围 1-20）
- `VLLM_ANOMALY_METRICS_PATH`（默认 `/anomaly/metrics`）
- `VLLM_ANOMALY_TOKENIZER_MODEL`（默认未设）：显式 tokenizer 加载源（最高优先），设为
  `vllm serve --model` 的实际值或 `--tokenizer` 的值（本地目录路径或 HF repo id）。覆盖 served 名为裸 basename /
  本地目录部署。未设则自动从同进程 `sys.argv` 解析 `vllm serve` 命令行：
  `--tokenizer` → `--model` 位置参数 → loopback `/v1/models` → HF 缓存扫描（§3.10）。
  设定时额外触发后台预热线程提前加载 tokenizer + 生成 tk2cat 映射（§3.11）。

校验：`top_logprobs∈[1,20]`、`monitor_rate∈[0.0,1.0]`。

检测器配置路径固定为 `configs/detector.yaml`（项目根目录），不可通过 env 覆盖。
文件不存在 → `None`（触发降级，§3.9）。

### 3.9 降级机制

- 检测器/配置无法加载或解析 → 记录一次日志，置 `enabled=False` 永久降级。
- 降级后：`__call__` 早退透传（不读 body、不注入、不拦截），但指标端点仍可达报零值。
- 触发点：仅在首个 `will_detect=True` 请求上尝试构造 runner（见 §6.1）。
  `monitor_rate=0` 则永不触发，runner 永不构造，指标报零。

### 3.10 token 文本还原（TokenTextResolver）

**背景与问题**：中间件强制注入 `return_tokens_as_token_ids=true`，使响应 token 字段呈
`"token_id:NNN"`（供检测抽取 token_id）。客户端**未**请求 `return_tokens_as_token_ids` 时，
中间件须把 `token_id:` 还原为 token 文本回客户端（spec §2.3、不变量 #7「`token_id:` 限制」）。
`_decode_bytes` 从 `bytes` 字段解码仅能覆盖 chat 主 token（实测正确）；对 chat `top_logprobs[].token`
会泄漏 `token_id:`（vLLM 把 token_id 字符串本身的字节塞入 `bytes`），对 completions 各字段无 `bytes`
可解。根因：`bytes` 路径无法覆盖这两种情形，须引入真正的 tokenizer 做 `decode([id])`。

**职责与接口**：`TokenTextResolver.resolve(token_id: int) -> Optional[str]`——给定 token_id 返回
该 token 的 surface 文本（OpenAI 语义即 `decode([id])`），不可用返回 `None`（调用方置 null）。
仅被 ASGI 事件循环（strip 路径）调用；检测 worker 线程不调用（检测用 token_id 整数）。
进程内单例、一次加载、全请求复用。

**tokenizer 获取顺序**（lazy，首注入请求触发，`acquire_tokenizer(model_hint, server, explicit)`）：

1. **显式 env `VLLM_ANOMALY_TOKENIZER_MODEL`**（最高优先）：设为 `vllm serve --model` 实际值
   或 `--tokenizer` 的值（本地目录路径或 HF repo id）→ `from_pretrained(explicit, local_files_only=True)`。
   覆盖本地目录部署（served 名为裸 basename、不在 HF 缓存、from_pretrained 与缓存扫描均无法解析）。
   未设则跳过。
2. **`--tokenizer` 从 `sys.argv` 解析**（`parse_vllm_argv()`）：vLLM 启动命令中 `--tokenizer <path>`
   即 vLLM 实际使用的 tokenizer 路径——与 `--model` 不同时（如使用独立 tokenizer），此为最精确来源。
   `parse_vllm_argv()` 解析 `vllm serve <model> ... --tokenizer <path> ... --host H --port P`，
   支持 `--flag value` 与 `--flag=value` 两种形式；非 `serve` 命令返回 None。
3. **`--model` 位置参数从 `sys.argv` 解析**：无 `--tokenizer` 时，`vllm serve <model>` 的 `<model>`
   即 tokenizer 路径（vLLM 默认从模型目录加载 tokenizer）。`parse_vllm_argv()` 提取 serve 后首个
   位置参数（跳过 `_VALUE_FLAGS` 中已知带值 flag 的值，避免误识别）。
4. **`from_pretrained(model_hint, local_files_only=True)`**：`model_hint` = 请求体 `model` 字段（免 HTTP）；
   命中 HF repo id / 本地路径。`local_files_only=True` → **零外网**。
5. **loopback `GET /v1/models`**（async httpx，一次性）取 vLLM 实际 serve 的 root 路径与 served id →
   `from_pretrained(root或served, local_files_only=True)`；root 优先于 served（root 为模型真实路径）。
6. **HF 缓存扫描**：served/model 名为裸 basename（如 `Qwen3-0.6B`）而 HF 缓存键为完整 repo id
   （如 `Qwen/Qwen3-0.6B`）时，`huggingface_hub.scan_cache_dir()` 找 `repo_id` 以 `/<hint>` 结尾或
   等于 `<hint>` 的条目（短优先），补全后重试 `from_pretrained`。`huggingface_hub` 不可用 → 返回 []。
7. 均失败 → 记一次 `logger.error`（提示用户设置 `VLLM_ANOMALY_TOKENIZER_MODEL`），resolver 进入
   「不可用」终态：后续 `resolve` 恒返回 `None`，不再重试（避免每请求开销）。
   终态下 §4.7 降级例外仍生效：触发条件命中（客户端请求 topk + 未设 rtati）的请求，受影响字段
   由 strip 函数回退 `token_id:NNN`（保证 topk logprob 数据不丢失，chat 主 token 仍优先 bytes 兜底）；
   未触发的请求维持 null。


**文本缓存**：`resolve` 内部维护 `dict[int, str]` 缓存，首次 `decode([id])` 后存入，后续命中微秒级。
缓存仅被事件循环单线程访问，无需锁；容量上界为实际出现过的 token id 数（远小于词表）。

**生命周期与触发点**：resolver 进程级、懒构造、双检锁；触发于**首个被注入请求**（runner 构造成功后、
`await self.app` 前，`_ensure_resolver(model_hint, server)`）。与 `_ensure_runner` 关系：runner 失败 →
整体降级透传（不注入、不 strip）→ 无需 resolver。故 `_ensure_resolver` 仅在注入路径、runner 就绪后调用。
resolver 失败为**软降级**：仍注入、仍 strip，只是 resolver 相关字段按 §4.7 例外分流
（触发例外 → `token_id:NNN`，未触发 → null/bytes）；与 runner 硬降级不同。
`AnomalyMiddleware.shutdown()` 无特殊清理（tokenizer 随进程退出）。

**strip 路径统一规则**（`extractor.py` `_token_text(token_id_value, bytes_value, resolver, *, fallback_to_id=False)`）：

```python
def _token_text(token_id_value, bytes_value, resolver, *, fallback_to_id=False):
    # 1) 优先 resolver：覆盖 chat 主 token / top_logprobs / completions 全部字段
    if resolver is not None:
        tid = parse_token_id(token_id_value)
        if tid >= 0:
            txt = resolver.resolve(tid)
            if txt is not None:
                return txt
    # 2) resolver 缺失 / 未解析 → 退回 bytes（仅当解码出真实文本，不含 token_id: 前缀）
    if bytes_value is not None:
        s = _decode_bytes(bytes_value)
        if s is not None and not s.startswith(TOKEN_ID_PREFIX):
            return s
    # 3) 都无 → §4.7 降级例外：fallback_to_id=True 时回退 token_id:NNN，否则 None
    if fallback_to_id:
        tid = parse_token_id(token_id_value)
        if tid >= 0:
            return f"{TOKEN_ID_PREFIX}{tid}"
    return None
```

要点：步骤 1 优先 resolver，文本来源统一、一致；步骤 2 的 `not s.startswith(TOKEN_ID_PREFIX)`
守卫**独立修复泄漏**——resolver 不可用时，chat top_logprobs 的破损 bytes 也被识别并跳过，
不再泄漏 `token_id:`；步骤 3 为 §4.7 降级例外——`fallback_to_id=True` 时回退 `token_id:NNN`，
保证 resolver 不可用时客户端仍可获取 topk logprob 数据。`fallback_to_id` 由 strip 函数按
触发条件计算（见下），默认 `False` 维持原 null 行为（向后兼容）。

**触发条件**（`strip_chat_response` / `strip_completions_response` 头部各算一次）：
- chat：`not orig.return_tokens_as_token_ids and resolver is None and orig.logprobs is True and (orig.top_logprobs or 0) > 0`
- completions：`not orig.return_tokens_as_token_ids and resolver is None and (orig.logprobs or 0) > 0`

**三层兜底**（chat 主 token 在触发时）：resolver → bytes（真实文本）→ `token_id:NNN`。
bytes 仍优先用——vLLM 已填了主 token 的真实 bytes，不用客户端自解码；仅在 bytes 破碎
（解码出 `token_id:` 前缀，守卫拒绝）时落 `token_id:NNN`。chat top_logprobs 的 bytes 是
`token_id:` 字符串本身字节（破损），故三层第二层必失败、直接落第三层。completions 无 bytes
字段，二层直接跳过、落第三层。

**行为变更**（相对引入前）：① chat `top_logprobs[].token`：原泄漏 `"token_id:NNN"` → 无 resolver
时 null、有 resolver 时真实文本（**bug 修复**）；② completions `tokens[]`/`top_logprobs[]`：原恒 null
→ 有 resolver 时真实文本、无 resolver 时 null（维持）；③ **本次新增（§4.7 降级例外）**：触发条件命中
（客户端请求 topk + 未设 rtati + resolver 不可用）时，受影响字段由 null 改为 `token_id:NNN`——
保证客户端仍可获取 topk logprob 数据，chat 主 token 仍优先 bytes 兜底；④ chat 主 token：文本来源
由 bytes 改为 **resolver 优先**（resolver 不可用时退回 bytes），可观察文本值不变。

### 3.11 token 分类与词表注入

检测器的生僻字（rare_character）与乱码（garbled）检测依赖 token 到类别的映射
（`tk2cat`：`{str(token_id): category}`），用于判断输出中是否出现非常规字符类别
（生僻 CJK / 乱码符号 / 控制字节等）。映射在运行时从已加载 tokenizer 直接生成，
不依赖预生成文件。

**token 分类**（`token_categorizer.py` `categorize_token`）：对单个 token 的解码文本
逐字符做 Unicode 脚本分类（`_classify_char`，`lru_cache` 加速），统计各类别占比，
取主导类别映射为类别标签（如 CJK→`chinese_cjk`、拉丁→`english_latin`、数字→`numbers`、
符号密集→`gibberish_symbols`、控制字节→`control_bytes` 等）。纯函数、无副作用，
供检测器与中间件共享。

**运行时映射生成**（`generate_tk2cat(tokenizer) -> (id_to_category, vocab_size)`）：
1. `tokenizer.get_vocab()` 取词表 → `invert_vocab` 反转为按 index 排序的 token 字符串列表；
2. decode 降级链（`_get_decode_fn`）优先 `backend_tokenizer.decoder.decode([token])`
   （最精确），退到 `tokenizer.decode([idx])`（高层 API）；均无则 raise（调用方降级）；
3. 逐 token `_safe_decode`（异常吞掉、跳过该 token）→ `categorize_token` →
   `{str(token_id): category}`。

**注入与降级**：映射经 `DetectorRunner.set_vocabulary(tk2cat, vocab_size)` 注入，
worker 线程懒构造检测器时同步注入（`_get_detector` 兜底）。检测器 `get_tk2cat()`
返回预计算映射或 `(None, None)`；后者降级为**无词表检测**：rare/garbled 走 top1 logp
路径（按概率阈值判异常），repetition/acf/trajectory 不受影响。

**预热**（构造时始终触发）：中间件构造时启动 daemon 线程 `_start_preheat`，模型路径来源优先级：
1. `VLLM_ANOMALY_TOKENIZER_MODEL`（显式 env）；
2. `parse_vllm_argv()` 解析 `--tokenizer`（argv）→ vLLM 实际 tokenizer 路径；
3. `parse_vllm_argv()` 解析 `--model`（argv，无 `--tokenizer` 时 fallback）；
4. `poll_model_root((host, port))` 轮询 loopback `/v1/models` 取 root（argv 解析失败时 HTTP 兜底）；
5. 均失败 → `logger.error` 提示用户设置 `VLLM_ANOMALY_TOKENIZER_MODEL`，预热放弃（首请求慢路径补生成）。

路径就绪后 `_from_pretrained` 加载 tokenizer → 设 resolver（strip 路径可用）→ `generate_tk2cat` 生成映射。
预热在首请求 runner 构造之前完成则注入 runner；否则 `_ensure_resolver` 慢路径补生成 + 注入。
tk2cat 生成失败不影响 resolver（仍注入、仍 strip，检测降级为无词表）；resolver 失败为软降级（§3.10）。

## 4. 关键组件设计

### 4.1 AnomalyMiddleware（统一中间件类）

**构造** `__init__(self, app)`：建 `PluginConfig`；持有未初始化 runner 占位；
attach 指标助手；建 `_pending_tasks` set 与 `_runner_lock`/`_runner_inited`、
`_resolver`/`_resolver_inited`/`_resolver_lock`（§3.10）、`_tk2cat`/`_vocab_size`/`_preheat_thread`
（§3.11）。**不做重活**（无 numpy、无文件读）；若 `enabled=True` 则始终启动预热线程
`_start_preheat()`（§3.11，daemon 线程经 argv/env/HTTP 自动发现 tokenizer 路径并提前加载 +
生成 tk2cat，无需用户额外配置）。

**`__call__(scope, receive, send)` 分派**：
1. 非 http scope → 透传 `self.app`。
2. `GET <metrics_path>` → 内联 `_serve_metrics`。
3. 非 POST、非目标路径、或 `enabled=False` → 透传（不读 body）。
4. 异常监控概率：`will_detect = random.random() < monitor_rate`；未选中 → 纯透传（不读 body、
   不注入、不恢复、不检测，见 §3.5）。
5. 选中：`_read_all_body(receive)` 聚合 body → `json.loads`；非 dict/非 JSON →
   `_make_replay_receive(receive, raw, request_id)` 原样重放透传。
6. `_ensure_runner()`（双检锁）；失败→`enabled=False`→本请求重放透传。
7. `save_original_params` → `inject_params`（注入值 max(客户端,N)）→
   `_patch_scope_content_length(new_body_len)` → 建 `RequestContext`
   (orig/model/request_id/will_detect) →
   `resolver = await _ensure_resolver(model, scope.get("server"))`（§3.10，软降级返回 None）→
   `replay_receive = _make_replay_receive(receive, new_body, request_id)` →
   装 `ResponseInterceptor`（透传 `resolver`）→ `await self.app(new_scope, replay_receive, interceptor)`。

**`_make_replay_receive(original_receive, body, request_id)`**：首次调用返回合成
`{"type":"http.request","body":body,"more_body":False}`；**后续调用委托
`await original_receive()`**，绝不返回空 body 的 `http.request`（vLLM 会重复处理请求）。
透传（非 JSON/非 dict）与注入两条路径都经此包装——body 已被读走必须重放。

**`_ensure_runner` 顺序前置**：runner 构造（廉价）在注入之前；失败则本请求纯透传，
避免半注入响应。runner 构造时若有预热生成的 `_tk2cat` 则同步 `set_vocabulary` 注入。
重头检测器在 worker 线程内首次检测时构造（见 §6.1）。

**`_ensure_resolver(model_hint, server)` 顺序前置**（§3.10 / §3.11）：resolver 构造在注入路径、
runner 成功后；**软降级**（失败返回 None 不抛）：仍注入、仍 strip，token 文本按 §4.7 例外分流
（触发例外 → `token_id:NNN`，未触发 → null/bytes）。
快路径（resolver 已就绪，可能由预热线程设置）：补调 `runner.set_vocabulary` 覆盖竞态窗口
（预热在 `_ensure_runner` 之后完成）。慢路径：`acquire_tokenizer` 加载 tokenizer → 设 resolver →
`generate_tk2cat` 生成映射 → 注入 runner（tk2cat 失败不影响 resolver，仅检测降级无词表）。
双检锁同 `_ensure_runner`；`finally` 置 `_resolver_inited` 避免每请求重试昂贵 tokenizer 加载。
内部调 `acquire_tokenizer(model_hint, server, explicit=self.config.tokenizer_model)`，顺序见 §3.10。
**`_serve_metrics(send)`**：`render_metrics()` → 200 + 正确 content-type +
正确 content-length，作为完整 ASGI 响应发出。

### 4.2 ResponseInterceptor（响应拦截器）

**构造**：`(send, *, ctx, runner, metrics, pending_tasks, resolver=None)`。`resolver`
为进程级共享引用（`TokenTextResolver` 或 None，§3.10），透传至流式 `SSEStreamProcessor` 与
非流式 `_process_complete` 的 `strip_*_response`。
状态：`_is_streaming`、`_start_msg`、`_body_buf`、`_sse`、`_finished`、`_detection_scheduled`、`_detection_results`。

**`__call__(message)` 分派**：
- `http.response.start` → `_on_start`：判 `content-type` 含 `text/event-stream`；
  注入 `x-anomaly-request-id`；流式建 `SSEStreamProcessor(is_chat, orig, top_logprobs, resolver)` 并立即 send(start)；
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
成功→`extract_*_response` 得 per-choice 存 `_detection_results`，再 `strip_*_response(data, orig, self._resolver)`
（§3.10 resolver 优先还原文本），`json.dumps(...).encode()` 返回。

**`_send_start(final)`**：从 `_start_msg` patch headers：改写/补 `content-length`，
注入 `x-anomaly-request-id`，send。

**`_maybe_schedule_detection`**：
```
if not will_detect or _detection_scheduled or _runner is None: return
_detection_scheduled = True
try: topk, tokens = _get_detection_inputs()
except: log; return
if not tokens or not any(tokens): return
schedule_detection(_runner, topk, tokens, request_id=_request_id, model=_model, metrics, pending_tasks)
```
`_get_detection_inputs`：非流式取 `_detection_results`；流式取 `_sse.get_detection_data()`。
两者返回 2-元组 `(topk, tokens)`（无 model_configs——model 仅用于指标标签）。

### 4.3 SSEStreamProcessor

见 §3.3 状态机。要点：
- `feed(chunk)->bytes`：缓冲 + `\n\n` 切分 + `_process_event`。
- `flush()->bytes`：排空尾部。
- `get_detection_data()->(topk_all, tokens_all)`（2-元组）。
- 构造增 `resolver` 形参；`_strip_streaming` 调 `strip_*_response(parsed, orig, self._resolver)`
  透传 resolver（§3.10）；`_extract_streaming`（检测累积）**不变**——继续用 token_id 整数。
- 累积（有状态）与每块恢复（无状态）并存。

### 4.4 DetectorRunner

**构造** `(config_path, max_workers=1, topk_n=None)`：`ThreadPoolExecutor`
+ `threading.Lock`。`topk_n` 为检测器 topk 截断参数（来自 `PluginConfig.top_logprobs`）。
持运行时词表缓存 `_tk2cat`/`_vocab_size`（由 `set_vocabulary` 注入）。

**`set_vocabulary(tk2cat, vocab_size)`**：注入运行时生成的 token2category 映射（幂等，
覆盖）。已构造的检测器同步注入；未构造时由 `_get_detector` 兜底注入。供 middleware
预热线程 / 慢路径调用（§3.11）。

**`_get_detector()`**（worker 线程内懒构造）：首次调用时
`from .detector import ILLDetector` 并构造（仅 config.yaml，不阻塞 event loop）；
若有注入的 `_tk2cat` 则同步 `set_vocabulary`。构造失败标记 `_unusable`，后续快速失败计 error。

**`run_sync(topk, tokens)`**：`with self._lock: detector=_get_detector();
return detector.run(topk, tokens, topk_n=self._topk_n)`。

**`run_async(topk, tokens)`**：`loop.run_in_executor(self._executor, self.run_sync, ...)`。

**`schedule_detection(runner, topk, tokens, *, request_id, model, metrics, pending_tasks) -> Task`**：
内部 `_run()`：`detection_duration.time()` 计时 → `runner.run_async(...)` →
`record_detection(results, model)`；except → `record_error()`。`model` 仅用于指标标签，
不参与检测（检测不再需要 model_configs）。

### 4.5 检测器契约要点

> **vendored 含义**：`detector.py` 是检测算法源码被**直接内置进本项目包**
> （vendor 进项目），随中间件分发。`configs/detector.yaml` 是其算法默认参数。
> 配置路径固定见 §3.8（固定 `configs/detector.yaml` → None 降级）。

- 构造 `ILLDetector(config_path)`：仅加载 `detector.yaml`（算法阈值），
  无模型识别文件、无预生成映射文件依赖。
- **`set_vocabulary(tk2cat, vocab_size)`**：接受运行时生成的 `{str(token_id): category}`
  映射（§3.11）。幂等，重复调用覆盖。`get_tk2cat()` 返回注入的映射或 `(None, None)`
  （未注入 → 无词表降级）。
- **`topk_n` 参数**：`run(topk, tokens, topk_n=N)` 由参数传入 topk 截断值，
  消除实例态 `topk` 首次锁定问题（`top_logprobs` 仍须跨请求恒定以保语义一致，
  但不再因实例态复位缺陷强制）。
- 批量入口：`run(topk: list[list[dict[int,float]]], tokens: list[list[int]],
  topk_n: int) -> list[[bool,int]]`。
- 实例态副作用：`_garbled_count` 每请求复位。单 worker + 锁保护实例态。
- 无词表降级：`tk2cat` 为 `None` 时，rare/garbled 走 top1 logp 路径
  （按概率阈值判异常），repetition/acf/trajectory 不受影响。

### 4.6 Config / Metrics

- `env.py`：`PluginConfig`（env 读取+校验）；`resolve_config_path()` 返回固定路径
  `configs/detector.yaml`（项目根目录），文件不存在 → None 降级。
- `metrics.py`：独立 registry；`record_detection(results, model)`、`record_error()`、
  `render_metrics()->bytes`、`METRICS_CONTENT_TYPE`。

### 4.7 TokenTextResolver

**职责**：token_id(int)→单 token surface 文本（`decode([id])`）；进程级单例，懒构造，软降级。
仅被 strip 路径（事件循环）调用，检测侧用 token_id 整数、不调用 resolver（§3.10）。

**构造** `TokenTextResolver(tokenizer)`：持 tokenizer 引用与空 `dict[int,str]` 缓存。

**`resolve(token_id) -> Optional[str]`**：`int(token_id)` → 命中缓存直接返回；否则
`tokenizer.decode([tid])`，非空存入缓存并返回、空则缓存 `None` 并返回 `None`。`decode` 对个别 id
抛错由 try/except 吞为 `None`（该处置 null），不影响其余。

**`acquire_tokenizer(model_hint, server, explicit) -> Optional[Any]`**（async，`token_resolver.py`）：
tokenizer 获取经 argv/env/HTTP/缓存自动发现，顺序见 §3.10。`_from_pretrained` 为间接层（便于测试 monkeypatch）。
`parse_vllm_argv(argv=None) -> Optional[VllmArgvInfo]` 解析 `vllm serve <model> ... --tokenizer <path> ...
--host H --port P`，返回 `VllmArgvInfo(model, tokenizer, host, port)` 或 None（非 serve 命令）；
`parse_vllm_server_from_argv(argv=None) -> Optional[(host, port)]` 为向后兼容封装。
`_fetch_model_info(server)` 走 loopback `GET /v1/models`（async httpx，timeout 5s，失败 None）。
`poll_model_root(server, timeout)` 同步轮询 loopback `/v1/models` 取 root（预热线程用，服务监听前轮询等待）。
`_scan_hf_cache_candidates(hint)` 走 `huggingface_hub.scan_cache_dir()`，不可用返回 []。

**并发**：`resolve` 仅事件循环单线程调用，`dict` 缓存无锁；`acquire_tokenizer` 仅在
`_ensure_resolver` 双检锁内首请求调用一次（§6.4）。loopback HTTP 用 async httpx 非阻塞，
事件循环可同时处理 `/v1/models`；中间件对 GET 放行，重入安全。

**降级**：resolver 不可用为**软降级**——strip 按 §4.7 例外分流：触发例外（客户端请求 topk + 未设
rtati）→ 受影响字段回退 `token_id:NNN`（保证 topk logprob 数据不丢失，chat 主 token 仍优先 bytes 兜底）；
未触发 → chat 主 token 走 bytes、其余 null、completions null，全文无 `token_id:`（§3.10 / §4.7）；
不影响检测、不影响客户端响应完整性。

## 5. 数据流

### 5.1 chat 非流式
```
client POST /v1/chat/completions (无 logprobs)
  → 读 body, parse JSON; will_detect (say True)
  → _ensure_runner (双检锁, 廉价; 预热有 tk2cat 则注入); ok
  → save_original_params; inject(logprobs/top_logprobs/return_tokens_as_token_ids)
  → patch 请求 CL; _ensure_resolver(软降级, 可 None; 慢路径补生成 tk2cat 注入); 装 ResponseInterceptor(透传 resolver)
   → app(...) 返回 choices[].logprobs.content[] (含 bytes, token="token_id:NNN")
   → _on_start(缓冲); _on_body(缓冲到 more_body=False)
        _process_complete: extract→存检测数据(topk,tokens); strip(resolver)→logprobs=null, token_id→文本(resolver 优先, bytes 兜底)
        _send_start(注入关联头 + patch 响应 CL); send(terminal body)
   → 调度检测(topk, tokens, request_id, model)
client 收到: logprobs=null, 无 token_id:, 带 x-anomaly-request-id
worker: run_sync→ILLDetector.run(topk, tokens, topk_n)→[[is_ill,ill_type]]→record_detection
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
worker: 累积全部 token 的 ILLDetector.run(topk, tokens, topk_n) → record_detection
```

### 5.3 completions
注入 `logprobs=<N>` + `return_tokens_as_token_ids=true`；恢复时若客户端未请求
`return_tokens_as_token_ids` → `tokens[]` 经 `_token_text(t, None, resolver)` 还原（resolver 可用为
真实文本，不可用为 null，**绝不留 `token_id:`**）；`top_logprobs` dict 截断到请求 N、重建为 `{文本:logprob}`
（resolver 不可用时落 null）。

## 6. 并发与生命周期

### 6.1 两段式懒加载

- **廉价阶段**（请求路径，首个 `will_detect` 请求）：`_ensure_runner()` 构造
  `DetectorRunner`——仅线程池 + 锁 + config 路径，无 numpy/文件 I/O。若有预热生成的
  `_tk2cat` 则同步 `set_vocabulary` 注入。失败（路径解析不出）则置
  `enabled=False` 永久降级。
- **重阶段**（worker 线程内，首次检测）：`_get_detector()` 懒构造 `ILLDetector`
  （numpy + config.yaml），位于请求路径之外，不影响首请求延迟。构造时若有注入的
  `_tk2cat` 则同步注入检测器。
- **预热线程**（构造时，`enabled=True` 即启动）：`_start_preheat()` 启动 daemon 线程，经优先级链
  （env → `--tokenizer`(argv) → `--model`(argv) → HTTP root → error）自动发现 tokenizer 路径，
  提前 `_from_pretrained` 加载 + `generate_tk2cat` 生成映射，在首请求 runner 构造之前完成则注入。
  预热失败（路径发现失败 / tokenizer 加载失败）不影响首请求（`_ensure_resolver` 慢路径补生成）。
- 双检锁：`if _runner_inited: return; with lock: if _runner_inited: return; ...; finally: _runner_inited=True`。
  `finally` 保证无论成败只跑一次，避免每请求重试昂贵导入。GIL 下 bool 写入在锁内安全。

### 6.2 检测串行化

- 检测器 CPU-bound（numpy/FFT），且每次 `run` 突变实例态（`_garbled_count`），
  不得阻塞 event loop、不得与自身并发。
- 单 worker 线程池自然串行；`threading.Lock` 为未来 `max_workers>1` 的 belt-and-suspenders。
- `run_sync` 在锁内调 `detector.run(topk, tokens, topk_n)`。

### 6.3 检测任务生命周期

- fire-and-forget：`asyncio.create_task`，异常全捕获。
- 防 GC：`_pending_tasks` 持引用，`done_callback` 出集。
- 关闭：未完成任务随 loop 取消（结果丢失不影响客户端）。

### 6.4 resolver 生命周期

- `_ensure_resolver` 双检锁（同 §6.1 `_ensure_runner`）；`finally` 置 `_resolver_inited`，
  避免每请求重试昂贵加载。与 runner 不同：resolver 失败为**软降级**（返回 None 不抛，
  不改 `enabled`），仍注入、仍 strip。
- 首请求 init 可能含一次 argv 解析 + 一次 loopback `GET /v1/models` + 一次本地 tokenizer 加载（百毫秒级），
  一次性、进程生命期复用；与首请求 runner 构造同窗，可接受。
- loopback HTTP 用 **async httpx**（非阻塞），事件循环可同时处理 `/v1/models`；中间件对 GET 放行，重入安全。
- `resolve` 仅事件循环调用；`dict` 缓存单线程访问，无锁。
- `shutdown` 无特殊清理（tokenizer 随进程退出）。

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
| 下游二次读 receive | 重放 receive 委托原始 `receive()` 取真实后续消息（`http.disconnect`）；不得合成空 body 的 `http.request`（实测 vLLM 会重复请求） |
| `VLLM_ANOMALY_TOKENIZER_MODEL` 设但 `from_pretrained` 抛错 | 落到 argv `--tokenizer`/`--model` 自动解析；记 INFO |
| argv `--tokenizer` 解析成功但 `from_pretrained` 抛错 | 落到 argv `--model` → model_hint → loopback 兜底；记 INFO |
| argv 无 `serve`（非 vLLM 命令 / 测试环境） | `parse_vllm_argv()` 返回 None，跳过 argv 路径，走 model_hint → HTTP |
| argv / env / HTTP 均无可用路径 | `logger.error` 提示设置 `VLLM_ANOMALY_TOKENIZER_MODEL`，预热放弃；首请求慢路径补尝试 |
| `from_pretrained(model_hint)` 抛错 | 落到 loopback `/v1/models` 兜底；记 INFO |
| `/v1/models` 不可达 / 非预期格式 | 落到 HF 缓存扫描；仍失败 → resolver 不可用终态，记一次 ERROR |
| 缓存扫描命中但 `from_pretrained` 抛错 | 记 WARNING，继续下一候选 / 落到不可用终态 |
| `decode([id])` 对个别 id 抛错 | `resolve` 内 try/except → 该 id 返回 None（置 null），不影响其余 |
| resolver 不可用 | strip 按 §4.7 例外分流：触发例外（客户端请求 topk + 未设 rtati）→ `token_id:NNN`；未触发 → chat 主 token 走 bytes、其余 null、completions null；不影响检测 |
| tk2cat 生成失败 | resolver 仍可用（strip 正常），检测降级为无词表（rare/garbled 走 top1 logp），记 WARNING |
| tokenizer 无 decode 路径 | `generate_tk2cat` raise → tk2cat 不注入 → 无词表检测 |
| 预热线程未完成 / 路径发现失败 | 首请求 `_ensure_resolver` 慢路径补生成 tk2cat（一次性，进程复用） |
| 自定义 `--tokenizer` 路径 | argv `--tokenizer` 优先于 `--model`，自动对齐 vLLM 实际 tokenizer；无需设 env |

## 8. 部署

项目路径：path=xxx/accuracy-monitoring/

安装：
```shell
# 进入项目路径
cd $path
# 安装包
pip install -e .
```


启动：`vllm serve <model> --middleware anomaly_middleware.AnomalyMiddleware`。


