# CATHelper — 慢节点（Straggler）检测

AI 智算集群中识别性能劣化 NPU 卡的两道防线检测体系。第一道**KPI 资源检测**（轻量、常态化）基于 NPU 资源指标时序做时间×空间双维交叉验证；第二道 **Profiling 深查**（按需触发）基于 Ascend PyTorch Profiler 数据从计算/通信/CPU/Bubble 四个维度精查。两道结果合并输出为**一个 JSON 文件**。

---

## 目录

- [一、快速开始](#一快速开始)
- [二、目录结构](#二目录结构)
- [三、输入数据](#三输入数据)
- [四、CLI 参数详解](#四cli-参数详解)
- [五、检测原理](#五检测原理)
- [六、输出与解读](#六输出与解读)
- [七、边界情况](#七边界情况)
- [八、构建与部署](#八构建与部署)
- [九、设计文档](#九设计文档)

---

## 一、快速开始

### 运行前提

- Go ≥ 1.23（构建时需能访问模块代理拉取 `modernc.org/sqlite`）
- Profiler 模式需要目标目录含 `ascend_pytorch_profiler_{N}.db` 文件

### 模式 1：仅 KPI 检测

```bash
cd feature/straggler
# 遗留 CSV 目录模式（每节点 CSV + node_config.json）
go run . --kpi-path=/data/kpi_csv_dir

# 整合模式（CATMonitor straggler_output JSONL，优先）
go run . --kpi-jsonl-dir=/var/lib/catmonitor/straggler \
         --faultsub-url=http://localhost:9101 \
         --baseline-hours=360 --detection-hours=1
```

### 模式 2：仅 Profiler 检测

```bash
go run . path=/data/profiler_output degradation=0.3
```

### 模式 3：KPI + Profiler 联合检测

```bash
go run . path=/data/profiler_output --kpi-path=/data/kpi_csv_dir degradation=0.3
```

**检测顺序**：先跑 KPI（轻量、无侵入）→ 若 KPI 发现确认异常则输出定界结果 → 若 KPI 无确认异常则 fallback 到 Profiler 精查。两道结果都会合并进 `straggler_output.json`。

---

## 二、目录结构

```
straggler/
├── main.go                 # 统一入口：CLI 解析、双模式编排、合并 JSON 输出
├── README.md               # 本文件
├── clustering/             # 共享 kmeans 比例检测算法（空间检测与 Profiler 均质化聚类共用）
│   ├── kmeans.go
│   └── kmeans_test.go
├── resource/               # 第一道防线：资源指标检测（KPI）
│   ├── types.go            #   数据结构 & 指标注册表 & 配置
│   ├── parser.go           #   CSV / KPI 目录解析（node 感知全局卡号）
│   ├── aggregator.go       #   10 秒聚合（裁剪均值 / 计数器增量）
│   ├── baseline.go         #   历史基线构建
│   ├── space_detector.go   #   空间维度检测（peer 对比，最后一点）
│   ├── time_detector.go    #   时间维度检测（自对比）+ 趋势
│   ├── fusion.go           #   二维交叉验证 + 先计算后通信
│   ├── rootcause.go        #   根因定界推理
│   ├── json_reader.go      #   CATMonitor straggler_kpi JSONL 读取
│   └── report.go           #   管线编排 + 文本报告
├── profiling/              # 第二道防线：Profiling 检测
│   ├── dataparse/          #   数据清洗（SQLite → CSV/JSON 中间件）
│   └── detector/           #   检测算法
│       ├── detection.go    #   主流水线（4 类检测编排）
│       ├── data_handler.go #   慢计算/慢通信/慢CPU/Bubble 实现
│       └── clustering.go   #   HomogenizationComparisonFunc 包装（kmeans）
├── config/                 # Profiler 共享配置
│   └── config.go
├── utils/                  # 结果聚合（节点级）+ 工具
│   └── node_result.go
├── report/                 # Profiler 文本报告生成
│   └── report.go
├── DESIGN.md               # Profiling 检测设计
├── DESIGN_NPU_RESOURCE.md  # KPI 资源检测设计
└── SPEC.md                 # 检测技术规范
```

---

## 三、输入数据

### 3.1 KPI 输入（二选一）

#### `--kpi-path`：每节点 CSV 目录（遗留模式）

传一个**目录**，内含多个每节点 CSV 文件 + 一个固定的 `node_config.json`：

```
/data/kpi_csv_dir/
├── node1.csv              # 节点 node-1 的卡数据
├── node2.csv              # 节点 node-2 的卡数据
└── node_config.json
```

**CSV 格式**：每行一个时间戳，指标列值为 JSON dict（`{cardID: value}`）：

```csv
timestamp,NPU_CARD_TEMP,NPU_CARD_POWER,NPU_CARD_AICORE_FREQ,NPU_CARD_AICORE_UTIL,NPU_CARD_HBM_BANDWIDTH_UTIL,NPU_CARD_HBM_UTIL,NPU_TX_BANDWIDTH,NPU_RX_PFC_PKT,NPU_ROCE_TX_ERR_PKT,NPU_ROCE_OUT_OF_ORDER,NPU_ROCE_NEW_PKT_RTY,NPU_NIC_RX_ALL_PKG,CPU_average
1784547926,"{""0"":55,""1"":56}","{""0"":1628}","{""0"":1800}","{""0"":90}","{""0"":80}","{""0"":50}","{""0"":102400}","{""0"":0}","{""0"":0}","{""0"":0}","{""0"":0}","{""0"":100}","{""cpu1"":""4.26""}"
```

> `timestamp` 列必填；其余指标列缺失只告警不阻断（对应 metric dict 为空）。列名不区分顺序。

**`node_config.json` 格式**：把每个 CSV 文件映射到物理节点及其卡号（0 起始）：
```json
{
  "node1.csv": { "node": "node-1", "cards": [0, 1] },
  "node2.csv": { "node": "node-2", "cards": [0, 1] }
}
```
- 校验：每个 CSV 必须在 config 里有条目；config 引用的 CSV 必须存在。
- 配置的卡在 CSV 中无数据 → 告警。

#### `--kpi-jsonl-dir`：CATMonitor straggler_output JSONL（整合模式）

传 CATMonitor 底座写出的 `straggler_kpi_{date}.jsonl` 目录，按 `--baseline-hours` 自动回溯读取窗口内各天文件（某天文件缺失则跳过该天）。**优先于 `--kpi-path`**。

### 3.2 Profiler 输入

`path=` 传含每卡一个 SQLite 文件的目录：

```
/data/profiler_output/
├── ascend_pytorch_profiler_0.db
├── ascend_pytorch_profiler_1.db
└── ...
```

---

## 四、CLI 参数详解

### 顶层入口参数

| 参数 | 类型 | 必需 | 默认 | 说明 |
|------|------|------|------|------|
| `path` | string | 否* | — | Profiler `.db` 目录（*与 KPI 输入至少提供一个） |
| `degradation` | float64 | 否 | 0.3 | 灵敏度。`< 0` 重置为 0.3；`> 1` 允许但告警。联动 KPI 阈值与 Profiler 阈值 |
| `--kpi-path` | string | 否* | — | KPI 模式：每节点 CSV + `node_config.json` 的**目录** |
| `--kpi-jsonl-dir` | string | 否* | — | KPI 模式：CATMonitor `straggler_kpi_{date}.jsonl` 目录（优先于 `--kpi-path`） |
| `--faultsub-url` | string | 否 | — | FaultSub 回调 URL，非空时把 KPI 命中卡回注 faultsub（闭环） |
| `--baseline-hours` | float64 | 否 | 360 | 历史基线窗口（小时，默认 15 天） |
| `--detection-hours` | float64 | 否 | 1 | 检测窗口（小时） |
| `--space-ratio-threshold` | float64 | 否 | 2.0 | 空间 kmeans 簇比例阈值（独立旋钮，不随 degradation 变化） |

\* `path` 与 KPI 输入（`--kpi-path` / `--kpi-jsonl-dir`）至少提供一个；都没有则打印用法并退出。

### 阈值联动（`degradation`）

```
KPI 模式:
  SpaceZThreshold = 1 + degradation            # 空间 Z 阈值（保留，zscore 备用）
  TimeZThreshold  = 1 + degradation × 0.8      # 时间 Z 阈值
  SpaceRatioThreshold = --space-ratio-threshold（默认 2.0，不随 degradation 变化）

Profiler 模式:
  CalThreshold  = 1 + degradation              # 慢计算阈值（默认 1.3）
  CommThreshold = 1 + degradation × 5          # 慢通信阈值（默认 2.5）
```

### KPI 内部配置（代码内默认值，非 CLI）

| 配置 | 默认值 | 说明 |
|------|--------|------|
| `AggregationWindowSec` | 10 | 10 秒聚合窗口 |
| `TrimRatio` | 0.25 | 裁剪比例（每端 25%，中间 50%） |
| `MinSamplesForTrim` | 4 | 桶内原始样本 < 4 时降级为普通均值 |
| `MinBaselineSamples` | 30 | 基线聚合点 < 30（≈5 分钟）时时间维度 Z=0，不判定 |
| `SpaceRatioThreshold` | 2.0 | 空间 kmeans 簇比例阈值（CLI `--space-ratio-threshold` 覆盖） |
| `TimeWeight` / `SpaceWeight` | 0.6 / 0.4 | 融合权重 α / β |
| `EnableTrend` | true | 启用趋势检测 |
| `TrendMinRSquared` | 0.6 | 趋势最小 R² |

---

## 五、检测原理

### 5.1 KPI 检测（resource/）

```
CSV/JSONL 解析 → 10 秒聚合 → 窗口切分(基线/检测) → 建历史基线 →
空间检测(最后一点 peer 对比) → 时间检测(自对比) → 融合(2D 交叉验证) →
根因定界 → 跨卡关联 → 合并 JSON
```

**指标注册表**（方向 = 异常方向；方法 = 空间/时间检测方法）：

| 指标 | 分类 | 方向 | 空间方法 | 时间方法 | 说明 |
|------|------|------|---------|---------|------|
| `temp` | 计算 | ↑ | cluster | MAD | 温度 (°C) |
| `power` | 计算 | ↑ | cluster | MAD | 功耗 (W) |
| `aicore_freq` | 计算 | ↓ | cluster | MAD | AI Core 频率 (MHz)，离散档位 |
| `aicore_util` | 计算 | ↓ | cluster | MAD | AI Core 利用率 (%) |
| `hbm_bandwidth_util` | 计算 | ↓ | cluster | MAD | HBM 带宽使用率 (%) |
| `hbm_util` | 计算 | ↓ | cluster | zscore | HBM 内存使用率 (%) |
| `tx_bandwidth` | 通信 | ↓ | cluster | zscore | TX 带宽 |
| `rx_pfc_pkt` | 通信 | ↑ | absolute | zscore | PFC 暂停帧（计数） |
| `roce_tx_err_pkt` | 通信 | ↑ | absolute | zscore | RoCE 发送错误包（计数） |
| `roce_out_of_order` | 通信 | ↑ | absolute | zscore | RoCE 乱序包（计数） |
| `roce_new_pkt_rty` | 通信 | ↑ | absolute | zscore | RoCE 重传包（计数） |

**空间维度（peer 对比）**：只取检测窗口**最后一个聚合点**；peer 组 = 同一节点内的在场卡（跨节点不互比）。
- **cluster（kmeans 比例）**：共享 `clustering` 包。过滤 ≤0 → z-score 标准化 → 肘部法选 k → kmeans++ + Lloyd 迭代 → 基线簇 = 方向极值簇（↑→最小均值簇，↓→最大均值簇）→ 簇均值比 `> SpaceRatioThreshold` 判定异常 → 对异常簇递归精化。被标记卡 `space_score = 簇比例`。多卡同档异常会一起标记。
- **absolute**：错误计数类指标，值 `> 0` 即异常（sentinel 999）。

**时间维度（自对比）**：基线聚合点 `N < MinBaselineSamples(30)` 时 Z=0 不判定。
- **MAD**：`Z = |current_median − baseline.Median| / (1.4826 × baseline.Mad)`（鲁棒，5 个连续/双峰指标）。
- **zscore**：`Z = |current_mean − baseline.Mean| / baseline.StdDev`。

**融合**：每指标独立判定象限 → 先计算后通信排序 → 复合评分 `α×TimeZ + β×SpaceZ`。

### 5.2 Profiler 检测（profiling/）

```
SQLite .db → 并行域拓扑解析 → 单步快照 → 4 类检测 → 节点聚合 → 合并 JSON
```

| 类别 | 数据 | 阈值/方向 | 说明 |
|------|------|-----------|------|
| 慢计算 `cal` | ZP_Kernel（优先）/ ZP_Duration（降级） | `CalThreshold`(1+deg) | kmeans，方向 max/min |
| 慢通信 `comm` | `{域}_Duration` | `CommThreshold`(1+deg×5) | 每组取通信时长最小的卡为代表，按 PP stage 分桶后 kmeans |
| 慢CPU `cpu` | ZP_Host（hostUid 平滑） | `CalThreshold` | 同主机卡取截尾均值消除节点内差异 |
| Bubble `npu_bubble` | ZP_Bubble | `< 5000 ns` | 固定阈值直接判定 |

> 四类检测统一走共享 `clustering` 包（kmeans 比例检测），与 KPI 空间 cluster 同一算法。

---

## 六、输出与解读

### 6.1 合并 JSON：`straggler_output.json`（运行目录）

两道检测结果合并为**一个文件**，写在**运行目录**（启动命令所在目录）：

```json
{
  "kpi": {
    "summary": { "total_cards": 8, "total_nodes": 2, "confirmed_anomalies": 1, "...": "..." },
    "results": [ { "card_id": 0, "node": "node-1", "quadrant": 2, "anomaly_details": [ "..."] } ],
    "root_causes": [ { "node": "node-1", "card_id": 0, "category": "thermal_throttle", "evidence": [ "..."] } ],
    "correlations": [ "..."]
  },
  "profiler": {
    "node_result": [
      { "hostname": "<hostName>", "npu": [ { "id": 0, "cal": { "score": 1.5 }, "npu_bubble": { "score": 3200.0 } } ], "cpu": { "score": 1.4 } }
    ],
    "comm_domain_result": { "tp": { "0,1,2,3": 3.2 } }
  }
}
```

- **只跑 KPI** → 只有 `"kpi"` 键；**只跑 Profiler** → 只有 `"profiler"` 键；KPI 失败且无 Profiler → 不写文件。
- `kpi` 段 = KPI 检测结果（summary / results / root_causes / correlations）。
- `profiler` 段 = 节点聚合结果：`node_result[]` 按物理节点（hostname）分组，`npu[]` 只含异常 NPU（cal/npu_bubble），`cpu` 节点级；`comm_domain_result` 按通信域分组（组内 rank 逗号连接 → score）。

### 6.2 文本报告

| 报告 | 路径 | 内容 |
|------|------|------|
| KPI 报告 | `path/analysis_result/npu_resource_detection_report.log`（仅 KPI 时 `./analysis_result/`） | 汇总、确认异常详情、早期劣化、跨卡关联 |
| Profiler 报告 | `path/analysis_result/detection_report.log` | 检测摘要表（4 类状态）、ZP_Kernel/ZP_Host 排序柱状图、通信域分组对比 |

### 6.3 stdout

- KPI 文本报告内容
- Profiler 逐类摘要（有异常才列出详情）：
  ```
  慢计算 (cal): 无异常 / 异常 (2) 0: 1.50x; 3: 1.60x
  慢通信 (comm): 无异常
  慢CPU (cpu): 无异常            ← 物理节点数 < 2 时整行不显示
  Bubble (npu_bubble): 无异常
  ```

### 6.4 结果字段解读

| 字段 | 含义 |
|------|------|
| `quadrant=confirmed_anomaly` | 时间+空间双维确认异常（高置信度） |
| `quadrant=early_degradation` | 仅时间维异常，卡偏离自身历史（关注） |
| `quadrant=individual_variance` | 仅空间维异常，卡一贯如此（非故障） |
| `space_score` | 空间簇比例（cluster 方法）；异常条件 `> SpaceRatioThreshold` |
| `time_score` | 时间 Z 值；异常条件 `> TimeZThreshold` 或 sentinel 999 |
| `anomaly_category` | `compute` / `communication` |
| `root_cause.category` | 定界结果（见下表） |
| `secondary_comm_anomalies` | 计算异常导致的继发性通信异常 |

### 6.5 根因定界 → 排查方向

| 定界结果 | 排查方向 |
|---------|---------|
| `thermal_throttle` | 检查风扇转速、风道堵塞、机房温度 |
| `cooling_insufficient` | 检查散热器接触、硅脂老化 |
| `forced_downclock` | 检查驱动/固件频率策略 |
| `straggler` | 触发 Profiling 精查，确认计算慢/通信慢根因 |
| `network_link_issue` | 检查光模块、光纤、交换机端口 CRC |
| `network_congestion` | 检查 PFC 配置、队列 buffer、ECN |
| `network_packet_loss` | 检查 RoCE ECN/DCQCN 参数 |
| `hardware_fault` | 隔离该卡，安排硬件诊断 |

---

## 七、边界情况

| 场景 | 处理 |
|------|------|
| 基线数据不足（N < 30，≈5 分钟） | 时间维度 Z=0，不判定异常 |
| 检测窗口无数据 | KPI 检测返回错误 |
| 空间维度同行点 < 2 卡 | 该节点 Z=0（无法 peer 对比） |
| 某节点在场卡 < 2 | 该节点 Z=0，其他节点不受影响 |
| MAD=0（历史恒定）且当前有偏差 | sentinel 999 → 判定异常 |
| 裁尾后数据不足 | 降级为中位数 |
| 计数器回绕 | 自动加 `MaxUint64` 修正 |
| JSONL 某天文件不存在 | 跳过该天（非错误） |
| CSV 列不完整 | 缺失列告警但不阻断，对应 metric dict 为空 |
| 仅 KPI 无 `path` | 只输出 KPI 结果（`straggler_output.json` 只有 `kpi` 键），不执行 Profiler |
| Profiler 单节点 | 慢CPU 无法检测，stdout 不显示该行 |
| `aicore_freq` 轻度降频（<2×） | 空间不标记，交给时间维度（MAD 自对比） |

---

## 八、构建与部署

```bash
# straggler 是 CATHelper 下的独立 Go module（feature/straggler/）
cd feature/straggler
go mod tidy                      # 首次拉取 modernc.org/sqlite 依赖（需网络）
CGO_ENABLED=0 go build -o slowNodeDetection .
```

全静态二进制，无 CGo（Profiler 用纯 Go SQLite 驱动 `modernc.org/sqlite`）。

**测试**：
```bash
go build ./... && go test ./...
```

---

## 九、设计文档

- [DESIGN_NPU_RESOURCE.md](./DESIGN_NPU_RESOURCE.md) — KPI 资源指标检测设计
- [DESIGN.md](./DESIGN.md) — Profiling 检测设计
- [SPEC.md](./SPEC.md) — 检测技术规范
- [straggler_combination_DESIGN.md](./straggler_combination_DESIGN.md) — 与 CATMonitor 底座的整合设计
