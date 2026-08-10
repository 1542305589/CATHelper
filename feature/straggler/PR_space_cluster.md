# KPI 空间维度：多数簇聚类（MethodCluster）替换 mean/std Z-Score

## 背景

KPI 空间维度检测（`resource/` 的 `detectSpaceAnomalies`）此前对 6 个连续指标统一使用 mean/std Z-Score，存在三个结构性问题：

**1. 多卡异常被稀释漏检（核心缺陷）**

逐时间点 zscore 的 mean/std 由**全体在场卡**计算，异常卡自己也参与统计。异常卡越多，均值被拖、std 被撑得越厉害：

```
3/8 卡 = 80，5/8 卡 = 55 → mean=64.4, std≈12.9 → z(80)≈1.21 < 阈值 1.3 → 漏检
4/8 卡异常时 z≈0.93，彻底失效
```

稀释是**数学性质**：异常占比越大，z 越被压缩。节点级热降频、负载不均这类"多卡同时偏离"场景被静默漏掉。

**2. 双侧对称误报反方向**

`|v − mean| / std` 是绝对值的双侧判定，把**非异常方向的偏离**也报出来——一张偏冷的 temp 卡（temp 异常方向是高）、一张 util 偏高的卡（util 异常方向是低），都被判异常，但都没有可行动的根因，纯噪声。

**3. 参照和尺度被异常卡污染**

瞬时 peer 的 mean/std 取自含异常卡的全体——既是参照又是尺度，异常卡污染了两者。需要一个"干净的多数参照 + 不受异常影响的尺度"。

---

## 修改内容

### feature/straggler/resource/types.go

- `DetectionMethod` 新增 `MethodCluster = "cluster"`
- `DetectionConfig` 新增 `SpaceClusterK float64`（显著性阈值，默认 3.0）
- 6 个指标的 `SpaceMethod` 从 `MethodZScore` 切换为 `MethodCluster`：`temp` / `power` / `aicore_util` / `hbm_bandwidth_util` / `hbm_util` / `tx_bandwidth`
- `aicore_freq`（`direct`）和 4 个 error counter（`absolute`）不变

### feature/straggler/resource/space_detector.go

- 签名增加 `baselines` 参数（cluster 方法需要历史噪声做 scale）
- 新增辅助函数：
  - `gapSplitClusters`：递归二分，**两侧全分解**直到子块无主导间隙（`maxGap*2 < span`）→ 完整簇划分。相对 Profiler 均质化"只递归进异常侧"，全分解不丢中间层异常（如 65 与 85 两层都能切出）
  - `pickBaselineCluster`：**多数簇为基线**（"谁多谁有理"）；成员数并列按方向取极值簇（DirHigh→最低簇，DirLow→最高簇）
  - `clusterMean`：簇均值
  - `spaceMetricScale`：从历史基线自我标定噪声尺度（各卡 `1.4826×baseline.Mad` 的中位数），每指标一个值，无人工调参
- `case MethodCluster`（逐时间点）：
  1. 全分解 → 多数基线簇 → 基线均值
  2. **基线簇成员豁免**（它们是正常参照本身）
  3. 对每个**非基线簇成员单侧判定**（只查异常方向）：`z = |卡值 − 基线均值| / scale`，`z > k` → 标记
- `aggregateSpaceScores`：cluster 方法按 `mean_z`（= 持续占比 × 平均幅度）聚合，`mean_z > k` 判空间异常

**关键设计**：
- **多数基线**：单卡降频时多数卡兜底，2~7/8 多卡异常整簇检出，无稀释
- **逐点 z + 基线豁免**：异常簇内每张卡按自己的偏离单独评分（严重度精确到卡）；无主导间隙的散布舰队是单簇 → 全员豁免 → 边缘卡不误报
- **单侧判定**：只报该指标异常方向，消除双侧误报（偏冷 temp、偏高 util 不再误标）
- **历史噪声 scale**：指标无关（temp 区间量表也能用绝对偏差）、自我标定、对基线污染鲁棒
- **`mean_z` 聚合**：持续与幅度互补，无固定占比阈值
- **边界**：多数本身异常时（5/8 热卡）空间沉默，由时间维度 + 跨卡关联兜底

### feature/straggler/resource/report.go

- `runDetection` 第 5 步调用处传入 `baselines`（scale 需要）

### feature/straggler/main.go

- 新增独立 CLI 参数 `--space-cluster-k`（默认 3.0），不随 degradation 变化；`degradation` 继续只驱动 `SpaceZThreshold` / `TimeZThreshold`

### feature/straggler/resource/space_detector_test.go（新增）

| 测试 | 验证点 |
|------|--------|
| `TestSpaceFreqSingleDownclock` | freq MethodDirect 回归（peer 最小值排除自身） |
| `TestSpaceFreqAllNormal` / `TiedDownclock` / `AbsentCardNotFlagged` / `WithinGapTolerance` | freq 回归场景 |
| `TestSpaceClusterSingleAnomaly` | 单卡热 → cluster 检出 |
| `TestSpaceClusterMultiAnomaly` | 2 卡热 → 双双检出（zscore 会稀释）|
| `TestSpaceClusterMajorityNormalSpread` | 散布舰队无主导间隙 → 全员豁免不误报 |
| `TestSpaceClusterMajorityAnomaly` | 多数热 → 空间沉默（时间维度兜底）|
| `TestSpaceClusterTieBaseline` | 4/4 并列 → 方向取低簇做基线，热半检出 |
| `TestSpaceClusterDirLow` | util 低利用 → DirLow 单侧检出 |
| `TestSpaceClusterMeanZPersistence` | mean_z = 持续×幅度：1/3 行不报、3/3 行报 |

### feature/straggler/SPEC.md / DESIGN_NPU_RESOURCE.md

- Step 5 空间检测、指标表、配置默认值同步为 cluster 方法（全分解 + 多数基线 + 逐点 z + 基线豁免 + 单侧判定 + mean_z 聚合）
- DESIGN §5.1 方法 A 更新，注明设计要点与边界

---

## 兼容性

- **无 API 破坏**：`RunDetection()` / `RunDetectionFromData()` 签名不变，JSON 输出结构不变
- **非目标指标不变**：`aicore_freq`（direct）、4 个 error counter（absolute）检测逻辑、输出格式完全不变，freq 测试回归验证
- **scale 自我标定**：历史噪声来自基线，无需每指标人工调参；唯一新增旋钮 `--space-cluster-k`（默认 3.0），可选
- **边界行为有定义**：多数本身异常时空间沉默，由时间维度 + 跨卡关联兜底——这是 peer comparison 的固有边界，非缺陷
- **阈值语义变化**：cluster 的 z 是"干净历史噪声单位"，与旧 zscore 的"被污染的瞬时 std 单位"不可直接对比，`SpaceClusterK=3.0` 是新的显著性基线，需用真实数据校准
