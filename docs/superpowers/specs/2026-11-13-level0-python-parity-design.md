# Level0 慢节点检测：Python skill → Go 移植设计

日期：2026-11-17（v2）
状态：草稿（待批准）
分支：`level0-parity`

## 1. 背景与目标

上游 Python skill（`C:\Users\admin\Desktop\Projects\straggler-detector`，只读）是慢节点检测的算法参考。本项目
`feature/straggler/` 的 Go 实现已独立演进，与 Python 在解析口径、检测类别、聚类算法、输出格式上都已分叉。

**目标**

1. `level0` 数据下，一次性模式的检测结论与项目内 skill（`5d1fff4`，扁平 schema）**严格一致**。
2. 额外落盘 Python schema 的 `straggler_detection_result.json`（双写）。
3. 分布式（daemon→center）具备**正确的跨节点通信检测**：daemon 额外上报算子时序，中心合并后回填带宽/PP 再检测。

**非目标**

- 不处理 `levelnone` / `level1` / `cluster` 单文件；Go 不做数据级别判定，**默认按 level0 处理**。
- 不移植 verl colocate（训练/rollout 混采）角色分类；仅面向推理与预训练场景。
- 不迁移到上游最新的 2.0 `faults` schema（后续再评估）。
- 不改 daemon/center 的 HTTP 路由与现有文件名契约。

**反目标**

- 控制台/落盘的「算法输入数据」（合成 op_metric）里混入原始算子时序。
- daemon/center 无法编译、启动或接口报错。
- 覆盖 `straggler_output.json` / `op_metric/` / `analysis_result/detection_report.log` 导致下游读到错数据。

## 2. 关键决定（已确认）

| 项 | 决定 |
|---|---|
| 输出 schema | 项目内 skill 的扁平 schema（6 类：`KERNEL_AICORE`/`kernel_aivec`/`memcpy_async`/`cpu`/`comm`/`pp_comm`） |
| 数据级别 | 只处理 level0，不做判定，仍按现有方式发现 `*.db` |
| `npu_bubble` | **彻底删除**（检测、字段、`bubble_ns` 阈值、报告、前端） |
| colocate | 本轮不做（仅推理/预训练） |
| 分布式 comm | 纳入本次：daemon 上报算子时序，中心回填 |
| 算子时序数据 | 仅传输用途；**不进** op_metric、不暴露控制台 |
| 双写 | 保留 `straggler_output.json`，新增 `straggler_detection_result.json` |
| 测试数据 | 不入库；端到端验证由内部机器完成 |

## 3. 现状差距（勘察结论）

### 3.1 解析层（`profiling/dataparse`）

- CSV 列不同：Go 用 `ZP_Kernel`（=KERNEL_AICORE），多 `ZP_Bubble`，缺 `MEMCPY_ASYNC`、`KERNEL_AIVEC`、
  `HostDuration`、memcpy 计数器。
- `ZP_Host` 公式不同（Go 会重复累加两张表的 kernel host）。
- `{domain}_Duration/_Count`：Go 去极值均值；Python 白名单域 + 普通均值 + 预置 0。
- 带宽回填缺「组内 rank 必须齐全」规则；对齐失败缺 5ms 近邻回退。
- `PP_WaitGap`（pp_comm）Go 完全没有。
- Go 无级别判定（符合本设计）。

### 3.2 检测层（`profiling/detector`）

- 只检测 `ZP_Kernel`；缺 `kernel_aivec`（1.3）、`memcpy_async`（2.5）。
- 无效值处理：Python 逐 rank 过滤；Go 整组跳过。
- CPU：Go per-rank；Python 节点级 + 4 卡回退。
- **Go 仍有 `npu_bubble`；无 `pp_comm`**。
- comm 旧口径基本等价；degradation 表示不同。

### 3.3 算法层

- 肘部法打分方向相反（选出的 K 不同）、随机实例、空簇、baseline、递归、min 方向 degradation 均不同。

### 3.4 输出层

- Go 无自然语言结论模块；结果 schema 与 Python 不同（见 §5.4）。

## 4. 数据流设计

### 4.1 一次性模式

```
.db 目录 → dataparse（解析 + 带宽/PP 回填）→ op_metric → detector → 结果
                                              └→ comm_ops（内存）
```

- 所有 rank 都在同一批 `.db` 中，带宽/PP 用完整 rank 计算，与 Python 一致。
- 产出：`straggler_output.json`（Go 契约） + `straggler_detection_result.json`（Python schema） +
  `analysis_result/detection_report.log` + stdout 汇总表/结论。

### 4.2 分布式模式

```
daemon: .db → 解析 → op_metric（干净） + comm_ops（算子时序）
        → POST /center/op_metric/{business}/{daemon}
          { "ranks": op_metric, "comm_ops": comm_ops }        # 并列，不混

center: 分别保存 lastOpMetric / lastCommOps（内存）
        → 合并 ranks + comm_ops
        → 用完整 rank 的 comm_ops 重算带宽/PP_WaitGap
        → 写回 op_metric 的派生列（仅 CSV 列，不含原始算子）
        → 检测
        → 落盘「干净 op_metric」+ 结果；comm_ops 用完即弃
```

- **兼容性**：载荷为 v2 信封。旧中心走 `ranks` 回退、忽略 `comm_ops`；旧 daemon 无 `comm_ops` 时中心回退到
  daemon 已算好的带宽。两种情况都不报错。
- **comm_ops 形状**：

```json
{
  "ranks":    { "0": { "group_info": {...}, "host_info": {...}, "npu_info": {...}, "global_rank": {...} } },
  "comm_ops": { "0": { "tp": [ {"op_type":"allReduce","count":4096,"start_ns":1000,"end_ns":1200} ],
                        "pp": [ {"op_type":"send","count":1,"start_ns":1300,"end_ns":1350} ] } }
}
```

- 只含集合通信与点对点算子，且限检测窗口；不含计算 kernel。
- **控制台不可见**：中心落盘的 op_metric、`/center/business/{name}/op_metric`、`/straggler/op_metric/*`
  均不包含 `comm_ops`。
- 体积控制：每 rank 上限 + 可选 gzip；超限时该 rank 回退到 daemon 本地带宽并告警。

### 4.3 时钟与口径

- 默认按 Python 的 wall-clock 对齐（跨节点依赖 NTP）。
- 若后续发现跨机时钟不可靠，可对「跨节点组」切换为扁平池口径（本次先不做，记录为后续工作）。

## 5. 实现设计

### 5.1 解析层

1. 追加/对齐 level0 指标：`KERNEL_AIVEC`、`MEMCPY_ASYNC`、`HostDuration`、memcpy 计数；修正 `ZP_Host` 与
   `{domain}_Duration/_Count`。
2. 带宽回填：增加「组内 rank 齐全」；5ms 近邻回退；抽出「纯算子数据」入口，供一次性与中心复用。
3. 新增 `PP_WaitGap` 回填。
4. 解析阶段额外产出 `comm_ops`（集合通信 + 点对点），供 daemon 上报与一次性内部使用。

### 5.2 检测层

- 检测组：Python 优先级；无命名域按 `hostUid` 物理节点分组。
- 计算类：`KERNEL_AICORE`(1.3)、`kernel_aivec`(1.3)、`memcpy_async`(2.5)，逐 rank 过滤无效值。
- CPU：节点级拉齐 + 4 卡回退；key 用节点名。
- comm：沿用现有算法，修正 degradation 表示。
- 新增 `pp_comm`：比较域 TP>CP>DP（退化到 PP stage 转置），`(max-own)/session > 5%`，按 PP 组上报；已检出
  计算类异常则跳过。
- 删除 `npu_bubble`。

### 5.3 算法层

按 Python `kmeans_detector.py` 重写：过滤 → z-score → 肘部法（候选区间/打分方向）→ KMeans++（共享 seed）→
Lloyd（收敛/空簇）→ 异常簇（非空簇 baseline/方向）→ 异常簇并集递归 → degradation（统一首次基线，min 方向
`baseline/value`）。

### 5.4 输出层

- 新增 `straggler_detection_result.json`：Python 扁平 schema。
- 新增结论文本模块（移植 `conclusion.py`），stdout 输出。
- stdout 汇总表：`类别 | 异常点位 | 劣化指数 | 劣化阈值`。
- `straggler_output.json`：保留 `{id?,started_at?,kpi?,profiler?}`；`node_result[].npu[]` 增加 aivec/memcpy；
  移除 `npu_bubble`；新增 `pp_comm_result`。同步更新 `report/report.go`、前端与 daemon/center 引用。
- 不新增 `<dataDir>/<business>/` 下的子目录。

## 6. 验收

1. **一次性 parity**：内部机器上用真实 level0 数据，Python 与 Go 的 `straggler_detection_result.json`
   逐项一致（类别、display_key、metric_value、阈值）。
2. **分布式**：daemon 上报 v2 载荷后，中心合成的 op_metric 中跨节点组的带宽与「一次性全量」一致；控制台
   op_metric 不含 `comm_ops`。
3. **构建**：`go build ./...`、`go test ./profiling/...`、`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`。
4. **回归**：daemon/center 启动、跑一轮、`/straggler/results/latest`、`/center/business/{name}/result|op_metric`
   正常。

## 7. 风险

- **本地无法端到端验证**：依赖内部机器的 level0 数据。
- **跨机时钟**：wall-clock 对齐的准确性依赖 NTP；必要时后续切换跨节点组为扁平池。
- **上报体积**：算子时序可能较大，需要上限与压缩策略。
- **`npu_bubble` 删除**：业务配置里已持久化的 `bubble_ns` 需兼容忽略/清理。
- **op_metric 口径变化**：center 合并/展示的列会变（generic 读取，预期不破坏）。
