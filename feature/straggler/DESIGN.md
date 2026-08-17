# 慢节点检测算法 — 设计文档

## 模块接口

### config
```go
var FilePath string                                  // CLI path= 设置
var CalThreshold float64                             // = 1 + degradation（默认 1.3）
var CommThreshold float64                            // = 1 + degradation × 5（默认 2.5）

type DegradationData map[string]map[string]float64   // 类别 → (key → 劣化分数)
func NewDegradationData() DegradationData
func (d DegradationData) AddSingle(category string, rank int, degradation float64)
func (d DegradationData) AddGroup(category string, ranks []int, degradation float64)
```
- **Key 编码**：单卡 `strconv.Itoa(rank)`（如 `"0"`），组 `sort + strings.Join(ranks, ",")`（如 `"0,2,4"`）
- **AddGroup 去重**：已存在 key 时保留**最大**劣化值

### dataparse（profiling/dataparse）
```go
func DataParsing(folderPath string)                          // 入口：遍历 .db → StartProcess
func StartProcess(dbFiles []string, outDir string) error     // 信号量（cap=4）+ WaitGroup
func ProcessDatabase(dbPath string, outDir string) error     // 单文件完整管线
func GetAllStepTimes(db *sql.DB) ([]StepTime, error)         // 3 级降级
func GetStepTimesFromSTEP_TIME(db *sql.DB) ([]StepTime, error)
func GetStepTimesFromTASK(db *sql.DB) ([]StepTime, error)
func TimeDiffForStep(db, xpToGroupName, stepTime) (PerformanceMetrics, error)
func GetAvgKernelTaskDuration(db *sql.DB, stepTime StepTime) (int, error)
func WriteResultsToCSV(outputFile string, pMS []PerformanceMetrics) error
func queryHostInfo(db *sql.DB) (hostUid, hostName string, err error) // 查询 HOST_INFO
func queryNpuID(db *sql.DB) (int, error)                     // 查询 NPU_INFO.id
func readGroupInfo(db, rankStr, outputDir) (map[string]interface{}, map[string]string, error) // META_DATA → group_info JSON + xpToGroupName
func writeHostInfo(outputDir, rankStr, hostUid, hostName string) // 写入 host_info_{N}.json
func writeNpuInfo(outputDir, rankStr string, npuID int)      // 写入 npu_info_{N}.json
func CalculateMean(values []int) (int, error)
func CalculateMidMeanPair(stats []OpStat) (meanDuration, meanCount int, err error)
```

**ProcessDatabase 执行顺序**：
1. `sql.Open("sqlite", path+"?mode=ro")` + WAL 模式
2. 创建 3 个索引（IF NOT EXISTS，幂等）
3. `extractGlobalRankFromFilename` → rank 字符串
4. `queryHostInfo` → `SELECT hostUid, hostName FROM HOST_INFO LIMIT 1`（识别卡所属物理节点与节点名）
5. `queryNpuID` → `SELECT id FROM NPU_INFO LIMIT 1`（NPU 编号，节点聚合输出用）
6. `readGroupInfo` → META_DATA `parallel_group_info` → group_info JSON（sync.Once 写入）+ xpToGroupName 映射（短名 → STRING_IDS 组名字符串，如 `"tp" → "group_name_3"`）
7. `GetAllStepTimes` → 合并为单 step（minStart → maxEnd）
8. `TimeDiffForStep` → 计算所有指标
9. `WriteResultsToCSV` → 单行 CSV
10. `writeHostInfo` → 写入 `op_metric/host_info_{N}.json`（rank → hostUid/hostName 映射）
11. `writeNpuInfo` → 写入 `op_metric/npu_info_{N}.json`（rank → NPU id）

**Step 时间降级链**：
1. `STEP_TIME` 表 → `SELECT id, startNs, endNs ORDER BY id DESC` → 反转升序
2. `TASK` + `STRING_IDS` + `MSTX_EVENTS` → 正则匹配 `^step\s+\d+$` → 查 connectionId → 查 TASK 时间
3. 哨兵：`{ID: -1, StartNs: math.MinInt, EndNs: math.MaxInt}`

**指标计算（TimeDiffForStep）**：
| 指标 | 计算方式 |
|------|---------|
| ZP_Host | 所有通信算子和 KERNEL_AICORE 的 `HEndNs - HStartNs` 均值（HStartNs > 0 && HEndNs ≥ HStartNs）；空 → -99999 |
| ZP_Bubble | 所有 `OpStartNs - HostEndNs > 0` 的正值均值（HEndNs > 0）；空 → -99999 |
| ZP_Duration | 收集所有通信区间 → `mergeIntervalsSimple` 合并重叠 → 总跨度 |
| ZP_Device | `stepDuration - ZP_Duration`（钳位到 0） |
| ZP_Kernel | `SELECT AVG(endNs - startNs) FROM TASK ... WHERE KERNEL_AICORE` |
| 各域 Duration/Count | 域内算子 → `CalculateMidMeanPair`（去 min/max 后均值） |

**通信组 Duration 契约（{domain}_Duration 列）**：
- COMMUNICATION_OP.groupName 是 STRING_IDS 中组名字符串的 id；`parallel_group_info` 的**顶层 key**（长名，如 `"group_name_3"`）即 STRING_IDS 里的组名字符串，而每项的 `group_name` 字段是**短名**（如 `"tp"`）
- `xpToGroupName` 以短名为键、长名为值；`idToXp` 反向映射为 **STRING_IDS id → 短名**，CSV 的域列以短名命名（`tp_Duration, tp_Count`），与 detector 的域常量一致
- 某域在 STRING_IDS/COMMUNICATION_OP 中无算子 → 该域无列（正常，无数据可测）

**三种数据缺失场景**：
- `xpToGroupName` 为空 → 全部填充 -99999，ZP_Kernel/DataLoader 独立查询
- `groupNameIds` 为空（组名不在 STRING_IDS）→ 通信指标填充 -99999
- `deviceOps` 为空 → 除 ZP_Host 外的指标填充 -99999，ZP_Host 回退用 KERNEL_AICORE Host 耗时

**区间合并（mergeIntervalsSimple）**：按 Start 排序 → 遍历合并重叠区间 → 累加非重叠部分总长。

**并发控制**：
```go
var csvMutex sync.Mutex                    // CSV 写入全局锁
var fileWriteOnce map[string]*sync.Once    // group_info/host_info/npu_info JSON 去重
var fileWriteOnceMu sync.Mutex             // 保护 fileWriteOnce map
```
- DB 并发：`make(chan struct{}, 4)` 信号量
- CSV：全局 Mutex（每 goroutine 写不同文件，但保留锁保安全）
- group_info/host_info/npu_info JSON：`sync.Once` 每文件名（同机卡拓扑/hostUid/NPU id 相同，只需写一次）

### detector（profiling/detector）
```go
func GetCurDetectionInfo(jobPath string) (map[string][][]int, []int)   // parallels + validRanks
func GetCurJobLastStepData(ranks []int) map[string]map[int]float64    // CSV → 单快照
func GetHostUidMapping(jobPath string, ranks []int) map[int]string    // 读取 host_info_*.json
func DelimitDetection(StepData map[string]map[int]float64, parallels map[string][][]int, validRanks []int) config.DegradationData
func GetCalDetectionGroup(parallels map[string][][]int, curNpus []int) (string, [][]int)
func DebugRankScores(stepData map[string]map[int]float64, validRanks []int) map[int]map[string]float64   // --debug-output 用
func DebugCommScores(stepData map[string]map[int]float64, parallels map[string][][]int) map[string]map[string]float64
```

**GetCurDetectionInfo**：遍历 `op_metric/group_info_*.json`，收集所有 rank ID 和域名称（`group_name` 字段，短名），对每个域调用 `getDetectionJobParallelInfo` 提取组，过滤 < 2 卡的组，返回 parallels 映射和排序 validRanks。**无 group_info 文件（组名未注册）时**：回退从 `global_rank_*.csv` 文件名收集 rank（该文件无条件写），返回空 parallels → 主流程降级 cal-only。

**GetCurJobLastStepData**：对每个 rank 读 CSV → `map[列名][]float64` → 取倒数第二行（n > 1 时 n-2）→ 跳过 -99999 → 返回 `map[指标名]map[rank]值`。

**主检测组优先级**：`tp → exp → ep → tp_exp → cp → cp2 → cp_ulysses → cp_ring → dp → dp_cp → dp_modulo_exp_cp`

**并行域去重**：`checkRankParallelExist` 通过 `parallelInfo map[int]map[int]bool` 追踪每个 rank 已归属的组，避免同域组重复。

## 检测逻辑

#### 慢计算（getSlowCalculateRanks → detCalForOneGroup）
```
对主检测组每个子组：
  1. 检查 ZP_Kernel 可用性（组内所有卡 > 0）
     ✓ → 指标 = ZP_Kernel，方向 = "max"
     ✗ → 指标 = ZP_Duration，方向 = "min"
  2. 收集非零值，要求 >= minRanksInGroup(2)
  3. kmeans 比例检测 → AddSingle("cal", rank, degradation)
```
主检测组无可用并行域（组名未注册/仅未知域如 mc2）时，GetCalDetectionGroup 降级为**全体 rank 一组**（default_group），cal 仍可检测；comm/CPU/Bubble 无数据保持静默。

#### 慢通信（detectionAllCommunicationParallel → HomogenizationForSlowCommunication）
```
对每个非 PP/非 embd 域：
  ppStageNum = len(parallels["pp"][0])（若无 PP 则为 1）
  1. 每个子组内部排序，组间字典序排序
  2. 每组取 {domain}_Duration 最小的卡为代表
  3. detectionCards 按 ppStageNum 均分桶
  4. 每桶内对代表卡做 kmeans 比例检测（方向 "max"）
  5. 异常代表卡通过 rank2Group 映射回完整组
  → AddGroup("comm", fullGroup, degradation)
```
PP=1 时所有代表卡在同一桶，算法天然降级为普通聚类。

#### 慢CPU（getSlowHostRanksByHomogenize）
```
1. 收集所有 validRanks 的 ZP_Host 值
2. smoothByHostUid(values, ranks, rankToHostUid) — 原地修改：
   从 host_info_{N}.json 读取 hostUid 映射（数据解析阶段从 HOST_INFO 表查询）
   相同 hostUid 的卡归为同一物理节点：
     > 2 个 → 去掉节点内 min/max，计算剩余均值
     ≤ 2 个 → 普通均值
   用均值覆盖节点内所有卡的值
   无 hostUid 的卡保持原始值不变
3. kmeans 比例检测（方向 "max"）→ AddSingle("cpu", rank, degradation)
```

注：旧版本硬编码每 4 个连续 rank 为一台机器，现已改为从 profiler 数据库 `HOST_INFO` 表读取实际 `hostUid` 进行分组。若 `HOST_INFO` 表不存在则对应卡跳过预处理。

#### NPU Bubble（detectionZpBubbleData）
```go
for npuID, value := range ZP_Bubble:
    if value > 0 && value < 5000:
        AddSingle("npu_bubble", npuID, value)
```
注：阈值硬编码 `< 5000`；config 中无对应参数（`zpBubbleAbnormalBoundary` 已移除）。

### clustering（共享 kmeans 比例检测）
```go
func HomogenizationComparisonFunc(fileRanks []int, alignedData []float64,
    degradationPercent float64, abnormalType string) ([]int, []float64)
```
`profiling/detector/clustering.go` 只是包装：把参数透传给共享包
`feature/straggler/clustering` 的 `Detect(values, threshold, abnormalType != "min")`，
再把 `Result.Index` 映射回 `fileRanks`、`Result.Ratio` 作为退化值。

**核心流程**（与 KPI 资源检测的空间 cluster 共享同一算法）：
```
1. 过滤值 <= 0；不足 2 个 → 无异常退出
2. Z-score 标准化（std≈0 → 强制 1）
3. 肘部法选 k（K=2..min(n,10)，取 inertia 二阶差分最大）
4. kmeans++ 初始化（首个质心 = data[0]，后续 D² 加权采样，固定种子 kmeansSeed=42）
5. Lloyd 迭代（≤300 轮，空簇处理，收敛 1e-9）
6. 基线簇 = 方向极值簇（"max"→最小均值簇，"min"→最大均值簇）
7. 簇均值比 > threshold → 异常簇
   "max": 簇均值 / 基线均值 > threshold
   "min": 基线均值 / 簇均值 > threshold
8. 对异常簇递归（深度 ≤10）：更深层异常替换父层，更深层无异常保持父层
9. 返回最深异常簇；degradation = 对应簇比例
```

固定种子（kmeansSeed=42）：kmeans++ 采样确定性，同一数据多次运行结果一致。

时间复杂度：kmeans O(n·k·iter)（n ≤ 64，k ≤ 10，iter ≤ 300），递归深度 ≤ 10；空间复杂度 O(n)。

### utils
```go
func BuildNodeResult(finalResult map[string]map[string]float64, parallels map[string][][]int, debug *DebugInfo) (*NodeOutput, error)
func CheckFileOrDirectoryReadMode(path string) bool
func CheckFileOrDirectoryIsSoftLink(path string) bool
func TransferFloatArrayToInt(ids []interface{}) []int
func ReadFile(filePath string) ([]byte, error)
```

**BuildNodeResult 逻辑**（节点聚合输出，不写文件，返回 `NodeOutput` 供 main 合并进 `straggler_output.json` 的 `profiler` 段）：
1. 读 `op_metric/host_info_{N}.json`（hostName）+ `npu_info_{N}.json`（NPU id）作为每 rank 元数据
2. cal / npu_bubble（逐 rank）→ 按 hostname 分组、按 NPU id 聚合 → `node_result[].npu[]`，只含有异常的节点/NPU
3. cpu（逐 rank，节点级）→ `node_result[].cpu`（节点内 rank 值相同，取共享值）
4. comm → 用 `findDomainForRanks` 解析域名 → `comm_domain_result[域名][组key] = score`
5. stdout 逐类摘要（慢计算/慢通信/慢CPU/Bubble，单节点跳过慢CPU）；JSON 由 main.go 合并写入运行目录 `straggler_output.json`（`{"kpi": ..., "profiler": ...}`）。`--debug-output` 时传入 `DebugInfo{ValidRanks, RankScores, CommScores}`，输出全部节点/NPU（含正常）及其诊断分

### report
```go
func WriteReport(stepData, parallels, validRanks, outputDir, detectionResult, inputPath, degradation) string
func GenerateReport(stepData, parallels, validRanks, detectionResult, inputPath, degradation) string
```

**报告章节**：头部 → 并行域拓扑 → 检测摘要表 → ZP_Kernel 柱状图（Top 30 + Bottom 5）→ ZP_Host 柱状图 → 总通信时间 → 各域分组对比（min/mean/max + 柱状图）。

**常量**：柱状图 `█`，最大宽度 40，Top N = 30，Bottom N = 5。

**时间格式化**：`≥1e9` → s，`≥1e6` → ms，`≥1e3` → µs，其余 → ns。

## 数据结构

```go
type StepTime struct {
    ID      int   // Step 编号（合并后 = 0）
    StartNs int   // 开始时间（ns）
    EndNs   int   // 结束时间（ns）
}

type CommunicationOp struct {
    OpStreamIndex int   // COMMUNICATION_OP._rowid_
    OpName        int   // 算子名称（STRING_IDS ID）
    StartNs       int   // 设备侧开始时间
    EndNs         int   // 设备侧结束时间
    HStartNs      int   // Host 侧开始时间（由 CANN_API/MSTX_EVENTS 关联填充）
    HEndNs        int   // Host 侧结束时间
    Count         int
    ConnectionID  int   // 设备-主机关联键
    DomainID      int   // 并行域 ID（COMMUNICATION_OP.groupName，即 STRING_IDS 中组名字符串的 id）
}

type PerformanceMetrics struct {
    HostUid      string         // 物理节点标识（从 HOST_INFO 表读取）
    StepIndex    int            // 合并后 = 0
    StepDuration int            // maxEndNs - minStartNs
    ZPDevice     int            // 非通信时间 = stepDuration - ZP_Duration（钳位到 0）
    ZPDuration   int            // 总通信时间（合并区间后）
    ZPHost       int            // 平均 Host 耗时（-99999 表示缺失）
    ZPBubble     int            // 平均 Bubble 时间（-99999 表示缺失）
    ZPCount      int            // 未使用（恒为 -99999）
    ZPKernel     int            // 平均 KERNEL_AICORE 耗时
    DataLoader   int            // DataLoader 耗时
    Durations    map[string]int // 各域通信耗时（按短名，如 "tp"）
    Counts       map[string]int // 各域通信计数
}

type OpStat struct { Duration, Count int }
type Interval struct { Start, End int }
type HostOp struct  { StartNs, EndNs int }
```

## 关键 SQL

```sql
-- 物理节点标识
SELECT hostUid, hostName FROM HOST_INFO LIMIT 1

-- NPU 编号（节点聚合输出）
SELECT id FROM NPU_INFO LIMIT 1

-- 并行域配置
SELECT value FROM META_DATA WHERE name = 'parallel_group_info'

-- 组名字符串 → STRING_IDS id
SELECT value, id FROM STRING_IDS WHERE value IN (?, ...)

-- 通信算子（step 时间窗口内 + 指定 groupName ID）
SELECT opName, startNs, endNs, connectionId, count, _rowid_, groupName
FROM COMMUNICATION_OP
WHERE groupName IN (?, ...) AND startNs >= ? AND endNs <= ?
ORDER BY startNs ASC

-- Host 时序（批量查 connectionId）
SELECT startNs, endNs, connectionId
FROM {CANN_API|MSTX_EVENTS}
WHERE connectionId IN (?, ...)

-- KERNEL_AICORE connectionId
SELECT t.connectionId FROM TASK t
INNER JOIN STRING_IDS s ON t.taskType = s.id
WHERE s.value IN ('KERNEL_AICORE') AND t.startNs >= ? AND t.endNs <= ? AND t.connectionId > 0

-- 平均 Kernel 耗时
SELECT AVG(t.endNs - t.startNs) FROM TASK t
INNER JOIN STRING_IDS s ON t.taskType = s.id
WHERE s.value IN ('KERNEL_AICORE') AND t.startNs >= ? AND t.endNs <= ?

-- DataLoader
SELECT startNs, endNs FROM MSTX_EVENTS
WHERE message = ? AND startNs >= ? AND endNs <= ? LIMIT 1
```

## 错误处理

**致命（程序终止）**：
- 无 CLI 参数 / 缺 path / path 非目录
- 目录下无 `.db` 文件
- 获取并行域/有效 rank 失败
- 无有效 step data
- 检测返回空

**非致命（跳过/降级，继续执行）**：
| 场景 | 处理 |
|------|------|
| 单个 .db 处理失败 | 日志 + 继续下一个 |
| CSV 为空 | 跳过该 rank |
| group_info JSON 无效 | 跳过该 rank 的拓扑 |
| CANN_API/MSTX_EVENTS 表不存在 | 跳过 Host 时间查询 |
| DataLoader 查询失败 | DataLoader = 0 |
| Kernel 查询无数据 | ZP_Kernel = 0 |
| 通信耗时 > step 总耗时 | ZP_Device 钳位到 0 + 警告 |
| 某域在 STRING_IDS/COMMUNICATION_OP 无算子 | 该域无 Durations 列（不参与慢通信检测） |
| 组名未注册（无并行拓扑） | 降级 cal-only：validRanks 从 global_rank_*.csv 收集，全体 rank 一组检测慢计算；comm/CPU/Bubble 无数据不检测 |

## 日志前缀

`[SLOWNODE ALGO]` 算法通用 | `[DATA PROCESS]` 数据解析 | `[WARN]` 警告 | `[REPORT]` 报告生成
