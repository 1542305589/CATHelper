# vllm_anomaly_middleware

vLLM 推理精度异常检测 ASGI 中间件。通过 vLLM `--middleware` 插件部署，透明拦截推理请求，
强制采集 logprobs 与 token_id，后台运行异常检测算法，并通过独立 Prometheus 端点暴露检测结果。
全过程对客户端无感知——不影响响应状态、不阻塞响应返回、不泄漏内部参数。

## 功能特性

- **透明拦截**：仅拦截 `POST /v1/chat/completions` 与 `POST /v1/completions`，其余请求原样转发
- **强制采集**：无视客户端参数，强制注入 `logprobs` / `top_logprobs` / `return_tokens_as_token_ids`，
  响应返回前按客户端原始参数恢复（截断 / 置 null / token 文本还原）
- **流式兼容**：SSE 增量转发不缓冲整流，跨块事件重组，每块即时恢复为客户端格式
- **后台检测**：响应全部发送完毕后 fire-and-forget 调度检测，客户端永远不等待
- **四种异常检测**：
  - `rare_character`（生僻字）—— 输出中出现非常规字符类别
  - `garbled`（乱码）—— 符号密集 / 控制字节 / 不可解码序列
  - `repetition`（重复）—— N-gram 轨迹 + ACF 自相关双重检测
  - `nan_value`（NaN）—— 输出含 NaN 值
- **token 文本还原**：自动加载 tokenizer 将强制注入产生的 `token_id:NNN` 还原为真实文本，
  多级兜底（显式 env → 请求体 model → loopback `/v1/models` → HF 缓存扫描）
- **运行时词表生成**：从已加载 tokenizer 直接生成 token→类别映射注入检测器，支持任意模型，无需预生成文件
- **优雅降级**：检测器 / tokenizer 不可用时自动降级为纯透传，指标端点仍可达报零值
- **独立指标**：独立 Prometheus CollectorRegistry，不干扰 vLLM 自带 `/metrics`

## 快速开始

### 安装

```powershell
# 方式 A（推荐）：可编辑安装，自动装依赖
pip install -e vllm_anomaly_middleware

# 方式 B（免安装）：vLLM 环境已有依赖时
$env:PYTHONPATH = "vllm_anomaly_middleware"
```

依赖：`prometheus_client`、`pyyaml`、`numpy`、`httpx`

### 部署

```powershell
$env:VLLM_ANOMALY_TOKENIZER_MODEL = "<model>"   # 必须配置，见下方说明
vllm serve <model> --middleware vllm_anomaly_middleware.AnomalyMiddleware
```

无需 entry-point 注册、无需插件白名单，仅需 vLLM 支持 `--middleware`。


### 验证

```powershell
# 发送推理请求（中间件自动拦截注入检测）
curl http://localhost:8000/v1/chat/completions -d '{"model":"...","messages":[...]}'

# 查看检测指标
curl http://localhost:8000/anomaly/metrics
```

## 配置

全部配置从环境变量读取（构造函数仅接受 `app`，无 kwargs）：

### 必须配置：VLLM_ANOMALY_TOKENIZER_MODEL

```powershell
$env:VLLM_ANOMALY_TOKENIZER_MODEL = "<vllm serve --model 的实际值>"
vllm serve <model> --middleware vllm_anomaly_middleware.AnomalyMiddleware
```

**设为 `vllm serve --model` 传入的实际值**（本地目录路径或 HF repo id）。

**为什么必须配置：** 中间件需要加载与 vLLM 相同的 tokenizer 来完成两项核心功能：
1. **token 文本还原**——中间件强制注入 `return_tokens_as_token_ids=true` 使响应 token 字段
   呈 `"token_id:NNN"` 格式，需用 tokenizer `decode([id])` 还原为真实文本回客户端。未设此参数时
   中间件会尝试自动解析（请求体 model 字段 → loopback `/v1/models` → HF 缓存扫描），但：
   - **本地目录部署**（served 名为裸 basename，不在 HF 缓存）→ 自动解析失败 → token 文本回退 null/bytes
   - **首请求延迟**——未设则首个请求触发慢路径加载 tokenizer（百毫秒级），设定则启动时后台预热零延迟
2. **token 类别映射生成**——生僻字 / 乱码检测依赖 token→类别映射（`tk2cat`），该映射在运行时从
   tokenizer 直接生成。未设参数且自动解析失败 → 无词表降级（rare/garbled 走 top1 logp 路径，
   检测精度降低；repetition/NaN 不受影响）

> 设定后中间件启动后台 daemon 线程预热 tokenizer + 生成 token 类别映射，首请求零延迟。
> 预热失败不影响启动（首请求慢路径补生成）。

### 其他可选配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `VLLM_ANOMALY_ENABLED` | `1` | 总开关，`0`/`false` → 纯透传不检测 |
| `VLLM_ANOMALY_TOP_LOGPROBS` | `20` | 注入的 top-logprobs 数量，范围 1-20 |
| `VLLM_ANOMALY_SAMPLE_RATE` | `1.0` | 检测采样率，范围 0.0-1.0，`0` 不检测 |
| `VLLM_ANOMALY_METRICS_PATH` | `/anomaly/metrics` | 指标端点路径 |
| `VLLM_ANOMALY_DETECTOR_WORKERS` | `1` | 检测线程池大小 |
| `VLLM_ANOMALY_DETECTOR_CONFIG_PATH` | 未设 | 显式指定 `detector.yaml` 路径覆盖 |

## Prometheus 指标

访问 `GET /anomaly/metrics`（默认路径），Content-Type: `text/plain; version=0.0.4; charset=utf-8`。

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `vllm_anomaly_requests_total` | Counter | — | 被检测请求计数 |
| `vllm_anomaly_detected_total` | Counter | `ill_type`, `model` | 检出异常计数 |
| `vllm_anomaly_detection_errors_total` | Counter | — | 检测失败计数 |
| `vllm_anomaly_detection_duration_seconds` | Histogram | — | 检测耗时 |
| `vllm_anomaly_last_rare_character` | Gauge | `model` | 最近生僻字结果（ill_type=1） |
| `vllm_anomaly_last_garbled` | Gauge | `model` | 最近乱码结果（ill_type=2） |
| `vllm_anomaly_last_repetition` | Gauge | `model` | 最近重复结果（ill_type=3） |
| `vllm_anomaly_last_nan_value` | Gauge | `model` | 最近 NaN 结果（ill_type=4） |

`ill_type` 取值：`0`=normal, `1`=rare_character, `2`=garbled, `3`=repetition, `4`=nan_value。
`model` 标签来自请求体 `model` 字段，缺失用 `"unknown"`。

## 检测算法配置

检测器算法默认参数在 `defaults/detector.yaml`，包含窗口大小、各类异常阈值等：

```yaml
window_size: 128    # 检测窗口大小
stride: 64          # 滑窗步长

rare_character:     # 生僻字检测
  explogp_sum_thresh: 0.4
  category_thresh: 2
  top1_logp_thresh: -6

garbled:            # 乱码检测
  top1_logp_thresh: -5
  window_ratio: 0.2
  window_thresh: 0

repetition:         # 重复检测
  trajectory:
    n: 3
    distinct_n_thresh: 0.2
    logp_thresh: -0.2
  acf:
    acf_threshold: 0.65
    logp_thresh: -0.2
  single_window_thresh: 14
  multi_window_thresh: 2
```

可通过 `VLLM_ANOMALY_DETECTOR_CONFIG_PATH` 指定自定义配置文件覆盖。

## 降级行为

| 场景 | 行为 |
|---|---|
| 检测器配置不可用 | 记录日志，永久降级为纯透传，指标报零 |
| 检测器运行抛异常 | 计 `detection_errors_total`，不影响客户端（响应已发完） |
| tokenizer 加载失败 | 软降级：仍注入仍恢复，token 文本回退 null/bytes，检测降级为无词表模式 |
| token 类别映射生成失败 | 检测降级为无词表模式（rare/garbled 走 top1 logp 路径），不影响其余异常检测 |
| 采样率 0.0 | 不注入不检测，请求直接透传 |

## 项目结构

```
vllm_anomaly_middleware/
├── __init__.py            # 重导出 AnomalyMiddleware 等
├── middleware.py          # 统一中间件类 + 预热线程
├── config.py              # PluginConfig（env 读取+校验）+ 路径解析
├── metrics.py             # 独立 Prometheus registry
├── extractor.py           # 请求注入 / 响应抽取恢复 / SSEStreamProcessor
├── token_resolver.py      # tokenizer 获取 + token 文本还原
├── token_categorizer.py   # token 分类纯函数 + 运行时映射生成
├── detector.py            # ILLDetector 检测器本体
├── detector_runner.py     # 检测运行器（线程池+懒构造+词表注入）
├── defaults/
│   └── detector.yaml      # 检测器算法默认参数
├── pyproject.toml
└── tests/                 # 单元测试 + 端到端测试
```

## 测试

```powershell
cd vllm_anomaly_middleware
python -m pytest -q
```

测试覆盖：单元测试（检测器、配置、token 分类、SSE 处理、指标）+ 端到端测试（chat/completions、
流式/非流式、采样、降级、检测调度）。

## 许可证

Mulan PSL v2
