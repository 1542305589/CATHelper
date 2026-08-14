# 慢节点检测算法 — 技术规范

基于双入口（KPI 资源指标 / Ascend Profiler Level0），从空间维度（KPI 的 peer 对比）检测 AI 训练集群中性能劣化的 NPU 卡。

---

## 系统概览

```
                    ┌─────────────────────────────┐
                    │       slowNodeDetection      │
                    └─────────────┬───────────────┘
                                  │
              ┌───────────────────┼───────────────────┐
              ▼                                       ▼
   ┌─────────────────────┐               ┌─────────────────────┐
   │  KPI 资源检测（轻量）│               │ Profiler 深查（重量）│
   │  --kpi-path 或        │               │  path=/data/dir      │
   │  --kpi-jsonl-dir     │               │  (每卡一个 .db)       │
   └─────────┬───────────┘               └─────────┬───────────┘
             │                                     │
             ▼                                     ▼
   NPU 资源 KPI 异常报告                慢计算/慢通信/慢CPU/Bubble
        │                                       │
        └──────────────────┬────────────────────┘
                           ▼
            合并输出 straggler_output.json
            {"kpi": ..., "profiler": ...}（运行目录，缺哪个维度就无哪个键）
```

- **KPI 模式**：基于 11 个 NPU 资源指标，空间维度 peer 对比（最后一个聚合点），轻量快速，适合常态化初筛。有异常时可选触发 Profiler 模式做交叉验证。
- **Profiler 模式**：基于 Ascend PyTorch Profiler Level0 SQLite 数据，从计算/通信/CPU/Bubble 四个维度深入分析单步性能。

**运行策略**：KPI 检测始终优先执行。若 KPI 发现异常且有 `path`（Profiler 数据），则继续运行 Profiler 做交叉验证；若 KPI 无异常，降级到 Profiler；若仅 KPI 无 Profiler，KPI 结果即为最终输出。

---

## CLI

```
slowNodeDetection path=/data/dir [degradation=0.3] [--kpi-path=/dir/of/kpi_csvs | --kpi-jsonl-dir=/dir] [--faultsub-url=http://host:9101] [--space-ratio-threshold=2.0]
```

### 参数

| 参数 | 类型 | 必需 | 默认 | 说明 |
|------|------|------|------|------|
| `path` | string | 否* | — | Profiler `.db` 文件目录（*KPI 模式或 Profiler 至少提供一个） |
| `degradation` | float64 | 否 | 0.3 | 灵敏度系数，< 0 重置为 0.3，> 1 允许但警告 |
| `--kpi-path` | string | 否 | — | KPI 模式：包含多个每节点 CSV + `node_config.json` 的目录 |
| `--kpi-jsonl-dir` | string | 否 | — | KPI 模式：CATMonitor `straggler_kpi_{date}.jsonl` 目录（优先于 `--kpi-path`） |
| `--faultsub-url` | string | 否 | — | FaultSub 回调 URL，KPI 发现异常时回传检测结果 |
| `--space-ratio-threshold` | float64 | 否 | 2.0 | 空间 kmeans 簇比例阈值（簇均值/基线均值，独立旋钮，不随 degradation 变化） |

### 阈值计算

```
Profiler 模式:
  CalThreshold  = 1 + degradation
  CommThreshold = 1 + degradation × 5
```

---

## 一、KPI 资源检测模式（`--kpi-path` / `--kpi-jsonl-dir`）

### 1.1 数据流

```
kpi_collect.sh CSV                     CATMonitor JSONL
      │                                      │
      ▼                                      ▼
 ParseCSV()                          ReadKPIFiles()
      │                                      │
      └────────────┬─────────────────────────┘
                   ▼
           TimeSeriesData{Rows, RawRows, CardIDs}
                   │
     ┌─────────────┼─────────────┐
     ▼             ▼             ▼
AggregateByMinute
 (10s trimmed mean / counter delta)
     │
     ▼
detectSpaceAnomalies
 (peer comparison at the last
  aggregated point)
     │
     ▼
buildAnomalyMetrics
        (metric-first grouping:
         metric → anomalous cards + space score)
                   │
                   ▼
    ┌──────────────┼──────────────┐
    ▼              ▼              ▼
合并输出JSON   WriteReport    EmitToFaultSub
 (straggler_    (text report)   (callback)
  output.json)
```

### 1.2 输入格式

card ID 在**每个节点内从 0 开始**编号，身份 = (node, cardID)。节点信息由**目录 + `node_config.json`** 指定。

#### CSV 格式（`--kpi-path` = 目录）

`--kpi-path` 传一个**目录**，内含多个每节点 CSV（平铺 `{cardID: value}`）+ 固定的 `node_config.json`：

```
/dir/
  node_config.json
  node1.csv      # 节点 node-1 的数据（平铺）
  node2.csv      # 节点 node-2 的数据
```

**node_config.json**（按 CSV 文件名 keyed，指定每个文件的节点名和生效的卡）：
```json
{
  "node1.csv": { "node": "node-1", "cards": [0, 1, 2, 3] },
  "node2.csv": { "node": "node-2", "cards": [0, 1, 2, 3] }
}
```

每个 CSV 列以平铺 JSON dict 编码各卡数值：
```
timestamp,NPU_CARD_POWER,NPU_CARD_TEMP,...,CPU_average
1784547926,"{""0"":1628,""1"":1747}","{""0"":47,""1"":51}",...,"{""cpu1"":""4.26""}"
```

`cards` 指定该节点实际使用的卡（CSV 里其他卡被过滤掉）。`ParseKPIDir` 合并所有 CSV 成一个 `TimeSeriesData`，用 `cardIndexer` 分配全局 ID + NodeOf/LocalID。基本校验：每个 CSV 都有配置项、配置引用的 CSV 存在、配置的卡在 CSV 中有数据（缺失 warn）。

> 注：单文件 `ParseCSV`（内部支持嵌套 JSON 单元格）保留为内部/测试用，CLI 主路径走目录方式。

#### JSONL 格式（`--kpi-jsonl-dir`）

由 CATMonitor `stragglerout` 模块写入，按日期分文件 `straggler_kpi_{YYYY-MM-DD}.jsonl`，每行一个采样点：

```json
{"ts":1784547926,"vals":{"0":{"temp":47,"power":1628,"aicore_freq":1800,...},"1":{...}},"cpu_avg":{"cpu1":"4.26"}}
```
- `vals` 始终是**平铺**形态 `{cardID: {field: value}}`，卡号在**节点内**从 0 编号；`cpu_avg` 可选。
- **多节点用二级目录 + `node_config.json`**（key = 子目录名；`node` = 节点名；`cards` = 该节点生效卡号，节点内 0 起始），与 `--kpi-path` 一致：
  ```
  {dir}/
  ├── node-a/straggler_kpi_2026-08-13.jsonl   # 每个文件仍是平铺
  ├── node-b/straggler_kpi_2026-08-13.jsonl
  └── node_config.json
  ```
- 无 `node_config.json` 时按单目录读取（旧版兜底）：`vals` 平铺为单节点 `"none"`，或样本内 `vals` 外层为节点名的**嵌套**形态 `{"node-ip-1": {"0": {...}}, "node-ip-2": {...}}`（`sampleToRow` 嗅探第一个字段值是否为对象来区分）。

`ReadKPIFiles()` 读取目录内全部 `straggler_kpi_{date}.jsonl` 文件并重建 `TimeSeriesData`，与 CSV 路径共享后续全部检测管线。

### 1.3 检测管线（4 步）

#### Step 1: CSV 解析 → `TimeSeriesData`

`ParseCSV()` 按列名映射解析 CSV，每行输出一个 `CSVRow`（各指标以 `map[全局卡ID]float64` 存储）。通过 `cardIndexer` 把 `(node, cardID)` 映射为全局整数卡 ID，并记录 `NodeOf`（全局ID→节点名）和 `LocalID`（全局ID→节点内卡ID）；平铺输入全局 ID 等于原始卡 ID。自动发现所有卡。

#### Step 2: 10 秒聚合

`AggregateByMinute()` 将原始行按 10 秒分桶（`AggregationWindowSec=10`），每桶产出 1 个聚合行：

| 指标类型 | 聚合方式 | 说明 |
|---------|---------|------|
| 连续型（temp/power/freq/util/hbm_bandwidth_util/hbm_util/tx_bw） | **裁剪均值 (midmean)** | 排序 → trim 两端 25% → 中间 50% 求均值。若样本 < `MinSamplesForTrim`(4) 降级为普通均值 |
| 计数器（error counters / PFC / retry） | **增量 (counter delta)** | `last − first`，处理 64-bit 回绕 |

CPU 取桶内最后一个值。

#### Step 3: 空间维度检测（Peer Comparison）

`detectSpaceAnomalies()` **只取全部数据的最后一个聚合点**判定（已无基线/检测窗口切分）。**peer 组 = 同一节点内的在场卡**（跨节点不互比）；平铺输入（单节点 "none"）时与之前一致，peer 组 = 全体在场卡。每卡每指标的 score 数组只含 1 个元素（最后一点）。

**对最后一个点、每个节点**，按 `Method` 判定：

| Method | 适用指标 | 机制 | 异常判定 |
|-------------|---------|------|---------|
| `cluster` | temp/power/freq/util/hbm_bandwidth_util/hbm_util/tx_bw | 递归 kmeans 比例检测（共享 `clustering` 包） | **score（簇比例）> 比例阈值**（`SpaceRatioThreshold`，默认 2.0）|
| `absolute` | 4× error counters | > 0 | sentinel 999 |

**cluster（kmeans 比例）机制**（共享 `feature/straggler/clustering/kmeans.go`，与 Profiler 均质化聚类同一算法）：
1. 收集节点内在场且值 `> 0` 的卡；不足 2 张 → 该节点全 0 退出
2. Z-score 标准化（std≈0 → 强制 1）
3. 肘部法选 k（K=2..min(n,10)，取 inertia 二阶差分最大）
4. kmeans++ 初始化（首个质心 = `data[0]`，后续 D² 加权采样）+ Lloyd 迭代（≤300 轮，空簇处理，收敛 1e-9）
5. **基线簇 = 方向极值簇**（DirHigh→最小均值簇，DirLow→最大均值簇）
6. 簇均值比 `> SpaceRatioThreshold`（默认 2.0）→ 异常簇
7. 对异常簇递归（深度 ≤10）：更深层异常替换父层，更深层无异常保持父层；返回最深异常簇
8. 参与聚类的卡都有 `score = 簇比例`：被标记卡为其比值（> 阈值），未标记卡为中性 1.0；缺失 / 值 ≤ 0 的卡为 0（无法计算比值）

> 方向极值簇作基线：即使异常方是多数（整片偏离），基线仍取正常方向极值簇，不会把"谁都高"误判为正常；比例阈值（2.0）则保证自然散布（如 54..60°C）不会被当作异常。

`aggregateSpaceScores()` 汇总：cluster 方法 `score = 簇比例`，`> SpaceRatioThreshold` 判空间异常；absolute 方法取异常占比。

#### Step 4: 按指标分组输出

`buildAnomalyMetrics()` 以**纯空间结果**判定：某指标某卡 `abnormal` → 该卡异常。输出为**指标优先**——每个异常指标下列出异常的卡及其 `score`（劣化程度）。卡级不再有 quadrant / 复合评分 / category；根因定界与跨卡关联也已移除。

```
某指标异常 → 该指标下所有空间异常的卡及其 score
```

### 1.4 输出

| 文件 | 位置 | 内容 |
|------|------|------|
| `straggler_output.json` | 运行目录（当前目录） | **合并输出**：`{"kpi": <KPI 结果>, "profiler": <Profiler 结果>}`；只跑了哪个维度就只含哪个键 |
| `npu_resource_detection_report.log` | `path/analysis_result/`（仅 KPI 时 `./analysis_result/`） | KPI 文本报告 |
| `detection_report.log` | `path/analysis_result/` | Profiler 文本报告 |
| stdout | — | KPI 文本报告内容 + Profiler 逐类摘要 |
| FaultSub | `--faultsub-url` | 异常卡事件回传 |

**JSON 输出结构**（`straggler_output.json` 的 `kpi` 段，即 `{"kpi": {...}}`）：

```json
{
  "summary": { "total_cards": 16, "total_nodes": 2, "anomalies": 1, "normal": 15 },
  "anomaly_metrics": [
    {
      "metric": "temp",
      "method": "cluster",
      "cards": [
        { "node": "node-ip-1", "card_id": 0, "score": 2.5, "abnormal": true }
      ]
    }
  ]
}
```
（输出为指标优先：`anomaly_metrics[].cards[]` 列出该指标异常的卡及其空间 score；无 quadrant / composite_score / root_causes / correlations。）

### 1.5 NPU 资源指标

| 指标名 | 分类 | 异常方向 | Method | 说明 |
|--------|------|---------|-------------|------|
| `temp` | 计算 | ↑ 偏高 | cluster | NPU 温度 (°C)，对称连续 |
| `power` | 计算 | ↑ 偏高 | cluster | NPU 功耗 (W)，对称连续 |
| `aicore_freq` | 计算 | ↓ 偏低 | cluster | AI Core 频率 (MHz)，离散档位，>2× 降频空间判定 |
| `aicore_util` | 计算 | ↓ 偏低 | cluster | AI Core 利用率 (%)，双峰（80%+ 工作态） |
| `hbm_bandwidth_util` | 计算 | ↓ 偏低 | cluster | HBM 带宽使用率 (%)，双峰 |
| `hbm_util` | 计算 | ↓ 偏低 | cluster | HBM 内存使用率 (%)，仅跟踪不参与规则 |
| `tx_bandwidth` | 通信 | ↓ 偏低 | cluster | TX 带宽，近似连续 |
| `rx_pfc_pkt` | 通信 | ↑ 偏高 | absolute | PFC 暂停帧（累积计数器） |
| `roce_tx_err_pkt` | 通信 | ↑ 偏高 | absolute | RoCE 发送错误包（累积计数器） |
| `roce_out_of_order` | 通信 | ↑ 偏高 | absolute | RoCE 乱序包（累积计数器） |
| `roce_new_pkt_rty` | 通信 | ↑ 偏高 | absolute | RoCE 重传包（累积计数器） |

### 1.6 边界情况

| 场景 | 处理 |
|------|------|
| 空间维度同行点 < 2 卡 | Z=0（无法做 peer comparison） |
| 某节点在场卡 < 2 | 该节点 Z=0（节点内无法做 peer comparison），其他节点不受影响 |
| 裁尾后数据不足 | 降级为普通均值 |
| 计数器回绕 | 自动加 `MaxUint64` 修正 |
| JSONL 某天文件不存在 | 天然跳过（只读存在的文件） |
| CSV 列不完整 | 缺失列 warn 但不阻断，对应 metric dict 为空 |
| 仅 `--kpi-path` 无 `path` | 仅输出 KPI 结果，不执行 Profiler |

### 1.7 配置默认值

```go
AggregationWindowSec: 10      // 10 秒聚合
TrimRatio:            0.25    // 裁剪比例（每端 25%，中间 50%）
MinSamplesForTrim:    4       // 低于此样本数降级为普通均值
SpaceRatioThreshold:  2.0     // 空间 kmeans 簇比例阈值（独立旋钮，--space-ratio-threshold 覆盖，默认 2.0）
```

---

## 二、Profiler 深查模式（`path=/data/dir`）

### 2.1 数据流

```
ascend_pytorch_profiler_{N}.db （每个设备一个）
  │
  ▼
[profiling/dataparse] SQLite 解析
  ├── 读取 META_DATA → parallel_group_info（JSON）→ op_metric/group_info_{N}.json
  ├── 合并所有 step 时间范围为单个聚合 step
  ├── 查询通信算子、Host 时间、Kernel 时间等指标
  └── 输出 op_metric/global_rank_{N}.csv （单行数据）
  │
  ▼
[profiling/detector] 检测引擎
  ├── GetCurDetectionInfo()    → 并行域拓扑 + 有效 rank 列表
  ├── GetCurJobLastStepData()  → 单次快照数据映射
  └── DelimitDetection()       → 执行 4 类检测
  │
  ▼
[utils]  BuildNodeResult()    → stdout 逐类摘要 + 返回节点聚合结果（并入 straggler_output.json 的 "profiler" 段）
[report] WriteReport()        → analysis_result/detection_report.log
```

### 2.2 输入目录结构

```
<path>/
  ├── ascend_pytorch_profiler_0.db
  ├── ascend_pytorch_profiler_1.db
  └── ascend_pytorch_profiler_N.db
```

### 2.3 中间产物（op_metric/）

| 文件 | 格式 | 内容 |
|------|------|------|
| `global_rank_{N}.csv` | CSV，单行 | 设备 N 的性能指标 |
| `group_info_{N}.json` | JSON | 并行域拓扑（sync.Once 去重） |
| `host_info_{N}.json` | JSON | 物理节点 hostUid（sync.Once 去重，同机多卡相同） |

### 2.4 CSV 列说明

| 列 | 含义 |
|------|------|
| `StepIndex` | 合并后 step ID（始终为 0） |
| `StepDuration` | 聚合 step 总时长（ns） |
| `ZP_Device` | step 内非通信时间 = stepDuration − 合并后通信总跨度 |
| `ZP_Duration` | 总通信时间（合并重叠区间） |
| `ZP_Host` | 平均 Host 耗时（通信算子 + KERNEL_AICORE 的 Host 端耗时均值） |
| `ZP_Bubble` | 平均 Bubble 时间（OpStartNs − HostEndNs 的正值均值） |
| `ZP_Kernel` | 平均 KERNEL_AICORE 任务耗时 |
| `DataLoader` | MSTX_EVENTS 中 DataLoader 耗时 |
| `{domain}_Duration` | 该并行域内通信算子平均耗时 |
| `{domain}_Count` | 该并行域内通信算子平均计数 |

### 2.5 检测类型

| 类别 | 标签 | 指标 | 方向 | 阈值 | 结果粒度 |
|------|------|------|------|------|---------|
| 慢计算 | `cal` | ZP_Kernel（优先）/ ZP_Duration（降级） | max / min | CalThreshold | 单卡 |
| 慢通信 | `comm` | `{domain}_Duration`（各域独立） | max | CommThreshold | 卡组 |
| 慢CPU | `cpu` | ZP_Host（按 hostUid 截尾均值预处理） | max | CalThreshold | 单卡 |
| NPU Bubble | `npu_bubble` | ZP_Bubble | < 5000ns | 固定 | 单卡 |

#### 检测方法

**慢计算**：对主检测组内每组卡，优先使用 ZP_Kernel（方向 max，值大 = 计算慢）；若组内有卡缺少 ZP_Kernel 则降级为 ZP_Duration（方向 min，值小 = 计算慢导致通信时间短）。

**慢通信**：对每个非 PP/非 embd 并行域，每组取通信时间最小的卡为代表，按 PP stage 分桶后均质化聚类，异常代表映射回完整组。

**慢CPU**：从每张卡的 `.db` 文件读取 `HOST_INFO.hostUid`，将相同 hostUid 的卡视为同一物理节点。每组节点内计算截尾均值（去 min/max 后平均其余值），覆盖原始值后均质化聚类，消除节点内差异暴露节点间差异。旧版 profiler 缺少 HOST_INFO 表时对应卡跳过预处理，保留原始 ZP_Host 参与聚类。

**NPU Bubble**：固定阈值 `< 5000 ns`（5µs），直接判定。

### 2.6 输出

#### straggler_output.json 的 "profiler" 段

Profiler 结果写入 `straggler_output.json` 的 `profiler` 键（顶层 `{"profiler": {...}}`），结构为节点聚合：结果按**物理节点**（hostname，来自 HOST_INFO.hostName）+ **NPU**（id，来自 NPU_INFO.id）分组；通信结果按**并行域**分组。只含有异常的节点/NPU。

```json
{
  "profiler": {
    "node_result": [
      {
        "hostname": "<hostName>",
        "npu": [
          {
            "id": 0,
            "cal":        { "score": 1.5 },
            "npu_bubble": { "score": 3200.0 }
          }
        ],
        "cpu": { "score": 1.4 }
      }
    ],
    "comm_domain_result": {
      "tp": {
        "0,1,2,3": 3.2
      }
    }
  }
}
```

- `node_result[]`：每个异常节点一条，含 `hostname`（HOST_INFO.hostName，缺失回退 hostUid）、`npu[]`（只含异常的 NPU，`id` 来自 NPU_INFO.id，`cal`/`npu_bubble` score 仅在异常时出现）、`cpu`（节点级，慢节点的共享值）
- `comm_domain_result`：key = 通信域名字（可读域名，如 tp），value = 组内 rank 集（逗号连接）→ score
- 顶层 `straggler_output.json`：KPI 结果在 `kpi` 键，Profiler 结果在 `profiler` 键；只跑 KPI 则只有 `kpi`，只跑 Profiler 则只有 `profiler`

#### detection_report.log

带柱状图（`█`，最大 40 字符宽度）的可读文本报告，包含：
- 数据目录、时间、有效 rank 数
- 并行域拓扑摘要
- 四类检测结果表格
- ZP_Kernel / ZP_Host 排序柱状图（Top 30 + Bottom 5）
- 各通信域分组对比（min/mean/max）
- 时间自动单位转换（s / ms / µs / ns）

### 2.7 均质化聚类算法（kmeans 比例检测）

唯一的异常检测算法，通过方向和阈值参数化适配所有检测场景。**与 KPI 资源检测的空间 cluster 共享同一 `clustering` 包**（`feature/straggler/clustering/kmeans.go`），用簇均值比值作显著性（Profiler 是单快照，无历史噪声可做 z）。

**核心流程**：
1. 过滤值 `≤ 0`；不足 2 个 → 无异常退出
2. Z-score 标准化（std≈0 → 强制 1）
3. 肘部法选 k（K=2..min(n,10)，取 inertia 二阶差分最大）
4. kmeans++ 初始化（首个质心 = `data[0]`，后续 D² 加权采样）+ Lloyd 迭代（≤300 轮，空簇处理，收敛 1e-9）
5. **基线簇 = 方向极值簇**（"max"→最小均值簇，"min"→最大均值簇）
6. 簇均值比 `> threshold` → 异常簇（"max"：`簇均值 / 基线均值`；"min"：`基线均值 / 簇均值`）
7. 对异常簇递归（深度 ≤10）：更深层异常替换父层，更深层无异常保持父层；返回最深异常簇
8. 异常卡的劣化值 = 对应簇比例

**示例**：数据 `[10, 10, 20, 10]`，阈值 1.3，方向 "max"
- kmeans 切出 {10×3}（均值 10）与 {20}（均值 20）
- 基线簇 = {10×3}（方向极值 = 最小均值簇），基线均值 10
- 卡 20：20/10 = 2.0 > 1.3 → 异常，劣化 = 2.0

**与旧版（间隙分裂 + 多数基线）的差别**：旧版按"谁多谁有理"选基线、用间隙切分；新版统一为 kmeans 聚类 + 方向极值基线 + 比例显著性，且对异常簇递归精化（更深层异常替换父层，避免浅层聚类吞掉深层结构）。kmeans 的 D² 采样具有随机性，同一数据多次运行结果可能不同——这是算法固有属性。

### 2.8 SQLite 源表

| 表 | 关键列 | 用途 |
|------|---------|------|
| `META_DATA` | `name, value` | 存储 `parallel_group_info` JSON |
| `STRING_IDS` | `id, value` | 名称 ↔ ID 映射 |
| `STEP_TIME` | `id, startNs, endNs` | Step 时间戳（降级链第一级） |
| `COMMUNICATION_OP` | `opName, startNs, endNs, connectionId, count, groupName` | 设备级通信算子 |
| `CANN_API` | `startNs, endNs, connectionId` | Host API 调用时序 |
| `MSTX_EVENTS` | `startNs, endNs, connectionId, message` | Host 事件（DataLoader、Step 标记） |
| `TASK` | `startNs, endNs, taskType, connectionId` | 任务执行（KERNEL_AICORE） |
| `HOST_INFO` | `hostUid` | 卡所属物理节点标识（慢 CPU 分组依据） |

运行时创建索引：`idx_string_ids_value`, `idx_device_op_time`, `idx_task_time_type`

### 2.9 并行域名称

`tp`, `dp_cp`, `dp`, `cp`, `exp`（Expert Parallel，非 "ep"）, `tp_exp`, `pp`, `cp_ring`, `cp_ulysses`, `default_group`

主检测组优先级：`tp → exp → ep → tp_exp → cp → cp2 → cp_ulysses → cp_ring → dp → dp_cp → dp_modulo_exp_cp`

### 2.10 边界情况（Profiler）

| 场景 | 处理 |
|------|------|
| 无 .db 文件 | `log.Fatalf` 退出 |
| ZP_Kernel 数据不全 | 慢计算降级为 ZP_Duration + 方向 "min" |
| 通信算子缺失 | 除 ZP_Host 外所有指标填充 -99999；ZP_Host 回退用 KERNEL_AICORE Host 耗时 |
| 通信耗时 > step 总耗时 | ZP_Device 钳位到 0 |
| 组内有效卡 < 2 | 跳过该组检测 |
| PP = 1（无流水线并行） | ppStageNum=1，所有代表卡放同一桶聚类 |
| 跨节点拓扑 | getDetectionGroups 通过 nodeGlobalRank 集合过滤 |
| group_info 写入竞态 | sync.Once 保证每个文件名只写一次 |
| HOST_INFO 表缺失 | queryHostUid 返回空串，对应卡跳过 hostUid 预处理 |
| DataLoader 不存在 | DataLoader = 0 |
| Kernel 查询无数据 | ZP_Kernel = 0 |

---

## 包结构

| 包 | 文件数 | 职责 |
|------|--------|------|
| `main` | 1 | CLI 参数解析、双模式编排（KPI → Profiler 降级链） |
| `resource` | 11 | KPI 检测引擎：解析 → 聚合 → 空间检测 → 融合 → 报告 → JSON 导出 → FaultSub 推送 |
| `config` | 1 | Profiler 全局配置（FilePath、阈值）、DegradationData 结果聚合 |
| `profiling/dataparse` | 3 | SQLite `.db` 解析 → CSV + JSON 中间文件（含 host_info） |
| `profiling/detector` | 4 | 并行域拓扑解析、单步快照、四类检测逻辑 |
| `clustering` | 1 | 共享 kmeans 比例检测算法（空间检测与 Profiler 均质化聚类共用） |
| `utils` | 1 | Profiler 结果写入（stdout + JSON 文件） |
| `report` | 1 | Profiler 文本报告生成 |

---

## 关键设计决策

- **双模式分离**：KPI（资源指标时序）和 Profiler（单步快照）是完全不同的检测范式和管线，在 `main.go` 中分支，`resource/` 和 `profiling/` 各自独立。
- **KPI: 纯空间 peer 对比**：已移除时间维度与基线/检测窗口，异常完全由最后一个聚合点的空间 peer 对比判定（kmeans 簇比例 / 错误计数绝对阈值）。
- **KPI: Compute-First 排序**：计算慢必然导致通信慢（卡无法按时参与集合通信），先判定计算再审视通信，避免将计算慢的卡误归因为通信故障。
- **KPI: 裁剪均值聚合**：原始数据 ~2s 采集，每 10 秒聚合窗口（`AggregationWindowSec=10`）内使用 25% 裁剪均值，抵抗采集噪声（温度/功耗传感器的瞬时抖动）。
- **KPI: HBM 双指标并存**：`hbm_bandwidth_util`（带宽）+ `hbm_util`（内存）都做空间检测；语义上带宽更贴合性能瓶颈判断，内存使用率仅跟踪展示。
- **Profiler: 合并 Step**：所有 step 合并为单聚合 step（minStart → maxEnd），CSV 仅一行。Profiler 时间分辨率低，逐 step 不可靠。
- **Profiler: 倒数第二行**：多行数据取 n-2 行，避免末行不完整。
- **-99999 哨兵**（Profiler）：统一无效数据标记，在 GetCurJobLastStepData、detectionZpBubbleData、report.filterValid 中跳过。
- **Profiler: 单一算法**：kmeans 比例检测（`clustering` 包）是唯一的异常检测器，所有场景通用，并与 KPI 空间检测共享同一实现。
- **Profiler: 不做时序分析**：仅处理单次快照，不进行趋势/移动平均/变点检测。

---

## 构建

```bash
# Linux ARM64（目标平台）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o slowNodeDetection .

# Linux AMD64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o slownode_linux_amd64 .

# Windows AMD64
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o slownode_win_amd64.exe .
```

全静态二进制，SQLite 驱动使用 `modernc.org/sqlite`（纯 Go 实现，无需 CGO）。
