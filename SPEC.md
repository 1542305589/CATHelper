# CATHelper 功能规格说明书 (SPEC)

> **文档定位**：本文档是 CATHelper 项目的面向使用者的功能规格介绍，作为 [README.md](README.md) 的补充。
> 详细技术设计与架构见各子项目文档：[CATMonitor/DESIGN.md](CATMonitor/DESIGN.md)、[feature/elastic-ep/DESIGN.md](feature/elastic-ep/DESIGN.md)。
> 构建与操作步骤见 [User_Manual.md](User_Manual.md)。

---

## 1. 定位与整体架构

### 1.1 项目定位

CATHelper 是 CAT 技术架构的主体部分，服务于鲲鹏和昇腾服务器，提供全栈故障指标采集、分析和容错恢复能力，方便被集成，以及使能大型生产环境的高可用特性开发。

### 1.2 分层架构

CATHelper 采用"**底座 + 上层特性**"的分层架构：

```
┌─────────────────────────────────────────────────────────────┐
│                    上层特性 (feature/)                        │
│   ┌──────────────────────┐   ┌──────────────────────────┐   │
│   │  Elastic EP (EEP)    │   │  推理慢节点满卡检测 (规划) │   │
│   │  推理卡级弹性容错     │   │                          │   │
│   └──────────┬───────────┘   └──────────────────────────┘   │
│              │ 故障信息订阅 (HTTP Webhook)                     │
├──────────────┼──────────────────────────────────────────────┤
│              ▼                                               │
│   底座 — CATMonitor (CATMonitor/)                            │
│   全栈指标采集 · 健康度评估 · Prometheus 导出 · 故障订阅推送  │
│   ┌────────┬────────┬────────┬────────┬──────────┐           │
│   │ 健康度 │ Web仪表盘│ 能效监控│Prometheus│ faultsub │           │
│   │ 评估   │         │        │  导出   │ 故障订阅 │           │
│   └────────┴────────┴────────┴────────┴──────────┘           │
│   7 部件采集器 · 204 指标 · 14 来源层                         │
└─────────────────────────────────────────────────────────────┘
```

- **底座（CATMonitor）**：成熟的全栈指标采集守护进程，提供故障信息的采集、判定与对外推送能力，供上层特性消费。
- **上层特性**：基于底座的故障信息，面向特定高可用场景实现容错恢复逻辑。当前已交付 **EEP**（推理卡级弹性容错），后续将新增推理慢节点满卡检测特性。

### 1.3 版本

| 项目 | 说明 |
|------|------|
| 当前版本 | v0.1.0（CATHelper 初始版本） |
| 底座版本 | CATMonitor v0.3.3 |
| EEP 版本 | Elastic EP v0.1.0 |
| 平台支持 | Linux (x86_64)，NPU 容错特性需华为昇腾 A3 服务器 |
| 许可证 | Apache-2.0 |

---

## 2. 底座功能 — CATMonitor

CATMonitor 是 CATHelper 的底座，可独立运行。详细功能规格见 [CATMonitor/SPEC.md](CATMonitor/SPEC.md)。

### 2.1 指标采集

| 能力 | 说明 |
|------|------|
| 多部件采集 | CPU / 内存 / 硬盘 / GPU / NPU / 网卡 / 机箱 共 7 个部件 |
| 指标规模 | 204 个指标（High 24 / Medium 121 / Low 59），详见 [指标清单](CATMonitor/docs/CATMonitor_indi_list.md) |
| 来源层架构 | 14 个来源包抽象数据获取与解析，采集器不直接读文件/执行命令，无硬件时优雅降级 |
| 采集粒度控制 | `collection.min_priority`（low/medium/high）按优先级阈值预过滤采集，降低开销 |
| 设备并行采集 | NPU 指标按设备并行采集（单卡失败不影响其他卡） |
| 跨平台 | Linux / Windows 双平台（NPU/GPU 部分指标 Linux 专有） |

### 2.2 健康度评估

- 基于 0-100 健康分评估服务器整体健康度，自动检测 GPU/NPU 切换权重方案
- 等级：Excellent (90-100) / Good (75-89) / Warning (60-74) / Critical (0-59)
- 按 High/Medium 指标阈值扣分，多卡取最差卡

### 2.3 数据输出

| 方式 | 端口 | 说明 |
|------|------|------|
| JSONL 落盘 | — | `{data_dir}/{component}_{date}.jsonl`，按天轮转 |
| Prometheus 导出 | `:9100` | `/metrics` 端点（`catmonitor_{component}_{name}` 前缀），含 `/-/healthy`、`/-/ready` |
| Web 仪表盘 | `:9527` | 独立二进制 `catmonitor-web`，可视化单机健康度与各部件指标 |
| 能效监控 | `:9527/dfee/` | 能效指标实时图表 SPA |

### 2.4 故障订阅推送（faultsub）— 承上启下

> faultsub 是底座与上层特性衔接的关键模块。它作为 daemon 的 Storage 插件，复用采集管道，对采集到的指标做故障判定并推送事件。

| 能力 | 说明 |
|------|------|
| 故障判定 | 对 NPU 指标判定 7 类故障：卡掉线 / 健康状态 / 错误码 / HBM UCE / DDR UCE / RoCE 链路异常 / 驱动异常 |
| 推送方式 | HTTP Webhook（`net/http`，零新依赖）主动 POST `FaultEvent` 到订阅者回调 URL；支持跨机 |
| 订阅配置 | REST API（`:9101`）注册订阅：故障类型 / 关注 NPU / 去抖窗口 / 严重级别 / 回调 URL |
| 事件语义 | 变迁驱动——仅故障出现/恢复时推送，持续故障不重复推送 |
| 拉取兜底 | REST `/faultsub/snapshot`（各 NPU 最新故障快照）、`/faultsub/events`（近期事件回补） |
| 默认关闭 | `faultsub.enabled` 默认 false，不启用时 daemon 行为零回归 |

故障类型与上层 EEP 恢复动作的对应见 [§3.3](#33-故障信息接入)。

详见 [CATMonitor/features/faultsub/faultsub_SPEC.md](CATMonitor/features/faultsub/faultsub_SPEC.md)。

---

## 3. 上层特性 — Elastic EP

EEP（Elastic EP）是 CATHelper 的首个上层特性，实现推理大 EP 卡级弹性容错。详见 [feature/elastic-ep/SPEC.md](feature/elastic-ep/SPEC.md)。

### 3.1 功能能力

| 能力 | 说明 |
|------|------|
| 故障上报 | vLLM 内的容错框架捕获故障后不立即退出，通过 ZMQ 向外报告异常详情与引擎健康状态 |
| 自动暂停 | 故障发生时自动暂停健康 DP rank，防止级联失败（健康→不健康→pause→已暂停） |
| 弹性缩容 | 故障不可恢复时移除故障 DP rank，重新分配专家（EPLB）、重载权重、重建通信组，在剩余健康 NPU 上恢复服务 |
| 重试恢复 | 瞬时性可恢复故障时，清理工作进程状态、重建 Gloo 通信组，恢复推理服务 |
| 网络闪断重推 | 支持 RoCE 链路短暂中断后请求重推恢复 |

### 3.2 适用场景与限制

| 项 | 说明 |
|----|------|
| 部署模式 | DP（数据并行）+ EP（专家并行），仅支持 TP=1，不支持 Pipeline Parallel |
| 硬件 | 当前版本仅支持华为昇腾 A3 服务器 |
| 框架 | 当前支持 vLLM，后续计划支持 SGLang |
| 量化模型 | 仅兼容 W8A8（ModelSlim 格式），W4A8/W4A16 暂不支持 |
| 冗余专家数 | 健康卡上的冗余专家总数必须大于故障卡上的逻辑专家数量 |
| FULL Graph 模式 | 暂未兼容，不支持大模型整图捕获 |

### 3.3 故障信息接入

EEP 的外部故障管理中心通过两条路径获取故障，并据此决策容错动作：

| 路径 | 来源 | 内容 | 处理 |
|------|------|------|------|
| ① 硬件故障 | **CATMonitor 订阅**（HTTP Webhook） | NPU 卡掉线 / HBM UCE / 错误码 / RoCE 链路等 `FaultEvent` | 映射 NPU→DP rank，下发 pause→scale_down（不可恢复）或 retry（恢复） |
| ② 引擎故障 | vLLM ZMQ PUB（`:22867`） | 引擎健康状态、dead 引擎 | 下发 scale_down |

故障类型与恢复动作对应：

| FaultEvent 类型 | EEP 恢复动作 |
|----------------|-------------|
| `card_drop` / `npu_health`(Critical) / `hbm_uce` / `ddr_uce` | pause → scale_down（移除故障 DP rank） |
| `roce_link_down`（recovered=true） | retry（网络闪断重推恢复） |
| `roce_link_down`（持续） | pause → 等待/人工 |
| `npu_error_code`（非卡掉线） | pause → 查状态 → scale_down 或 retry |

整合设计详见 [feature/elastic-ep/EEP_combination_DESING.md](feature/elastic-ep/EEP_combination_DESING.md)。

### 3.4 容错工作流

```
NPU 故障 → CATMonitor 采集判定 → FaultEvent(webhook) → EEP 故障管理中心
                                                          │ NPU→DP 映射
                                                          ▼
                                          pause 暂停健康 DP → scale_down 移除故障 rank
                                                          │
                                                          ▼
                                          重排专家(EPLB) → 重载权重 → 重建通信组 → 恢复推理
```

完整容错工作流图见 [feature/elastic-ep/DESIGN.md §1.3](feature/elastic-ep/DESIGN.md)。

---

## 4. 路线图

| 特性 | 状态 | 说明 |
|------|------|------|
| CATMonitor 底座 | 已交付 (v0.3.3) | 全栈采集 + 健康度 + Prometheus + 故障订阅 |
| Elastic EP | 已交付 (v0.1.0) | 推理卡级弹性容错，已与 CATMonitor 整合 |
| 推理慢节点满卡检测 | 规划中 | 基于底座采集的推理性能与卡级指标检测慢节点并满卡处理 |
| SGLang 支持 | 规划中 | EEP 后续计划支持 SGLang 框架 |

---

## 5. 集成方式

CATHelper 设计为"方便被集成"：

- **作为整体部署**：底座 daemon + EEP 容错框架 + 外部故障管理中心协同运行（见 [User_Manual.md](User_Manual.md)）。
- **底座独立集成**：CATMonitor 可作为独立指标采集组件被任意监控系统通过 Prometheus `/metrics` 或 JSONL 集成。
- **故障信息集成**：第三方故障管理者可按 faultsub 订阅契约（REST + Webhook）接入 CATMonitor 的故障事件流。
- **特性定制**：上层特性可基于底座的故障订阅能力开发专用容错逻辑。

---

*文档版本：v1.0 · 对应 CATHelper v0.1.0*
