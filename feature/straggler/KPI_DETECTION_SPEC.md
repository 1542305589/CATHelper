# Center KPI Detection Specification

本文件只描述中心节点 KPI 检测能力，不描述整个 straggler 系统的其它功能。

## 1. 目标与范围

中心节点周期性消费已入库的 NPU KPI 数据，对每个业务、每个节点、每张 device 做时间和空间两个维度的异常检测。

- 检测周期：每个业务单独配置，默认 10s，最小 5s，与该业务的 profiler 检测周期完全独立。
- 检测指标：只检测 `power`、`aicore_util`、`aicore_freq`。
- 其它 KPI 指标不检测、不入参考窗口、不参与统计。
- 检测只使用通过中心节点 device 过滤规则的 device。
- KPI 数据查询只保留内部 SQL，不对外提供 latest/history HTTP API。

## 2. 数据来源

数据来自中心节点 SQLite 的 `kpi_samples` 表，每条记录形如：

```text
business, daemon, node, ts, device, metrics(JSON)
```

检测只读取 `metrics` 中的：

```text
power
aicore_util
aicore_freq
```

缺失值不参与统计，也不进入参考窗口。

## 3. 检测周期与聚合

- 每个业务按自己的 `kpi_interval_sec` 执行检测，默认 10s；检测使用该节点最新已入库数据的时间戳作为窗口结束，若没有比上次检测更新的数据则跳过本轮。
- 每个 `(business, node, device)` 在一个检测周期内，把最近一个检测周期长度的样本聚合为一个当前点；窗口长度与该业务的 KPI 检测周期一致（默认 30s）。
- 聚合方式：30s midmean（trimmed mean，不是中位数）。
- 若最近 10s 没有新数据，跳过该 device。
- 一个检测周期只产生一个当前点。

## 4. 参考窗口

- 参考窗口不单独维护基线数据库。
- 参考窗口 = 该 `(business, node, device)` 最近 10000 个历史 ts 点。
- 参考窗口从 `kpi_samples` 查询，并排除已被标记为异常的 `(business, node, device, ts)`。
- 参考窗口先按与当前点相同的检测周期长度做聚合（midmean），再计算 median/MAD。
- 少于 100 个参考点：跳过时间维度异常判定；空间维度仍按同节点至少 3 张 device 执行。
- 参考窗口不做主动异常剔除。median/MAD 自身容忍少于 50% 的异常点。
- 参考窗口只做工作状态过滤，见下一节。

## 5. 工作状态过滤

工作状态过滤只用于参考窗口统计，不改变入库数据。

- 计算参考窗口的 `median_freq = median(aicore_freq)`。
- 计算 `freq_work = max(median_freq, current_freq)`。
- 保留 `aicore_freq >= freq_work * (1 - 10%)` 的参考点（10% 用于覆盖 1800→1900 的超频场景）。
- 如果参考窗口所有 `aicore_freq` 相同，则不过滤。
- 过滤后参考点少于 100：标记 `reference_invalid`，本轮跳过该 device 所有指标的时间维度检测；空间维度仍可用。
- 不重置参考窗口；继续使用最近 10000 个点，让旧点自然滚出。

已知工作频率范围：

- 最低工作频率约 800 MHz。
- 最高工作频率约 1800 MHz，人为超频可能到 1900 MHz。
- 该范围只用于理解工作状态，不硬编码到算法。

## 6. 统计方法

- 使用 median/MAD。
- 对每个 `(device, metric)` 独立计算：

```text
median = median(values)
mad = median(abs(value - median))
z = 0.6745 * (value - median) / mad
```

- `mad == 0` 或过小时使用每个指标的最小 MAD 下限；异常记录中的 `mad` 写有效下限值，避免除零和过度敏感：
  - `power`：5
  - `aicore_util`：5
  - `aicore_freq`：5
- 时间维度和空间维度使用相同的 median/MAD 方法。
- 不做参考窗口的两遍异常剔除。

## 7. n-sigma 参数

| 指标 | 时间维度 n | 空间维度 n | 方向 |
|---|---:|---:|---|
| `power` | 4 | 4 | 绝对值 |
| `aicore_util` | 4 | 4 | 绝对值 |
| `aicore_freq` | 3 | 3 | 只查低值 |

- `power` / `aicore_util`：`abs(z) > n` 为异常。
- `aicore_freq`：`z < -n` 为异常；高值不报异常。
- 每个业务的 n 可分别配置：
  - `power_time` / `power_space`
  - `util_time` / `util_space`
  - `freq_time` / `freq_space`
- 默认值：power/util 时间/空间均为 4，freq 时间/空间均为 3。
- 暂不做 persistence；如果误报多，再增加“连续 2 个周期异常才报”。

## 8. 时间维度检测

对每个 `(device, metric)`：

1. 取最近 10000 个参考点。
2. 应用工作状态过滤。
3. 计算 median/MAD。
4. 计算当前点 z。
5. 按第 7 节的方向和 n 判断异常。

特殊情况：

- 参考点少于 100：跳过时间维度异常判定。
- 参考窗口为空：第一个点无条件进入参考窗口。
- 当前 `aicore_freq > median + 3*MAD`：
  - 不报 `aicore_freq` 异常；
  - 说明参考窗口不是完全工作状态；
  - 用当前频率更新 `freq_work`，重新过滤参考窗口；
  - `power` / `aicore_util` 也用过滤后的参考窗口检测；
  - 如果过滤后参考点少于 100，标记 `reference_invalid`，本轮跳过该 device 所有指标的时间维度检测。

## 9. 空间维度检测

- 对同一 `(business, node, ts)` 的多个 device 比较。
- 同一节点至少 3 张 device 才执行空间维度检测；少于 3 张跳过。
- 每个指标独立计算 median/MAD。
- 方向：
  - `power` / `aicore_util`：`abs(z) > 4`。
  - `aicore_freq`：`z < -3`。
- 空间维度只使用通过 device 过滤的 device。

## 10. 冷启动与入库规则

- 参考窗口为空：第一个点无条件进入参考窗口。
- 参考点少于 100：
  - 时间维度异常判定跳过；
  - 当前点入库前仍做空间维度检查；
  - 空间维度异常的点不进入参考窗口。
- 参考点达到 100 后：
  - 当前点必须时间维度和空间维度都无异常才进入参考窗口；
  - 当前 device 异常：只拒绝当前 device 的当前点；
  - 其他 device 同 ts 的点只要自身无异常，仍然进入参考窗口；
  - 不做“一张卡异常导致同 ts 所有卡都不入库”的跨 device 排除。

## 11. 异常存储与参考窗口排除

异常结果存入内部 SQLite 表 `kpi_anomaly`：

```text
business, node, device, ts, metric, kind(time/space),
value, median, mad, z, created_at
```

异常记录的 `ts` 是检测窗口结束时间；参考窗口查询时排除 `s.ts > a.ts - 30 AND s.ts <= a.ts` 的任意异常记录，即：

- 当前 device 的当前 ts 只要任一指标时间/空间异常，该 device 该 ts 不进入后续参考窗口；
- 其他 device 不受影响。

## 11.1 全局漂移

- 每个 `(business, node, metric)` 维护连续全局漂移计数。
- 某一轮该 metric 满足 `flagged >= 3` 且 `flagged / devices >= 50%` 时计数 +1，否则清零。
- 连续 3 轮满足后确认全局漂移：
  - 回溯删除这 3 个 ts 下该 metric 的异常明细；
  - 重新统计对应 run 的 anomalies/flagged；
  - run 标记 `global_shift=true`；
  - 参考窗口起点 `reference_since[node]` 设为 `now - 100 * window`；
  - 最近 100 个窗口的异常点允许进入参考窗口；
  - 之后的新点恢复正常规则。
- 全局漂移确认不触发 profiler；只有非 global_shift 的真实异常才驱动 profiler 自动触发。

## 12. Profiler 自动触发

- 不再因为 `flagged > 50%` 设置 `invalid`，也不再跳过下一轮检测。
- 所有时间/空间异常都正常写入 `kpi_anomaly`。
- 如果某个业务连续 3 次 KPI 检测都至少有一条异常，自动触发一次该业务的 profiler 检测。
- profiler 触发后设置 10 分钟冷却；冷却期内即使再次连续 3 次异常，也不触发。
- profiler 正在运行、业务暂停、或冷却未结束时，不触发。
- 连续异常计数在无异常时清零；触发 profiler 后清零。

## 12.1 参考窗口接纳率

- 异常点默认不进入参考窗口。
- 每个业务可配置 `kpi_reference_admit_ratio`（默认 0.1）。
- 检测到异常时，按该概率决定该 `(node, device, ts)` 的异常点是否仍进入参考窗口。
- 参考窗口查询只排除未接纳的异常点；接纳的异常点参与 median/MAD。
- median/MAD 对 <50% 的异常点仍稳健。
- 该触发不影响 KPI 检测周期本身。

## 13. 调度与结果

- KPI 检测周期是业务字段 `kpi_interval_sec`，添加业务时设置，也可通过 `POST /center/business/{name}/kpi_interval` 修改。
- 业务添加/控制台里的 `interval_sec` 是 profiler 周期，不是 KPI 检测周期。

- 中心节点启动一个独立的 KPI 检测循环，周期 10s。
- 每个 business 独立检测。
- 检测结果只写入内部 SQLite，不对外提供通用 KPI 数据查询 API；业务控制台的 KPI 检测 tab 通过 console-only 接口展示最近的检测运行和异常。
- 控制台只保留 KPI device 过滤配置接口。

## 14. 非目标

- 不维护独立基线数据库。
- 不做参考窗口异常剔除。
- 不做 10000 点淘汰。
- 不做 frequency 最大频率优先淘汰。
- 不做跨 device 的 ts 级联排除。
- 不做 persistence。
- 不检测其它 8 个 KPI 指标。
- 不对外提供 KPI 数据查询 API。
