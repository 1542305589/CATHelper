# Level0 一次性检测 parity 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `level0` 数据在一次性模式下的检测结论与项目内 Python skill 严格一致，并额外产出 Python schema 的结果文件。

**Architecture:** 以 Python `scripts/*.py` 为语义基准，逐个替换 Go 的解析口径、检测类别、聚类算法与输出层；保留 Go 现有文件契约与 daemon/center 可用性；新增 `straggler_detection_result.json` 双写。

**Tech Stack:** Go 1.22+（CGO 关闭）、`modernc.org/sqlite`、纯标准库。

**Spec:** `docs/superpowers/specs/2026-11-13-level0-python-parity-design.md`

## Global Constraints

- 只处理 `level0`；**不做数据级别判定**，默认 level0，仍按现有方式发现 `*.db`。
- 不移植 colocate / level1 / cluster / levelnone。
- `npu_bubble` 彻底删除（检测、字段、`bubble_ns`、报告、前端）。
- 保留 `straggler_output.json`、`op_metric/`、`analysis_result/detection_report.log` 的文件名契约。
- 新增 `straggler_detection_result.json`；不新增 `<dataDir>/<business>/` 下的子目录。
- 目标 `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`；所有命令在 `feature/straggler/` 下执行。
- 跨机时钟不可靠，但本计划按 Python 的 wall-clock 对齐实现（分布式另见 Plan 2）。

## Review Focus

- 同一数值在不同 rank 的**缺失/0/-99999**：Python 逐 rank 剔除，Go 当前整组跳过；错判会漏报或误报。
- **肘部法选 K**：打分方向或候选区间写错会静默改变所有类别结果。
- **min 方向（comm）的劣化指数**：`baseline/value` vs `value/baseline`，错了会让“越慢值越小”变成反向。
- **CPU 节点拉齐**：同节点卡值应被拉平，异常落在节点；按 rank 报会把一个节点算成多张卡。
- **跨 rank 带宽对齐失败**：无重叠时应回退 5ms 近邻，而不是直接丢弃。

---

### Task 1: 聚类算法对齐 Python

**Files:**
- Modify: `clustering/kmeans.go`
- Test: `clustering/kmeans_test.go`

**Interfaces:**
- Consumes: 现有 `Detect(values []float64, ratioThreshold float64, highIsAnomaly bool) []Result`。
- Produces: 同名同签名；`Result` 的 degradation 语义改为「统一第一次 KMeans 基线，min 方向取 `baseline/value`」。

- [ ] **Step 1: 增加失败测试** —— `TestDetectElbowKParity`：构造 3 簇 + 1 离群样本，断言被标记的 index 集合与 Python 规则一致；`TestDetectMinDirectionDegradation`：`[10,10,10,10,2]`、`highIsAnomaly=false`，断言异常项 degradation 约为 `10/2=5`（而非 `0.2`）；`TestDetectEmptyClusterBaseline`：使某簇为空时 baseline 取非空簇。
- [ ] **Step 2: 运行测试确认失败** —— `go test ./clustering/ -run 'TestDetect(ElbowKParity|MinDirectionDegradation|EmptyClusterBaseline)' -v`，期望 FAIL。
- [ ] **Step 3: 按 Python `kmeans_detector.py` 修改**：肘部法候选 `k=2..min(n,10)`、打分 `2I_k-I_{k-1}-I_{k+1}` 取最大、回退 2；KMeans++ 全流程共用 `rand.New(rand.NewSource(42))`、跳过已选索引；空簇质心放到离自身簇质心最远的点；baseline 只统计非空簇；递归对**异常簇并集**；degradation 统一第一次基线并按方向取比值。
- [ ] **Step 4: 运行测试确认通过** —— 同上命令，期望 PASS；再跑 `go test ./clustering/`。
- [ ] **Step 5: Commit** —— `git add clustering/kmeans.go clustering/kmeans_test.go && git commit -s -m "refactor(clustering): 聚类语义对齐 Python skill"`。

### Task 2: 解析指标口径对齐（ZP_Host / 域均值 / 新增列）

**Files:**
- Modify: `profiling/dataparse/data_process.go`、`profiling/dataparse/utils.go`
- Test: `profiling/dataparse/data_process_test.go`（新建）

**Interfaces:**
- Consumes: 现有 `TimeDiffForStep`、`WriteResultsToCSV`。
- Produces: CSV 增加 `KERNEL_AIVEC`、`MEMCPY_ASYNC`、`HostDuration`、memcpy 计数器；`ZP_Host`、`{domain}_Duration/_Count` 按 Python 口径。

- [ ] **Step 1: 失败测试** —— 用 `modernc.org/sqlite` 在 `t.TempDir()` 构造最小 `.db`（`TASK`/`STEP_TIME`/`CANN_API`/`MSTX_EVENTS`/`HOST_INFO`），断言：`ZP_Host` 不重复累加、`{domain}_Duration` 为普通均值且预置 0 列、`ZP_Count=0`、`MEMCPY_ASYNC`/`KERNEL_AIVEC` 列存在。
- [ ] **Step 2: 运行确认失败** —— `go test ./profiling/dataparse/ -run TestMetricsParity -v`，期望 FAIL。
- [ ] **Step 3: 实现** —— 按 Python `profilingdataparse.py` 的 `time_diff_for_step` / `write_results_to_csv`：host 表优先级 CANN_API→MSTX_EVENTS（不复用合并 map）、kernel host 只取 KERNEL_AICORE、域列白名单 + 全预置 0 + 普通均值、补 memcpy 计数、`ZP_Count=0`。
- [ ] **Step 4: 通过** —— `go test ./profiling/dataparse/`。
- [ ] **Step 5: Commit** —— `-s -m "refactor(dataparse): level0 指标口径对齐 Python"`。

### Task 3: 带宽回填纯算子化 + 组齐全 + 5ms 回退

**Files:**
- Modify: `profiling/dataparse/slow_domain.go`
- Test: `profiling/dataparse/slow_domain_test.go`

**Interfaces:**
- Produces: `type CommOp struct{ Domain, OpType string; Count int; StartNs, EndNs int64 }`；`func ComputeBandwidth(ops map[int][]CommOp, parallels map[string][][]int, noAlign bool) map[string]float64`（返回 `domain_opType_count → 带宽`）；`BackfillSlowDomainBandwidth` 改为调用它，供中心复用。

- [ ] **Step 1: 失败测试** —— 组内缺 rank → 该组无输出、其它组正常；时间窗无重叠但起点差 <5ms → 按近邻对齐；`noAlign` 走扁平池。
- [ ] **Step 2: 确认失败** —— `go test ./profiling/dataparse/ -run 'TestComputeBandwidth(GroupComplete|NearFallback|Flat)' -v`。
- [ ] **Step 3: 实现** —— 抽出纯算子入口；加入组齐全判断（level0 恒生效）、5ms 近邻回退、扁平池变体；`BackfillSlowDomainBandwidth` 从 `.db` 收集 `CommOp` 后调用。
- [ ] **Step 4: 通过** —— `go test ./profiling/dataparse/`。
- [ ] **Step 5: Commit** —— `-s -m "refactor(dataparse): 带宽回填抽出纯算子入口并补组齐全/近邻回退"`。

### Task 4: PP_WaitGap 回填

**Files:**
- Create: `profiling/dataparse/pp_wait.go`
- Test: `profiling/dataparse/pp_wait_test.go`

**Interfaces:**
- Produces: `func BackfillPPWait(inputPath string) (sessionNs int64, err error)`，把 `PP_WaitGap` 写进各 rank CSV；`config.PP_SESSION_NS` 保存会话时长。

- [ ] **Step 1: 失败测试** —— 构造含 `BatchSendRecv` / `Send` 与后续集合通信的最小算子序列，断言单卡 `PP_WaitGap` 与 `sessionNs`。
- [ ] **Step 2: 确认失败** —— `go test ./profiling/dataparse/ -run TestBackfillPPWait -v`。
- [ ] **Step 3: 实现** —— 按 Python `backfill_pp_wait`：锚点 BatchSendRecv 优先，否则 Send/Recv；找其后第一个集合通信、跳过中间 P2P；无后续则不计；会话时长取各 rank 合并窗口最大值。
- [ ] **Step 4: 通过** —— `go test ./profiling/dataparse/`。
- [ ] **Step 5: Commit** —— `-s -m "feat(dataparse): 回填 PP_WaitGap 供 pp_comm 检测"`。

### Task 5: 检测类别与无效值处理

**Files:**
- Modify: `profiling/detector/data_handler.go`、`profiling/detector/constants.go`、`profiling/detector/detection.go`
- Test: `profiling/detector/data_handler_test.go`（新建）

**Interfaces:**
- Produces: `cal`（KERNEL_AICORE）、`kernel_aivec`、`memcpy_async`、`cpu`；检测类别键与 Python 一致。

- [ ] **Step 1: 失败测试** —— 组内一张卡为 0/-99999 → 只剔除该卡，其它卡照常检测；`kernel_aivec` 用 1.3、`memcpy_async` 用 2.5；cpu 异常 key 为节点名。
- [ ] **Step 2: 确认失败** —— `go test ./profiling/detector/ -run 'TestDetect(PerRankFilter|Aivec|Memcpy|CpuNode)' -v`。
- [ ] **Step 3: 实现** —— 逐 rank 过滤；新增两个类别；cpu 改为节点级聚合与 4 卡回退；删除 `detectionZpBubbleData` 调用。
- [ ] **Step 4: 通过** —— `go test ./profiling/detector/`。
- [ ] **Step 5: Commit** —— `-s -m "feat(detector): level0 检测类别与无效值处理对齐 Python"`。

### Task 6: pp_comm 检测

**Files:**
- Create: `profiling/detector/pp_comm.go`
- Test: `profiling/detector/pp_comm_test.go`
- Modify: `profiling/detector/detection.go`（调用点）

**Interfaces:**
- Produces: `func DetectPPSlowDomain(parallels map[string][][]int, stepData map[string]map[int]float64, sessionNs int64, localResult config.DegradationData)`；结果键 `pp_comm`，degradation 为 0–1 占比。

- [ ] **Step 1: 失败测试** —— 比较域 TP 内 4 rank，某 rank 比组内最大低 >5% → 上报其 PP 组；可检出计算类异常时整段跳过。
- [ ] **Step 2: 确认失败** —— `go test ./profiling/detector/ -run TestDetectPPSlow -v`。
- [ ] **Step 3: 实现** —— 比较域 TP>CP>DP，否则 PP stage 转置；逐 rank `(max-own)/session`；按 PP 组取最大；前置跳过计算类异常。
- [ ] **Step 4: 通过** —— `go test ./profiling/detector/`。
- [ ] **Step 5: Commit** —— `-s -m "feat(detector): 新增 pp_comm 慢通信检测"`。

### Task 7: 彻底移除 npu_bubble

**Files:**
- Modify: `config/config.go`、`profiling/detector/*`、`report/report.go`、`utils/node_result.go`、`main.go`、`daemon/*`、`center/*`、`daemon/console.html`、`center/business.html`

**Interfaces:**
- Produces: 全仓无 `npu_bubble`/`ZP_Bubble`/`bubble_ns` 引用；旧业务配置中的 `bubble_ns` 被忽略且不报错。

- [ ] **Step 1: 删除检测与字段** —— 移除 `detectionZpBubbleData`、`NodeResult.NpuBubble`、报告/前端展示、阈值参数与持久化字段；对旧配置做忽略处理。
- [ ] **Step 2: 校验** —— `Get-ChildItem feature/straggler -Recurse -File -Include *.go,*.html | Select-String 'npu_bubble|ZP_Bubble|bubble_ns'` 应无输出（测试数据除外）。
- [ ] **Step 3: 构建** —— `go build ./...`；期望 0。
- [ ] **Step 4: Commit** —— `-s -m "refactor(straggler): 移除 npu_bubble 检测与相关配置"`。

### Task 8: 检测组选择（hostUid 回退）

**Files:**
- Modify: `profiling/detector/data_parser.go`、`profiling/detector/detection.go`
- Test: `profiling/detector/detection_test.go`（新建）

**Interfaces:**
- Produces: `GetCalDetectionGroup` 在无命名域时按 `hostUid` 分组；`HasNamedDomain` 门控 comm 组间检测。

- [ ] **Step 1: 失败测试** —— 无命名域 + 两组 hostUid → 返回按节点的检测组。
- [ ] **Step 2: 确认失败** —— `go test ./profiling/detector/ -run TestDetectionGroupHostFallback -v`。
- [ ] **Step 3: 实现** —— 优先级列表后追加 hostUid 回退；补充 `HasNamedDomain` 标志与门控。
- [ ] **Step 4: 通过** —— `go test ./profiling/detector/`。
- [ ] **Step 5: Commit** —— `-s -m "feat(detector): 无命名域时按 hostUid 回退检测组"`。

### Task 9: 结果文件与结论输出

**Files:**
- Create: `report/result_json.go`、`report/conclusion.go`
- Test: `report/result_json_test.go`、`report/conclusion_test.go`
- Modify: `main.go`

**Interfaces:**
- Produces: `func WriteDetectionResult(dir string, result map[string]map[string]float64, thresholds config.Thresholds) error`，生成 Python 扁平 schema；`func RenderConclusion(result map[string]map[string]float64, thresholds config.Thresholds) string`。

- [ ] **Step 1: 失败测试** —— 给定 result（6 类，含空类别），断言 JSON 结构为 `{类别:[{display_key,metric_value,is_abnormal}]}` 且空类别为 `[]`；结论包含异常项与阈值。
- [ ] **Step 2: 确认失败** —— `go test ./report/ -run 'TestWriteDetectionResult|TestRenderConclusion' -v`。
- [ ] **Step 3: 实现** —— 按项目内 skill 的 `utils.write_result` 与 `conclusion.py` 输出规则；stdout 汇总表按 `类别|异常点位|劣化指数|劣化阈值`。
- [ ] **Step 4: 通过** —— `go test ./report/`。
- [ ] **Step 5: Commit** —— `-s -m "feat(report): 新增 Python schema 结果文件与自然语言结论"`。

### Task 10: Go 结果结构扩展与 main 接线

**Files:**
- Modify: `utils/node_result.go`、`main.go`、`report/report.go`
- Test: `utils/node_result_test.go`（新建）

**Interfaces:**
- Produces: `NodeResult.Npu[]` 增加 `Aivec`/`Memcpy`；`NodeOutput` 增加 `PPCommResult`；一次性流程写 `straggler_output.json` + `straggler_detection_result.json`。

- [ ] **Step 1: 失败测试** —— `BuildNodeResult` 对 6 类结果生成 node/npu/comm/pp_comm 结构，且不含 bubble。
- [ ] **Step 2: 确认失败** —— `go test ./utils/ -run TestBuildNodeResultLevel0 -v`。
- [ ] **Step 3: 实现** —— 扩展结构并接线；更新 `report.GenerateReport` 的类别表。
- [ ] **Step 4: 通过** —— `go test ./utils/ ./report/`；`go build ./...`。
- [ ] **Step 5: Commit** —— `-s -m "feat(straggler): 结果结构扩展并接入新检测类别"`。

### Task 11: 一次性端到端回归

**Files:**
- Test: `profiling/detector/level0_parity_test.go`（新建，使用合成输入，不依赖真实 `.db`）

**Interfaces:**
- Consumes: 前序任务的检测与输出函数。

- [ ] **Step 1: 写回归测试** —— 构造 4 rank / 1 节点的合成 `stepData` + `parallels`（含一个慢卡、一个慢通信组、一个节点级 CPU 异常），走 `DelimitDetection` → `BuildNodeResult` → `WriteDetectionResult`，断言结果文件内容。
- [ ] **Step 2: 确认失败** —— `go test ./profiling/detector/ -run TestLevel0ParityEndToEnd -v`。
- [ ] **Step 3: 修到通过** —— 只允许修实现，不放宽断言。
- [ ] **Step 4: 全量验证** —— `go test ./...`、`go build ./...`、`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`。
- [ ] **Step 5: Commit** —— `-s -m "test(straggler): 增加 level0 一次性检测端到端回归"`。

## 说明

- 本计划只覆盖一次性 parity。分布式（daemon 上报 `comm_ops` + 中心回填）见 Plan 2。
- 端到端与真实数据的最终一致性由内部机器验证；本计划的测试均为本地可跑的合成输入。
