# NPU 资源指标异常卡定位定界 — 设计文档

## 1. 概述

### 1.1 背景与动机

现有慢节点检测基于 Ascend PyTorch Profiler Level0 数据，做**单次快照**检测。Profiler 存在两个限制：

- **性能开销大**：Profiler API 侵入式采集，产生大量 `.db` 文件，不适合常态化

本方案基于 `kpi_collect.sh` 采集的**NPU 资源 KPI CSV**（保留 **15 天**，聚合窗口 10 秒），以**无侵入、常态化**方式实现：

- **纯空间 peer 对比**：异常完全由最后一个聚合点的空间维度判定（时间维度与基线/检测窗口已移除）
- **纯空间 peer 对比**：异常完全由最后一个聚合点的空间维度判定（时间维度、基线/检测窗口、根因定界、四象限均已移除）
- **10秒截尾均值抗噪**：排序→去两端 25%→中间 50% 均值，单采样点噪声不污染检测结果

### 1.2 与 Profiler 检测的定位

```
                    ┌─────────────────────────────────────────┐
                    │         慢节点检测体系                     │
                    │                                         │
                    │  ┌──────────────────────┐               │
                    │  │ KPI 资源指标检测      │  ← 第一道防线  │
                    │  │ (本方案)              │    轻量、常态化 │
                    │  │ 纯空间 peer 对比      │    15天数据    │
                    │  └─────────┬────────────┘               │
                    │            │                            │
                    │            │ 未发现异常                   │
                    │            ▼                            │
                    │  ┌──────────────────────┐               │
                    │  │ Profiler 慢节点检测   │  ← 第二道防线  │
                    │  │ (已有)                │    精查、深度   │
                    │  │ 单次快照、4维检测      │    按需触发    │
                    │  └──────────────────────┘               │
                    └─────────────────────────────────────────┘
```

**检测顺序：先 KPI → KPI 查不到异常 → 再触发 Profiling**

- KPI 检测覆盖面广（热降频、网络错误、功耗异常等硬件级问题），且无额外开销
- Profiling 检测覆盖 KPI 看不到的软件级问题（Kernel 慢、通信慢），按需触发避免常态开销
- KPI 发现异常 → 可以直接定界输出，也可选择再跑 Profiling 做交叉验证
- KPI 未发现异常 → 自动 fallback 到 Profiling 检测

---

## 2. 数据格式

### 2.1 CSV 结构

`--kpi-path` 传一个**目录**，内含多个每节点 CSV + 固定的 `node_config.json`（与 JSONL 多节点布局一致）：

```
timestamp,NPU_CARD_POWER,NPU_CARD_TEMP,NPU_CARD_AICORE_FREQ,NPU_CARD_AICORE_UTIL,NPU_CARD_HBM_BANDWIDTH_UTIL,NPU_CARD_HBM_UTIL,NPU_TX_BANDWIDTH,NPU_RX_PFC_PKT,NPU_ROCE_TX_ERR_PKT,NPU_ROCE_OUT_OF_ORDER,NPU_ROCE_NEW_PKT_RTY,NPU_NIC_RX_ALL_PKG,CPU_average
1784547926,"{""0"":1628,...,""7"":1688}","{""0"":47,...,""7"":50}",...,"{""cpu1"":""4.26"",...}"
```

每个 CSV 的指标单元格为**平铺** `{cardID: value}`，card ID 在**节点内**从 0 编号；`node_config.json` 的 `cards` 之外的卡被过滤。单文件 `ParseCSV`（平铺 → 节点 `"none"`，保留嵌套 JSON 单元格解析）仅内部/测试用，CLI 主路径走目录方式。

内部把 `(node, cardID)` 映射为全局整数卡 ID（`cardIndexer`），并记录 `NodeOf`（全局ID→节点名）与 `LocalID`（全局ID→节点内卡ID）；平铺输入全局 ID = 原始卡 ID。**空间检测的 peer 组是同一节点内的卡**，跨节点不互比。

| 列 | 含义 | 单位 | 类型 |
|---|------|------|------|
| `timestamp` | 采集时间戳 | Unix秒 | int64 |
| `NPU_CARD_POWER` | 每卡功耗 | W | JSON dict[card→float] |
| `NPU_CARD_TEMP` | 每卡温度 | ℃ | JSON dict[card→float] |
| `NPU_CARD_AICORE_FREQ` | 每卡 AI Core 频率 | MHz | JSON dict[card→float] |
| `NPU_CARD_AICORE_UTIL` | 每卡 AI Core 利用率 | % | JSON dict[card→float] |
| `NPU_CARD_HBM_BANDWIDTH_UTIL` | 每卡 HBM 带宽使用率 | % | JSON dict[card→float] |
| `NPU_CARD_HBM_UTIL` | 每卡 HBM 内存使用率（仅采集跟踪，不参与根因规则） | % | JSON dict[card→float] |
| `NPU_TX_BANDWIDTH` | 每卡发送带宽 | ? | JSON dict[card→float] |
| `NPU_RX_PFC_PKT` | 每卡接收 PFC 暂停帧 | 包数 | JSON dict[card→float] |
| `NPU_ROCE_TX_ERR_PKT` | 每卡 RoCE 发送错误包 | 包数 | JSON dict[card→float] |
| `NPU_ROCE_OUT_OF_ORDER` | 每卡 RoCE 乱序包 | 包数 | JSON dict[card→float] |
| `NPU_ROCE_NEW_PKT_RTY` | 每卡 RoCE 重传包 | 包数 | JSON dict[card→float] |
| `NPU_NIC_RX_ALL_PKG` | 每卡 NIC 接收总包 | 包数 | JSON dict[card→float] |
| `CPU_average` | 各 CPU 平均利用率 | % | JSON dict[cpu→string] |

### 2.2 数据分区

时间维度与基线/检测窗口已移除：全部数据聚合后，空间检测只取**最后一个聚合点**（最接近当前时刻的读数）做 peer 对比。历史数据仅用于 10 秒聚合，不参与判定。

---

## 3. 数据预处理：10秒截尾均值聚合

### 3.1 为什么需要预处理

原始 KPI 采集频率可能很高（秒级甚至亚秒级），单个采样点受瞬时波动、采集噪声、短时尖峰影响大。直接用裸数据点做检测会导致：

- **误报**：一个瞬时尖峰被标记为异常
- **漏报**：持续偏高但因单点波动大被统计方法稀释

解决方案：**将每个聚合窗口（`AggregationWindowSec`，默认 10 秒）内的所有原始采样点聚合为一个稳健的统计量**，作为后续检测的"一个数据点"。

### 3.2 截尾均值算法（Midmean）

```
输入：10 秒窗口内某卡某指标的 N 个原始采样值
输出：该 10 秒窗口该卡该指标的聚合值

步骤：
  1. 排序：将 N 个值升序排列 → sorted[0..N-1]
  2. 截尾：去掉前 25% 和后 25%，取中间 50%
     trim = floor(N * 0.25)
     保留区间 = sorted[trim .. N-1-trim]
  3. 平均：对保留区间内的值取算术平均
     midmean = avg(sorted[trim .. N-1-trim])
  4. 返回 midmean 作为该 10 秒窗口该卡该指标的聚合值
```

**示例**：某卡在 10 秒内采集了 20 个温度值（N=20，采集频率 2Hz）

```
原始值: [45, 47, 46, 48, 62, 47, 46, 49, 45, 48, 47, 46, 51, 47, 46, 48, 45, 47, 46, 49]
排序后: [45, 45, 45, 46, 46, 46, 46, 46, 47, 47, 47, 47, 47, 48, 48, 48, 49, 49, 51, 62]
         │←─ 前5个(25%)去掉 ─→│←────── 中间10个(50%)保留 ──────→│←─ 后5个(25%)去掉 ─→│
trim = 5
保留区间 = [46, 46, 46, 46, 47, 47, 47, 47, 47, 48]
midmean = (46+46+46+46+47+47+47+47+47+48) / 10 = 46.7

对比：
  - 全量均值 = 48.1  （被 62 和 51 拉高）
  - 中位数   = 47.0  （只看中间一个点）
  - 截尾均值 = 46.7  ← 最接近真实稳定温度，不被尖峰污染
```

### 3.3 特殊指标的处理

| 指标类型 | 聚合方式 | 理由 |
|---------|---------|------|
| 连续型指标（TEMP, POWER, FREQ, UTIL, BANDWIDTH） | 截尾均值 | 需要消除尖峰，取稳定代表值 |
| 计数型指标（ERR_PKT, RETRY, OUT_OF_ORDER, PFC_PKT） | **累加** | 错误包是累积计数器，应取窗口内（10 秒）的增量总和而非均值 |
| NIC_RX_ALL_PKG | 截尾均值 | 接收包数波动大，截尾后更稳定 |
| CPU_average | 截尾均值 | 同上 |

对于计数型指标，聚合时注意处理计数器回绕（counter wrap）：
```
该窗口增量 = counter[t_end] - counter[t_start]
if 增量 < 0: 增量 += 2^64 （处理回绕）
if 增量 == 0: 正常，无错误
if 增量 > 0: 聚合值 = 增量
```

### 3.4 聚合前后数据量变化

```
聚合前：N 行/10秒（N = 采集频率 × 10）
  e.g., 每秒采集 1 次 → 10 行/10秒 → 15天 = 1,296,000 行

聚合后：1 行/10秒
  e.g., 15天 = 129,600 行

聚合后的每行数据结构保持不变（timestamp 取该窗口起始时间），
仅 value 从"瞬时采样值"变为"截尾均值/累加和"。
```

### 3.5 在管线中的位置

```
原始 CSV（秒级）
  │
  ▼
[CSV 解析]  逐行解析 → rawRows
  │
  ▼
[聚合]  ← 本步骤（AggregationWindowSec，默认 10 秒）
  对每 10 秒窗口：
    按(窗口, 卡号)分组 → 排序 → 截尾25% → 中间50%均值
    （或计数型指标：取增量累加）
  输出：聚合行 (每 10 秒 1 行)
  │
  ▼
[空间检测]  → 只取最后一个聚合点，节点内 peer 对比（kmeans 簇比例 / 绝对阈值）
  │
  ▼
[融合]  → 纯空间判定（compute-first）→ 输出（异常指标 + 空间 score）
```

---

## 4. 空间检测模型

### 4.1 核心思想

时间维度与基线/检测窗口已移除：是否异常**完全由空间维度判定**——在最后一个聚合点与其他卡 peer 对比，问"这张卡比别人差吗？"。peer 组 = 同一节点内的在场卡（跨节点不互比；平铺输入为单节点 "none"，等同全体卡）。

### 4.2 判定

```
空间维度
  某指标某卡正常 → 正常
  某指标某卡异常 → 该卡异常（输出按指标分组：异常指标 → 卡 + score）
```

| 状态 | 空间 | 判定 | 含义 |
|------|------|------|------|
| 正常 | 正常 | ✓ 正常 | 该卡一切正常 |
| 异常 | 异常 | ✗ 告警 | 最后一个聚合点上该卡偏离同伴群体 |

（quadrant / composite_score / severity 已移除；输出只保留异常指标及其空间 score。）

### 4.3 空间评分

```
对指标 m，卡 c：

  空间分 S_space[m][c] = 簇比例（kmeans）：簇均值 / 方向极值簇均值
                         （absolute 方法：值 > 0 → sentinel 999）
  被标记卡 score = 簇比例（> SpaceRatioThreshold）
  基线簇成员 score = 1.0（真实比值）
  其他未标记簇 score = 真实比值（如 1.2）

  复合评分 = 异常指标 score 的均值
```

- 只取全部数据的**最后一个聚合点**判定（无窗口切分）
- 网络错误类指标不适用 kmeans（正常值恒为 0），改用绝对阈值（> 0 即异常）

---

## 5. 检测算法设计

### 5.1 空间维度检测（Peer Comparison）

**只取全部数据的最后一个聚合点**判定（`detectSpaceAnomalies`），peer 组 = 同一节点内的在场卡（跨节点不互比；平铺输入为单节点 "none"，等同全体卡）。**主方法为 kmeans 比例检测（MethodCluster）**，与 Profiler 均质化聚类共享 `clustering` 包：空间维度问"谁偏离同伴"，同伴的标准是**双方向各检一次**——max 方向（基线 = 最小均值簇）与 min 方向（基线 = 最大均值簇），标记数少的方向为异常（相等不上报）。

**方法 A：kmeans 比例检测（MethodCluster，默认）**

```
对最后时间点 t，指标 m，节点 N 内在场卡的读数 V（值 ≤ 0 的读数钳制到极小值 zeroFloor=1e-3——真实 0 是有意义的空闲/关闭读数，参与聚类而非丢弃）：
1. 不足 2 张 → 全 0 退出
2. Z-score 标准化（std≈0 → 强制 1）
3. 肘部法选 k（K=2..min(n,10)，取 inertia 二阶差分最大）
4. kmeans++ 初始化（首个质心 = data[0]，后续 D² 加权采样）
5. Lloyd 迭代（≤300 轮，空簇处理，收敛 1e-9）
6. **双方向各检一次**：max 方向（基线 = 最小均值簇，标记高于它且比例超阈值的簇）→ α1；min 方向（基线 = 最大均值簇，标记低于它且比例超阈值的簇）→ α2
7. 比较 |α1| 与 |α2|：**少数者为异常**；**个数相等 → 不上报**（含 0==0 健康情形与 50/50 歧义情形）
8. 对选中方向的异常簇递归（深度 ≤10）：更深层异常替换父层，更深层无异常保持父层
9. 参与聚类的卡都有 score = 选中方向的簇比例（簇均值/基线均值，或基线/簇均值）：基线簇成员恰为 1.0，其他未标记簇保留真实比值，被标记卡为其比值；缺失/NaN 的卡为 0（无读数，无法计算比值）
聚合：判定用选中方向递归 Detect 的标记（Flagged 数组）；score 为选中方向的真实簇比值
```

适用：POWER, TEMP, AICORE_UTIL, HBM_BANDWIDTH_UTIL, HBM_UTIL, TX_BANDWIDTH

**方法 B：IQR**

```
Q1, Q3 = 25th, 75th percentile
IQR = Q3 - Q1
异常: vi < Q1 - 1.5*IQR 或 vi > Q3 + 1.5*IQR
```

适用：PFC_PKT（可能有少量卡出现尖峰）

**方法 C：均质化聚类**（Profiler 复用共享 `clustering` 包，即上方法 A 的 kmeans 比例检测）

**特殊处理**：
- **AICORE_FREQ**：频率为固定档位值（离散）。并入 kmeans 比例检测（方向自适应，双方向投票决定），只标记 >2× 的严重降频；多卡同档降频一起标记
- **网络错误类**（ERR_PKT, RETRY, OUT_OF_ORDER, PFC_PKT）：正常值恒为 0，> 0 即异常
- **CPU_average**：机器粒度，不与卡级混合，独立检测

### 5.2 时间维度检测（已移除）

```
对卡 c，指标 m：
  历史基线值 B = [v_t1, v_t2, ..., v_tN]  （基线窗口内该卡所有值）
  baseline_mean = avg(B)
  baseline_std  = stdev(B)
  baseline_median = median(B)
  baseline_mad    = MAD(B) = median(|B - baseline_median|)

  检测窗口内的均值 current_mean = avg(检测窗口内的值)
  时间 Z-Score = |current_mean - baseline_mean| / baseline_std

  if baseline_std == 0 → 跳过（历史无波动）
  if 时间 Z-Score > tThreshold (默认 2.0) → 时间维度异常
```

**鲁棒 MAD 变体（temp / power / aicore_freq / aicore_util / hbm_bandwidth_util）**：
这五个指标改用 median + MAD 构造鲁棒 Z-Score，防止基线被历史故障数据污染
（mean 会被异常拖向异常值、std 被平方放大，导致 Z-Score 缩小而漏报）：

```
  检测窗口内的中位数 current_median = median(检测窗口内的值)
  鲁棒时间 Z-Score = |current_median - baseline_median| / (1.4826 * baseline_mad)

  if baseline_mad == 0 → 跳过（历史无波动）
  if 鲁棒时间 Z-Score > tThreshold (默认 2.0) → 时间维度异常
```

- `1.4826 = 1/0.6745`：正态分布下 MAD ≈ 0.6745σ，使鲁棒 Z-Score 与经典
  Z-Score 同尺度，阈值 2.0 的语义不变。
- median 与 MAD 的崩溃点为 50%：只要基线窗口内正常数据占多数，少量故障
  时段不会带偏鲁棒统计量（而 mean/StdDev 会被少数极端值拖走）。
- aicore_util / hbm_bandwidth_util 在工作态占比 ≥50% 时（实际场景几乎 100%），
  median 和 MAD 同样不会被空闲态（双峰分布的低值尾巴）污染——空闲点只占
  ≤20% 且落在绝对偏差的上尾，碰不到偏差的中位数。因此这两个指标也采用 MAD。
- 其余指标（含 4 个网络错误计数器、tx_bandwidth、hbm_util）仍使用经典
  mean/std Z-Score；每指标方法由 `MetricMetaRegistry.TimeMethod` 决定。

**基线防污染的前提假设**：基线窗口中大部分数据（>50%）是正常的，只有小部分
（<50%）是异常/故障数据。MAD 的崩溃点为 50%——只要正常数据占比 >50%，median
和 MAD 就不会被少数异常值污染。如果异常数据过半，则基线本身已失去参考意义，
此时任何统计方法都无法可靠区分"正常"和"异常"。建议缩短基线窗口或手动剔除
已知故障时段。

**趋势增强**：对历史基线窗口 + 检测窗口的整体数据做线性回归：
- `value = slope * timestamp + intercept`
- 若 `slope > 0` 且 R² > 0.6：该指标在持续恶化
- 趋势信息作为定界推理的辅助证据

**基线更新策略**：
- 初始基线：首次运行时用全部历史数据建立
- 定期更新：每 24 小时用最近 15 天数据重建基线
- 异常点的值不纳入基线（避免污染）

### 5.3 指标分类：计算类 vs 通信类

KPI 指标天然分属两个层面，且存在**因果依赖**：计算慢的卡必然表现出通信慢（无法按时参与集合通信），反之不成立。

| 类别 | 指标 | 含义 | 异常方向 |
|------|------|------|---------|
| **计算** | `AICORE_FREQ` | AI Core 频率 | ↓ 降频 |
| | `AICORE_UTIL` | AI Core 利用率 | ↓ 计算没跑满 |
| | `HBM_BANDWIDTH_UTIL` | HBM 带宽使用率 | ↓ 带宽闲置 |
| | `HBM_UTIL` | HBM 内存使用率 | ↓ 内存空闲 |
| | `TEMP` | 温度 | ↑ 过热 |
| | `POWER` | 功耗 | ↓ 空载 / ↑ 过热 |
| **通信** | `TX_BANDWIDTH` | 发送带宽 | ↓ 通信受限 |
| | `RX_PFC_PKT` | PFC 暂停帧 | ↑ 网络拥塞 |
| | `ROCE_TX_ERR_PKT` | RoCE 发送错误 | ↑ 链路故障 |
| | `ROCE_OUT_OF_ORDER` | 乱序包 | ↑ 网络质量问题 |
| | `ROCE_NEW_PKT_RTY` | 重传包 | ↑ 网络丢包 |

### 5.4 检测顺序：先计算后通信

**因果逻辑**：计算异常 → 卡无法按时完成计算 → 在集合通信中迟到 → 通信指标也表现为异常。如果先检测通信，会把计算慢导致的通信异常误判为网络问题。

```
对每张卡：
  ┌─────────────┐
  │ 1. 检测计算  │  ← FREQ, AICORE_UTIL, HBM_BANDWIDTH_UTIL, HBM_UTIL, TEMP, POWER
  └──────┬──────┘
         │
    ┌────┴────┐
    │ 计算异常? │
    └────┬────┘
         │
   ┌─────┴─────┐
   │ 是         │ 否
   ▼            ▼
┌──────────┐  ┌─────────────┐
│ 输出:     │  │ 2. 检测通信  │  ← TX_BANDWIDTH, ERR_PKT, PFC_PKT, RETRY, OUT_OF_ORDER
│ 计算类异常 │  └──────┬──────┘
│ 通信指标   │         │
│ 标记为     │    ┌────┴────┐
│ "可能继发" │    │ 通信异常? │
└──────────┘    └────┬────┘
                     │
               ┌─────┴─────┐
               │ 是         │ 否
               ▼            ▼
          ┌──────────┐  ┌──────┐
          │ 输出:     │  │ 正常  │
          │ 通信类异常 │  └──────┘
          │ (独立)    │
          └──────────┘
```

**关键规则**：
- 计算类任一指标异常 → 该卡归类为"计算异常"，通信指标**不独立检测**（标记为"可能继发于计算异常"）
- 计算类全部正常 → 才检测通信指标 → 通信异常可确认为**独立网络问题**

这个顺序也决定了定界规则的优先级：计算类规则优先匹配，通信类规则仅在计算正常时生效。

### 5.5 指标元信息定义

---

## 6. 根因定界

### 6.1 定界规则表

基于异常指标的组合模式推断根因。规则按**先计算后通信**的顺序排列，且通信类规则仅在计算类指标正常时生效（避免将计算慢导致的继发通信异常误判为网络问题）。

**计算类规则**（优先匹配）：

| # | 模式 | 根因推断 | 置信度 | 建议动作 |
|---|------|---------|--------|---------|
| C1 | TEMP↑ + FREQ↓ | **热降频** | 高 | 检查风扇转速/风道堵塞/机房环境温度 |
| C2 | TEMP↑ + POWER↑ + FREQ— | **散热能力不足** | 高 | 检查散热器接触/硅脂老化/风扇故障 |
| C3 | FREQ↓ + TEMP— | **强制降频（非热）** | 中 | 检查驱动/固件的频率策略配置 |
| C4 | POWER↓ + AICORE_UTIL↓ + HBM_BANDWIDTH_UTIL↓ | **Straggler（卡空闲等待）** | 高 | 该卡可能在等通信/等数据，触发 Profiling 精查 |
| C5 | AICORE_UTIL↓ + HBM_BANDWIDTH_UTIL— | **计算负载不均** | 中 | 检查数据分发策略/模型并行切分是否均衡 |
| C6 | HBM_BANDWIDTH_UTIL↓ + AICORE_UTIL— | **内存带宽瓶颈** | 低 | 检查 HBM 访问模式/是否有大量 cache miss |
| C7 | TEMP↑ + POWER— + FREQ— | **温度传感器漂移** | 中 | 交叉验证功率数据（真发热必伴随功率↑） |
| C8 | 多指标同时异常（≥4个，含计算类） | **板卡综合性硬件故障** | 高 | 建议隔离该卡，安排硬件诊断/更换 |
| C9 | 单项 TEMP↑ 孤立 | **局部热点/传感器个体差异** | 低 | 持续观察，若升级为双维异常则按 C1 处理 |
| C10 | 单项 POWER↑ 孤立 | **功耗计量偏差** | 低 | 交叉验证：功率↑应伴随温度↑，否则可能是计量误差 |

**通信类规则**（仅在计算类指标全部正常时生效）：

| # | 模式 | 根因推断 | 置信度 | 建议动作 |
|---|------|---------|--------|---------|
| N1 | ERR_PKT↑（持续） | **网络物理链路故障** | 高 | 检查光模块/光纤/交换机端口 CRC 错误 |
| N2 | PFC_PKT↑（持续） | **网络拥塞（PFC 风暴）** | 高 | 检查交换机 PFC 配置/队列 buffer/ECN 标记 |
| N3 | OUT_OF_ORDER↑ + RETRY↑ | **RoCE 网络丢包乱序** | 高 | 检查 RoCE 路径 ECN 配置/DCQCN 参数 |
| N4 | TX_BANDWIDTH↓ + AICORE_UTIL— | **通信带宽受限** | 中 | 检查网卡协商速率/PCIe 带宽/光模块型号 |

> **注意**：若某卡同时命中计算类和通信类规则，以计算类为准，通信异常标记为"可能继发于计算异常"，不独立告警。

### 6.2 定界实现

```
func BoundRootCause(cardID, anomalyMatrix, trends) RootCauseResult:
    1. 检查计算类指标异常集
    2. 若计算类有异常 → 仅匹配计算类规则 C1-C10
       → 通信类异常标记为"可能继发于计算异常"
    3. 若计算类全部正常 → 才匹配通信类规则 N1-N4
    4. 按规则表优先级逐条匹配（精确匹配 + 允许额外指标）
    5. 匹配成功 → 返回根因类别 + 置信度 + 证据 + 建议
    6. 无匹配 → "unknown"，输出全量异常指标供人工分析
```

### 6.3 跨卡关联分析

部分根因会同时影响多张卡：

| 异常卡分布 | 推断 | 示例 |
|-----------|------|------|
| 同一物理节点的所有卡 | **服务器级故障**（散热/电源） | 节点 A 的 8 张卡温度同时升高 |
| 同一通信域的所有卡 | **网络级故障**（交换机端口） | tp 组内 4 张卡 ERR_PKT 同时增加 |
| 全部卡 | **任务级故障**（训练 hang） | 所有卡 UTIL↓ + POWER↓ |
| 个别孤立卡 | **板卡级故障** | 单卡 TEMP↑ + FREQ↓ |

根因定界（C1-C10 / N1-N4 规则）与跨卡关联已删除：输出只保留**异常指标及其空间 score（劣化程度）**。faultsub 事件 detail 为 `{指标: score}`。

---

## 7. 检测流程：KPI 优先 + Profiling 降级

### 7.1 整体流程

```
                    ┌─────────────┐
                    │  CLI 入口    │
                    └──────┬──────┘
                           │
                           ▼
                    ┌──────────────┐
                    │ 有 KPI CSV？  │
                    └──────┬───────┘
                           │
               ┌───────────┴───────────┐
               │ 是                    │ 否
               ▼                       ▼
    ┌──────────────────────────┐  ┌──────────────────┐
    │ KPI 资源指标检测           │  │ Profiler 慢节点检测│
    │                          │  │ (已有流程)         │
    │ 1. CSV/JSONL解析 + 10秒聚合 │  └──────────────────┘
    │ 2. 空间检测（最后一点 peer）│
    │                          │
    │ ┌── 先检测计算 ─────────┐ │
    │ │ FREQ/UTIL/HBM/        │ │
    │ │ TEMP/POWER            │ │
    │ │ → 异常? → 计算类定界   │ │
    │ └──────────────────────┘ │
    │           │               │
    │           │ 计算正常       │
    │           ▼               │
    │ ┌── 再检测通信 ─────────┐ │
    │ │ BANDWIDTH/ERR_PKT/    │ │
    │ │ PFC_PKT/RETRY/OOO     │ │
    │ │ → 异常? → 输出         │ │
    │ └──────────────────────┘ │
    │                          │
    │ 4. 输出异常卡详情          │
    └──────────┬───────────────┘
               │
               ▼
    ┌─────────────────────┐
    │ 发现异常?           │
    └──────────┬──────────┘
               │
     ┌─────────┴─────────┐
     │ 是                │ 否
     ▼                   ▼
┌──────────┐   ┌────────────────────┐
│ 输出 KPI  │   │ KPI 未发现明显异常   │
│ 检测结果  │   │ → Fallback 到       │
│ 定界报告  │   │   Profiling 精查    │
└──────────┘   └────────────────────┘
```

### 7.2 集成到 main.go

在现有 8 步管线的**前面**插入 KPI 检测：

```go
// main.go 改造示意
func main() {
    inputPath, kpiPath, degradation := parseCLI()

    config.FilePath = inputPath
    config.CalThreshold = 1 + degradation
    config.CommThreshold = 1 + degradation*5

    // ────── 第一道防线：KPI 资源指标检测 ──────
    var kpiResult *nupresource.DetectionResult
    if kpiPath != "" {
        kpiResult = runKpiDetection(kpiPath, degradation)
        // runKpiDetection 内部按顺序执行：
        //   1. 解析 → 2. 聚合 → 3. 空间检测（最后一点 peer）
        //   4. 先检测计算类指标 (FREQ/UTIL/HBM/TEMP/POWER)
        //   5. 计算正常 → 再检测通信类指标 (BANDWIDTH/ERR/PFC/RETRY/OOO)
        //   6. 融合（compute-first）→ 7. 输出（异常指标 + 空间 score）
    }

    if kpiResult != nil && kpiResult.HasAnomaly() {
        nupresource.WriteResourceReport(kpiResult, inputPath)
        nupresource.ExportResourceJSON(kpiResult, inputPath)
        if inputPath == "" {
            os.Exit(0)
        }
    }

    // ────── 第二道防线：Profiler 慢节点检测 ──────
    profilingdataparse.DataParsing(inputPath)
    parallels, validRanks := nodelevel.GetCurDetectionInfo(inputPath)
    stepData := nodelevel.GetCurJobLastStepData(validRanks)
    result := nodelevel.DelimitDetection(stepData, parallels, validRanks)
    utils.Write_result(result, parallels)
    report.WriteReport(stepData, parallels, validRanks, inputPath, result, inputPath, degradation)

    // KPI + Profiling 联合输出
    if kpiResult != nil {
        nupresource.MergeAndWriteCombinedReport(kpiResult, result, inputPath)
    }
}
```

### 7.3 KPI 与 Profiling 的能力互补

| 故障类型 | KPI 能发现？ | Profiling 能发现？ |
|---------|------------|------------------|
| 热降频（TEMP↑ + FREQ↓） | ✓ 直接 | ✗ |
| 网络链路错误（ERR_PKT↑） | ✓ 直接 | ✗ |
| 网络拥塞（PFC_PKT↑） | ✓ 直接 | ✗ |
| 散热不足（POWER↑ + TEMP↑） | ✓ 直接 | ✗ |
| Straggler（UTIL↓ + POWER↓） | ✓ 间接发现 | ✓ 精确发现 |
| 单卡 Kernel 计算慢 | ✗（UTIL 可能仍高） | ✓ 精确发现 |
| 集体通信延迟 | ✗ 间接 | ✓ 精确发现 |
| CPU Host 处理慢 | ✗ | ✓ |
| Bubble 时间异常 | ✗ | ✓ |

**总结**：KPI 擅长硬件/物理层异常，Profiling 擅长软件/性能层异常。两者互补。

---

## 8. 模块设计

### 8.1 包结构

```
features/straggler/
  ├── nupresource/               # 新增：NPU 资源 KPI 异常检测
  │   ├── types.go               # 数据结构 + 常量定义
  │   ├── parser.go              # CSV 解析 → 原始行
  │   ├── aggregator.go          # 10秒截尾均值聚合（AggregationWindowSec，排序→截尾→均值）
  │   ├── space_detector.go      # 空间维度检测（peer 对比，最后一点；kmeans 比例 / 绝对阈值）
  │   └── report.go              # 结果输出（JSON + stdout 文本报告）
  └── config/
      └── config.go              # 扩展：KPI 检测配置项
```

### 8.2 核心数据结构

```go
// ==================== types.go ====================

// CSVRow 一行原始 CSV 数据。
type CSVRow struct {
    Timestamp      int64
    Power          map[int]float64
    Temp           map[int]float64
    AICoreFreq     map[int]float64
    AICoreUtil     map[int]float64
    HBMBandwidthUtil map[int]float64
    HBMUtil        map[int]float64
    TXBandwidth    map[int]float64
    RXPfcPkt       map[int]float64
    RocETxErrPkt   map[int]float64
    RocEOutOfOrder map[int]float64
    RocENewPktRty  map[int]float64
    NICRxAllPkg    map[int]float64
    CPUAvg         map[string]string
}

// TimeSeriesData 解析后的完整时间序列。
type TimeSeriesData struct {
    Rows     []CSVRow // 聚合后的行（1 行/聚合窗口）
    CardIDs  []int    // 全局卡 ID
    RawRows  []CSVRow // 聚合前的原始行（计数器增量用）
    NodeOf   map[int]string // 全局卡 ID → 节点名
    LocalID  map[int]int    // 全局卡 ID → 节点内卡 ID
}

// MetricName 指标枚举。
type MetricName string
const (
    MetricTemp           MetricName = "temp"
    MetricPower          MetricName = "power"
    MetricAICoreFreq     MetricName = "aicore_freq"
    MetricAICoreUtil     MetricName = "aicore_util"
    MetricHBMBandwidthUtil MetricName = "hbm_bandwidth_util"
    MetricHBMUtil        MetricName = "hbm_util"
    MetricTXBandwidth    MetricName = "tx_bandwidth"
    MetricRXPfcPkt       MetricName = "rx_pfc_pkt"
    MetricRocETxErrPkt   MetricName = "roce_tx_err_pkt"
    MetricRocEOutOfOrder MetricName = "roce_out_of_order"
    MetricRocENewPktRty  MetricName = "roce_new_pkt_rty"
)

// DetectionMethod 检测方法。
type DetectionMethod string
const ( MethodAbsolute DetectionMethod = "absolute"; MethodCluster = "cluster" )

// ==================== 检测结果 ====================

// MetricAnomalyDetail 单个指标的异常详情（仅空间维度）。
type MetricAnomalyDetail struct {
    Metric        MetricName
    Score    float64 // 空间簇比例
    Abnormal bool    // 空间维是否异常
    Method   DetectionMethod
}

// AnomalousCard 某指标下异常的卡。
type AnomalousCard struct {
    Node          string
    CardID        int
    Score    float64
    Abnormal bool
}

// MetricAnomaly 指标优先的异常分组。
type MetricAnomaly struct {
    Metric      MetricName
    Method DetectionMethod
    Cards       []AnomalousCard
}

// AnomalyCategory 异常大类。
type AnomalyCategory string
const (
    CatNone          AnomalyCategory = "none"
    CatCompute       AnomalyCategory = "compute"
    CatCommunication AnomalyCategory = "communication"
)

// ==================== 定界（已移除） ====================

type Severity string // "critical" | "warning" | "info"

// ==================== 配置 ====================

type DetectionConfig struct {
    // 预处理
    AggregationWindowSec int     // 聚合窗口（秒），默认 10（10秒）
    TrimRatio            float64 // 截尾比例，默认 0.25（去前后各25%）
    MinSamplesForTrim    int     // 截尾最少样本数，默认 4

    // 空间维度
    SpaceRatioThreshold float64 // kmeans 簇比例阈值，默认 2.0

    // 调试
    EnableDebug bool // --debug-output：全量输出（含正常卡/正常指标）
}
```

### 8.3 接口设计

```go
// ==================== parser.go ====================
// ParseCSV 解析单文件 KPI CSV（平铺/嵌套单元格，内部/测试用）。
func ParseCSV(filePath string) (*TimeSeriesData, error)

// ParseKPIDir 解析 KPI 目录（每节点 CSV + node_config.json）。
func ParseKPIDir(dir string) (*TimeSeriesData, error)


// ==================== aggregator.go ====================
// AggregateByMinute 对原始行按聚合窗口（AggregationWindowSec，默认 10 秒）做截尾均值聚合。
// 连续型指标（TEMP/POWER/FREQ/UTIL/BANDWIDTH/NIC_RX）→ 排序→截尾25%→中间50%均值
// 计数型指标（ERR_PKT/RETRY/OUT_OF_ORDER/PFC_PKT）→ 取窗口增量（处理counter wrap）
// 输出：每窗口 1 行聚合数据。
func AggregateByMinute(rawRows []CSVRow) ([]CSVRow, error)

// Midmean 计算截尾均值：排序→去前后各25%→中间50%取平均。
func Midmean(values []float64) float64


// ==================== space_detector.go ====================
// detectSpaceAnomalies 对最后一个聚合点执行空间 peer 对比（节点内互比）。
func detectSpaceAnomalies(detectionRows []CSVRow, cardIDs []int, cfg DetectionConfig, nodeOf ...map[int]string) *SpaceDetectionResult


// ==================== report.go ====================
// buildAnomalyMetrics 以纯空间结果按指标分组异常卡（指标优先输出）。
func buildAnomalyMetrics(spaceDetails map[int]map[MetricName]*MetricAnomalyDetail, cardIDs []int, nodeOf map[int]string, localID map[int]int, cfg DetectionConfig) ([]MetricAnomaly, int)

// HasAnomaly 结果中是否有异常卡。
func HasAnomaly(result *DetectionResult) bool

// DetectionResult 是 KPI 检测的完整结果（指标优先）。
type DetectionResult struct {
    Summary DetectionSummary
    Metrics []MetricAnomaly
}

// WriteReport 生成 KPI 文本报告（仅 stdout，不落盘）。
func WriteReport(result *DetectionResult) string

// ExportResourceJSON 导出 JSON。
func ExportResourceJSON(result *DetectionResult, outputPath string) error
```

---

## 9. CLI 设计

```
# 仅 KPI 检测
slowNodeDetection --kpi-path=/dir/of/kpi_csvs [options]

# KPI + Profiling 联合（KPI 优先，无异常则 fallback Profiling）
slowNodeDetection path=/data/dir --kpi-path=/dir/of/kpi_csvs [options]

# 仅 Profiling（已有，不变）
slowNodeDetection path=/data/dir [degradation=0.3]

KPI 检测专用选项:
  --kpi-path=<dir>                KPI 模式：每节点 CSV + node_config.json 的目录
  --kpi-jsonl-dir=<dir>           KPI 模式：CATMonitor straggler_kpi_{date}.jsonl 目录（优先于 --kpi-path）
  --faultsub-url=<url>            FaultSub 回调 URL，非空时把 KPI 命中卡回注 faultsub（闭环）
  --space-ratio-threshold=<float> 空间簇比例阈值，默认 2.0（未传时联动 degradation 取 1+degradation）
  --debug-output                 同时输出所有正常+异常数据（KPI anomaly_metrics 全部指标含正常卡；Profiler 全节点/全通信组）
```

> 注：`--baseline-hours` / `--detection-hours` / `--space-method` / `--space-z-threshold` / `--time-z-threshold` / `--time-weight` / `--no-trend` / `--no-fallback` / `--always-profiling` 等旧 flag 已移除（时间维度与基线/检测窗口已删除，KPI 异常完全由空间维度判定）。

---

## 10. 输出格式

### 10.1 JSON（`straggler_output.json` 的 `kpi` 段）

```json
{
  "summary": { "total_cards": 8, "total_nodes": 1, "anomalies": 1, "normal": 7, "source": "/data/kpi_jsonl_dir" },
  "anomaly_metrics": [
    {
      "metric": "temp",
      "method": "cluster",
      "cards": [
        { "node": "86", "card_id": 3, "score": 3.2, "abnormal": true }
      ]
    },
    {
      "metric": "aicore_freq",
      "method": "cluster",
      "cards": [
        { "node": "86", "card_id": 3, "score": 5.0, "abnormal": true }
      ]
    }
  ]
}
```
（输出为指标优先：`anomaly_metrics[].cards[]` 列出该指标异常的卡及其空间 score（劣化程度）。）

### 10.2 stdout 文本报告

KPI 文本报告**只打印到 stdout，不再写入文件**（原 `npu_resource_detection_report.log` 已移除）。包含：
- 检测摘要（正常 / 异常卡数统计）
- 异常指标详情（指标在前，其后为异常卡及空间 score，如 `aicore_freq  node86:card1(2.25)`）

---

## 11. 关键设计决策

| # | 决策 | 理由 |
|---|------|------|
| 1 | **10秒截尾均值预处理** | 单采样点噪声大。窗口内排序→去前后各25%→中间50%取均值，比全量均值稳健（抗尖峰），比中位数有代表性（保留分布信息） |
| 2 | **指标优先输出** | 输出按指标分组（异常指标 → 异常卡 + score），卡级不再有象限/复合评分/计算通信归类 |
| 3 | **纯空间 peer 对比** | 已移除时间维度与基线/检测窗口。异常完全由最后一个聚合点的空间对比判定（kmeans 簇比例 / 错误计数绝对阈值），简单且无需历史基线 |
| 4 | **KPI 优先 + Profiling 降级** | KPI 无侵入开销、覆盖硬件层异常，适合常态化；Profiling 开销大、覆盖软件层异常，按需触发 |
| 5 | **空间 kmeans 簇比例为主** | 只取最后一个聚合点（每节点少量卡），kmeans O(n·k·iter) 开销可忽略；方向极值簇作基线 + 比例阈值判异常，免调参 |
| 6 | **score 即劣化程度** | 每个异常指标的卡直接带其 score，无需卡级复合评分 |
| 7 | **网络错误用绝对阈值** | ERR_PKT/RETRY 正常值为 0，统计方法失效。>0 即异常 |
| 8 | **计数型指标累加而非截尾** | ERR_PKT/RETRY/PFC_PKT 是累积计数器，应取增量总和。截尾会抹掉真正的错误尖峰 |
| 9 | **10 秒聚合窗口** | 每个窗口内裁剪均值 / 计数器增量，单采样点噪声不污染检测结果 |
| 10 | **正常 / 异常二元判定** | 某指标某卡空间异常 → 该卡异常；不再有四象限概念 |

---

## 12. 边界情况

| 场景 | 处理 |
|------|------|
| CSV 空文件 | 报错退出，不 fallback Profiling |
| 聚合窗口内某卡某指标采样数 < 4 | 截尾25%后不足2个点，降级为全量均值 |
| 窗口边界时间戳不齐 | 按 `timestamp / AggregationWindowSec * AggregationWindowSec` 向下取整分桶 |
| 计数型指标出现 counter wrap | `增量 < 0` 时 += 2^64 修正，若仍 < 0 标记数据异常跳过该窗口 |
| 所有卡某指标完全一致(std=0) | 空间维跳过该指标 |
| 某卡在某些时间点数据缺失 | 该时间点跳过该卡 |
| 网络错误类全为 0 | 跳过该指标检测（无限信息量） |
| 总卡数 < 3 | 空间 peer 不可靠，仅标记极端簇比例（如 >2×） |
| 全部卡同时异常 | 空间维不会标记（同伴一致）。触发任务级关联告警（job_level） |
| CPU 字段引用未知 NPU | CPU 不参与卡级检测，仅用于关联分析的物理节点推断 |
| 瞬态尖峰后又恢复 | 聚合窗口裁剪均值会稀释瞬态尖峰影响 |
| 计算异常 + 通信异常同时出现 | 以计算类定界为准，通信异常标记为"可能继发"，不独立告警 |
| 仅通信异常（计算正常）→ 但 Profiling 发现计算慢 | KPI 指标粒度粗，可能漏检软件层面计算慢。此时 Profiling 作为补充生效 |

---

## 13. 后续扩展方向

1. **在线流式检测**：不等待完整 CSV，逐行消费 + 实时更新
2. **多快照联合**：跨多个时间点的空间检测结果联合（当前只取最后一个聚合点）
3. **与告警系统集成**：Prometheus AlertManager / 企业微信 / 邮件通知
4. **KPI + Profiling 联合报告**：将两次检测结果合并为统一的诊断报告
5. **多 Job 联合分析**：同集群多个训练任务的 KPI 数据联合分析，发现集群级基础设施问题
