# CATHelper Release Notes

> 本文档按时间倒序记录 CATHelper 每次发布的版本信息。每次发布在顶部追加，不删除历史记录。

---

## v0.2.0

| 项目 | 说明 |
|------|------|
| 版本号 | v0.2.0 |
| 发布时间 | 2026-07-28 |
| 发布人 | sunnytao |
| 平台支持 | Linux (x86_64)；NPU 容错/检测特性需华为昇腾 A3 服务器 |
| 组成 | 底座 CATMonitor v0.3.3 + 上层特性 Elastic EP v0.1.0 + Straggler 慢节点检测 v0.2.0 |
| 许可证 | Apache-2.0 |

### 版本定位

在 v0.1.0（底座 + EEP 弹性容错）基础上，新增上层特性 **Straggler 慢节点（慢卡）检测**，并与 CATMonitor 底座有机整合。至此 CATHelper 形成"采集 → 故障容错（EEP） + 性能劣化检测（straggler）"的双特性上层，二者共享底座的指标采集与故障订阅能力。

### 变更摘要

#### 上层特性 — Straggler 慢节点检测 v0.2.0

- **两道防线检测体系**：第一道（KPI 资源指标检测）基于 15 天历史基线 + 1h 检测窗，时间×空间双维 Z-score + 二维交叉验证 + 根因定界；第二道（Profiler 检测）读 Ascend PyTorch Profiler `.db`，均质化聚类检测慢计算/慢通信/慢CPU/NPU Bubble
- **独立 Go module**：`feature/straggler/`（自带 `go.mod`，依赖纯 Go `modernc.org/sqlite`，无 CGo），import 路径重构为 `.../CATHelper/feature/straggler/*`
- **与 CATMonitor 底座整合**：第一道接入 CATMonitor——新增 opt-in 的 `stragglerout` KPI 文件输出（替代自带 `kpi_collect.sh`）；第二道（Profiler `.db`）保留独立
- **检测命中回注 faultsub**：straggler 把慢卡作为 `straggler_detected` 事件 POST 给 CATMonitor faultsub（经 `POST /faultsub/events` ingest 端点），由 faultsub 推送给订阅者（EEP/运维）触发卡隔离/排查，闭环"采集→检测→响应"
- 详见 [feature/straggler/straggler_combination_DESIGN.md](feature/straggler/straggler_combination_DESIGN.md)

#### 底座 — CATMonitor 增强

- **新增 `features/stragglerout` 模块**（opt-in `collector.Storage` 插件）：抽 11 项 NPU KPI（temp/power/aicore_freq/aicore_util/hbm_util/tx_bandwidth/rx_pfc_pkt/roce_tx_err_pkt/roce_out_of_order/roce_new_pkt_rty + cpu_avg），按时刻×按卡聚合追加写日级 `straggler_kpi_{date}.jsonl`（保留 15 天，60s flush），默认关闭、零回归
- **faultsub 新增事件 ingest**：`POST /faultsub/events` 端点接收外部检测器（straggler）回注的 FaultEvent，经同一 Dispatch 管道推送给订阅者；新增 `straggler_detected` 故障类型
- **指标补充**：`metrics.yaml` 登记 `roce_new_pkt_rty`（RoCE 重传报文数，Medium），补齐 straggler 第 11 项 KPI；hccn_tool 统计解析器本为通用 key:value，无需改代码

#### 根目录文档

- README/SPEC/User_Manual 同步新增 straggler 特性说明、用法与配置；路线图更新（straggler 由"规划中"转为"已交付"）

### 测试

- **CATMonitor（Go）**：`go vet`/`go build`/`go test` 全绿，27 包（+1 stragglerout），含 stragglerout 6 子测试、faultsub ingest 3 子测试；`straggler_output`/`faultsub` 未启用时行为零回归
- **straggler（独立 Go module）**：`go vet`/`go build`/`go test` 全绿，resource 包 9 子测试（json_reader 6 + emit 3）；修复既存编译错误（report.go 字段/位置混用）

### 已知限制

1. **DCMI/真机未验证**：NPU KPI 真实采集、Profiler `.db` 解析、faultsub webhook 端到端推送均需在昇腾 A3 真机复测（单测由 mock 驱动）
2. **roce_new_pkt_rty 字段名待真机确认**：mapper 用别名表（roce_new_pkt_rty / roce_retrans_pkt_num / roce_rx_retrans_pkt_num）兼容；若 hccn_tool 无该字段将缺省为 0
3. **KPI 文件量**：3s 采样 × 8 卡 ≈ 5.7MB/天、15 天≈86MB，定时检测读取可接受
4. **EEP 既有已知问题**：缩容后再次缩容存在偶现问题（详见 [feature/elastic-ep/Release_Notes.md](feature/elastic-ep/Release_Notes.md)）
5. **未推送到远端**：本次发布暂在本地完成
6. **后续**：SGLang 框架支持待后续版本交付

---

## v0.1.0

| 项目 | 说明 |
|------|------|
| 版本号 | v0.1.0 |
| 发布时间 | 2026-07-28 |
| 发布人 | sunnytao |
| 平台支持 | Linux (x86_64)；NPU 容错特性需华为昇腾 A3 服务器 |
| 组成 | 底座 CATMonitor v0.3.3 + 上层特性 Elastic EP v0.1.0 |
| 许可证 | Apache-2.0 |

### 版本定位

CATHelper 的初始版本（v0.1.0）。确立"**底座 + 上层特性**"的分层架构：以 CATMonitor 作为全栈指标采集与健康度评估底座，向上层推理高可用特性提供故障信息采集、判定与推送能力；首个上层特性 Elastic EP（推理卡级弹性容错）完成开发并与底座有机整合。CATHelper 是 CAT 技术架构的主体部分，服务于鲲鹏和昇腾服务器，提供全栈故障指标采集、分析和容错恢复能力，方便被集成，以及使能大型生产环境的高可用特性开发。

### 变更摘要

#### 底座 — CATMonitor v0.3.3

- **全栈指标采集**：7 个部件（CPU / 内存 / 硬盘 / GPU / NPU / 网卡 / 机箱）、204 个指标；14 个来源包抽象数据获取与解析，无硬件时优雅降级；NPU 指标按设备并行采集
- **采集粒度控制**：`collection.min_priority`（low/medium/high）按优先级阈值预过滤采集，采集器经 `AnyWanted` DI 在执行前跳过无需采集的指标组
- **健康度评估**：0-100 健康分，自动检测 GPU/NPU 切换权重方案（Excellent/Good/Warning/Critical）
- **Prometheus 导出**：daemon 内置 `/metrics` 端点（`:9100`），`CachingStorage` 复用采集管道，一次采集同时落盘 JSONL + 缓存导出，零额外进程；含 `/-/healthy`、`/-/ready`
- **Web 仪表盘与能效监控**：独立二进制 `catmonitor-web`（`:9527`），可视化单机健康度与各部件指标；`/dfee/` 能效指标实时图表 SPA
- **数据输出**：JSONL 落盘（按天轮转）+ Prometheus + Web；跨平台（Linux/Windows，NPU/GPU 部分指标 Linux 专有）
- **外部依赖**：仅 `gopkg.in/yaml.v3`，默认构建无 CGo；NPU DCMI 采集在 `-tags dcmi` 后

#### 底座 — 故障订阅推送机制（faultsub，承上启下的新特性）

- **新增 `features/faultsub` 模块**：作为 daemon 的 `collector.Storage` 插件（与 exporter 的 `CachingStorage` 同模式），零侵入 tap 进采集管道，对采集到的 NPU 指标做故障判定并向订阅者推送事件
- **故障判定规则**：7 类——卡掉线（`card_drop`）/ 健康状态（`npu_health`）/ 错误码（`npu_error_code`）/ HBM UCE（`hbm_uce`）/ DDR UCE（`ddr_uce`）/ RoCE 链路异常（`roce_link_down`）/ 驱动异常（`driver_unhealthy`）；规则可配置开关，未配置默认启用（fail-open）
- **变迁驱动事件语义**：仅故障出现/恢复时推送，持续故障不重复推送，事件流安静
- **HTTP Webhook 推送**：经 `net/http` 主动 POST `FaultEvent`（JSON）到订阅者回调 URL，异步不阻塞采集管道，失败重试；**零新依赖**，CATMonitor 保持"仅 yaml.v3"
- **订阅 REST API**（`:9101`）：注册/查询/注销订阅（声明回调 URL / 故障类型 / 关注 NPU / 去抖窗口 / 严重级别）+ `/faultsub/snapshot`（最新故障快照）+ `/faultsub/events`（事件回补）+ `/faultsub/types`（能力发现）
- **订阅级去抖**：`SubscriptionManager` 持有每订阅去抖状态，`Subscription` 保持值类型便于序列化
- **默认关闭**：`faultsub.enabled` 默认 false，不启用时 daemon 行为与底座原版完全一致（零回归）
- **DCMI 故障信息增强**：`ErrorCodeList` 返回完整 hex 错误码列表（原仅返回计数，EEP 靠 `0x40f84e00` 判卡掉线）；`CardDrop` 显式识别 `DeviceNotReady(-8012)`；NPU 采集器新增 `npu/card_drop` 指标，`error_code` 指标升为 High 并输出完整错误码 labels

#### 上层特性 — Elastic EP v0.1.0

- **推理卡级弹性容错**：DP+EP 部署模式下卡故障后推理实例不退出，隔离故障卡所在 DP 域、重排专家（EPLB）后剩余 DP 继续提供推理服务；支持网络闪断故障后请求重推恢复
- **三级哨兵容错框架**：ClientSentinel / EngineCoreSentinel / NPUWorkerSentinel，基于 ZMQ 通信，故障上报 + 自动暂停 + 重试/缩容；vLLM/vLLM-Ascend v0.18.0 补丁形态
- **对外 REST API**：`/fault_tolerance/apply`（pause/retry/scale_down）、`/fault_tolerance/status`
- **外部故障管理中心**：`scale_down_demo.py` + `catmonitor_fault_sub.py`，双路径故障检测（**CATMonitor webhook 订阅 NPU 故障** + ZMQ 引擎健康订阅），映射 NPU→DP rank 后下发容错指令
- **模型支持**：已在 DeepSeek-V3、Qwen3-235B-A22B、GLM5.1（W8A8）完成验证

#### 底座与特性整合

- **EEP 故障信息输入与 CATMonitor 真实衔接**：原 EEP 自带的 DCMI 轮询 Demo 替换为订阅 CATMonitor 的 faultsub 机制；CATMonitor 采集并判定 NPU 故障后经 HTTP Webhook 推送 `FaultEvent` 给 EEP 故障管理中心，由其映射 NPU→DP rank 后下发 pause/scale_down/retry；引擎健康 ZMQ 路径（EEP 内部边界）保留不变
- **整合设计文档**：[feature/elastic-ep/EEP_combination_DESIGN.md](feature/elastic-ep/EEP_combination_DESIGN.md)
- **跨机支持**：EEP 注册时声明可达回调 URL，CATMonitor 反向 POST 推送，支持分机部署

#### 根目录文档体系

- 新增根目录 [README.md](README.md)（项目简介）、[SPEC.md](SPEC.md)（功能规格）、[User_Manual.md](User_Manual.md)（使用手册）、[Release_Notes.md](Release_Notes.md)（本文档），作为 CATHelper 整体入口

### 测试

- **CATMonitor（Go）**：`go vet ./...` 零警告，全量测试通过（含新增 `features/faultsub` 46 个子测试、NPU 采集器 error_code/card_drop 用例、DCMI ErrorCodeList/CardDrop mock 路径）；未启用 faultsub 时行为零回归
- **EEP（Python）**：`test_catmonitor_fault_sub.py` 10 用例全过（含端到端 webhook 往返：mock CATMonitor POST → 订阅器 → mock vLLM 收到 pause+scale_down 且 `exclude_dp_ranks` 正确）
- **既有容错框架**：66 用例（51 单元 + 15 端到端）保持通过

### 已知限制

1. **DCMI CGo 未真机验证**：`dcmi_cgo.go`（含新增 `ErrorCodeList`/`CardDrop` wrapper）在 `dcmi` 构建标签后，本机无 CANN SDK 无法编译，需在真 NPU 服务器 `go build -tags dcmi` 验证
2. **NPU/GPU/Chassis 无真机**：系统测试仅验证优雅降级路径与 mock 驱动路径，真实故障指标采集与端到端容错需在配备昇腾 A3 硬件的机器复测
3. **EEP 容错已知问题**：缩容后再次缩容存在偶现问题会导致缩容不成功（详见 [feature/elastic-ep/Release_Notes.md](feature/elastic-ep/Release_Notes.md)）
4. **FULL Graph 模式未兼容**：EEP 暂不支持大模型整图捕获
5. **未推送到远端**：本次发布暂在本地完成
6. **后续特性待开发**：推理慢节点满卡检测特性、SGLang 支持待后续版本交付

---

*本文档仅追加新版本记录，不删除历史。*
