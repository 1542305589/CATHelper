package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// DetectionResultFile is the Python-schema artifact written alongside the Go
// straggler_output.json (dual write).
const DetectionResultFile = "straggler_detection_result.json"

// categoryOrder maps the internal category keys to the Python schema keys, in
// display order.
var categoryOrder = []struct {
	key   string
	label string
}{
	{"cal", "KERNEL_AICORE"},
	{"kernel_aivec", "kernel_aivec"},
	{"memcpy_async", "memcpy_async"},
	{"comm", "comm"},
	{"pp_comm", "pp_comm"},
	{"cpu", "cpu"},
}

// detectionItem is one Python-schema result entry.
type detectionItem struct {
	DisplayKey  string  `json:"display_key"`
	MetricValue float64 `json:"metric_value"`
	IsAbnormal  bool    `json:"is_abnormal"`
}

// WriteDetectionResult writes the Python flat-schema result file into dir.
// All six categories are present (empty arrays when clean); items are sorted by
// degradation descending (display key breaks ties, so output is deterministic).
func WriteDetectionResult(dir string, result config.DegradationData, parallels map[string][][]int) error {
	raw, err := BuildDetectionResultJSON(result, parallels)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, DetectionResultFile), raw, 0o644)
}

// BuildDetectionResultJSON marshals the Python flat-schema result document.
func BuildDetectionResultJSON(result config.DegradationData, parallels map[string][][]int) ([]byte, error) {
	out := make(map[string][]detectionItem, len(categoryOrder))
	for _, cat := range categoryOrder {
		items := make([]detectionItem, 0, len(result[cat.key]))
		for key, v := range result[cat.key] {
			items = append(items, detectionItem{
				DisplayKey:  detectionDisplayKey(cat.label, key, parallels),
				MetricValue: v,
				IsAbnormal:  true,
			})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].MetricValue != items[j].MetricValue {
				return items[i].MetricValue > items[j].MetricValue
			}
			return items[i].DisplayKey < items[j].DisplayKey
		})
		out[cat.label] = items
	}
	return json.MarshalIndent(out, "", "  ")
}

// detectionDisplayKey maps an internal result key to the Python display key.
// Python formats group ranks with ", " (comma + space): comm groups become
// "tp[0, 1]" and pp_comm groups become "[0, 1]" (no category prefix). Other
// categories use the key as-is (rank or node name).
func detectionDisplayKey(category, key string, parallels map[string][][]int) string {
	switch category {
	case "comm":
		ranks := parseRankList(key)
		if len(ranks) == 0 {
			return key
		}
		target := joinRanks(ranks)
		inner := joinDisplayRanks(ranks)
		for _, domain := range sortedDomains(parallels) {
			for _, g := range parallels[domain] {
				if joinRanks(g) == target {
					return domain + "[" + inner + "]"
				}
			}
		}
		return "[" + inner + "]"
	case "pp_comm":
		ranks := parseRankList(key)
		if len(ranks) == 0 {
			return "[" + key + "]"
		}
		return "[" + joinDisplayRanks(ranks) + "]"
	}
	return key
}

// joinDisplayRanks renders ranks with Python's ", " separator.
func joinDisplayRanks(ranks []int) string {
	parts := make([]string, len(ranks))
	for i, r := range ranks {
		parts[i] = strconv.Itoa(r)
	}
	return strings.Join(parts, ", ")
}

// RenderSummaryTable renders the Python-style "类别 | 异常点位 | 劣化指数 | 劣化阈值"
// table. When nothing is anomalous it renders a single "无异常" line.
func RenderSummaryTable(result config.DegradationData, parallels map[string][][]int, thresholds config.Thresholds) string {
	headers := []string{"类别", "异常点位", "劣化指数", "劣化阈值"}
	var rows [][]string
	for _, cat := range categoryOrder {
		data := result[cat.key]
		if len(data) == 0 {
			continue
		}
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if data[keys[i]] != data[keys[j]] {
				return data[keys[i]] > data[keys[j]]
			}
			return keys[i] < keys[j]
		})
		pointers := make([]string, 0, len(keys))
		degs := make([]string, 0, len(keys))
		for _, k := range keys {
			display := detectionDisplayKey(cat.label, k, parallels)
			pointers = append(pointers, display)
			if cat.key == "pp_comm" {
				// Python renders PP wait as a share of the session (val * 100).
				degs = append(degs, fmt.Sprintf("%s:%.6g%%", display, data[k]*100))
			} else {
				degs = append(degs, fmt.Sprintf("%s:%.6g", display, data[k]))
			}
		}
		rows = append(rows, []string{cat.label, strings.Join(pointers, "，"), strings.Join(degs, "，"), thresholdLabel(cat.key, thresholds)})
	}
	if len(rows) == 0 {
		return "无异常\n"
	}
	return renderBoxTable(headers, rows)
}

// thresholdLabel renders the degradation threshold for one category.
func thresholdLabel(category string, th config.Thresholds) string {
	switch category {
	case "cal", "kernel_aivec":
		return fmt.Sprintf("%.6gx", th.Cal)
	case "cpu", "memcpy_async":
		return fmt.Sprintf("%.6gx", th.CPU)
	case "comm":
		return fmt.Sprintf("%.6gx", th.CommThreshold)
	case "pp_comm":
		return ">5%"
	}
	return "-"
}

// renderBoxTable renders an ASCII box table, adjusting column widths.
func renderBoxTable(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = displayWidth(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if w := displayWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	var b strings.Builder
	sep := func() {
		b.WriteString("+")
		for _, w := range widths {
			b.WriteString(strings.Repeat("-", w+2))
			b.WriteString("+")
		}
		b.WriteString("\n")
	}
	writeRow := func(cells []string) {
		b.WriteString("|")
		for i, c := range cells {
			b.WriteString(" ")
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(c)))
			b.WriteString(" |")
		}
		b.WriteString("\n")
	}
	sep()
	writeRow(headers)
	sep()
	for _, row := range rows {
		writeRow(row)
		sep()
	}
	return b.String()
}

// displayWidth approximates terminal width: CJK runes count as 2 columns.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x1100 && (r <= 0x115F || r == 0x2329 || r == 0x232A ||
			(r >= 0x2E80 && r <= 0xA4CF) || (r >= 0xAC00 && r <= 0xD7A3) ||
			(r >= 0xF900 && r <= 0xFAFF) || (r >= 0xFE30 && r <= 0xFE6F) ||
			(r >= 0xFF00 && r <= 0xFF60) || (r >= 0xFFE0 && r <= 0xFFE6)) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// RenderConclusion renders the natural-language conclusion grouped by
// compute / communication / host / memcpy. A clean result says 未检出异常.
// It mirrors Python's conclusion.py in two non-obvious ways: a compute hit
// suppresses PP detection, so the text says PP was not evaluated (an empty
// pp_comm must not read as "PP is healthy"); and memcpy_async carries its known
// false-positive caveat.
func RenderConclusion(result config.DegradationData, parallels map[string][][]int, thresholds config.Thresholds) string {
	total := 0
	for _, cat := range categoryOrder {
		total += len(result[cat.key])
	}
	if total == 0 {
		return "未检出异常。\n"
	}
	var b strings.Builder
	b.WriteString("检测结论：\n")
	writeLine := func(title, key string, asPercent bool) {
		data := result[key]
		if len(data) == 0 {
			return
		}
		keys := sortedResultKeys(data)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			display := detectionDisplayKey(key, k, parallels)
			if asPercent {
				parts = append(parts, fmt.Sprintf("%s(%.2f%%)", display, data[k]*100))
			} else {
				parts = append(parts, fmt.Sprintf("%s(%.2fx)", display, data[k]))
			}
		}
		b.WriteString(fmt.Sprintf("- %s：%d 项 —— %s\n", title, len(keys), strings.Join(parts, "，")))
	}
	// Compute group: KERNEL_AICORE + kernel_aivec.
	compute := mergeCategory(result, "cal", "kernel_aivec")
	if len(compute) > 0 {
		b.WriteString(fmt.Sprintf("- 慢计算（KERNEL_AICORE / kernel_aivec）：%d 项\n", len(compute)))
	}
	writeLine("慢通信 comm", "comm", false)
	writeLine("PP 通信 pp_comm", "pp_comm", true)
	writeLine("慢CPU 节点", "cpu", false)
	writeLine("搬运 memcpy_async", "memcpy_async", false)
	b.WriteString(fmt.Sprintf("阈值：计算 %.6gx / IO %.6gx / 通信 %.6gx / PP >5%%\n",
		thresholds.Cal, thresholds.CPU, thresholds.CommThreshold))

	// PP detection is skipped when compute anomalies were found, because its
	// measurement window necessarily contains compute operators.
	computeHit := len(result["cal"]) > 0 || len(result["kernel_aivec"]) > 0
	if computeHit && len(result["pp_comm"]) == 0 {
		b.WriteString("\nPP 点对点通信等待（pp_comm）未评估：本次已检出计算类故障" +
			"（KERNEL_AICORE / kernel_aivec），PP 检测被跳过" +
			"（其统计区间内必含计算算子，会被污染），不能据此判断 PP 是否正常。\n")
	}

	if memcpy := result["memcpy_async"]; len(memcpy) > 0 {
		b.WriteString("\n" + memcpyNote(memcpy) + "\n")
	}
	return b.String()
}

// memcpyNote mirrors Python's _memcpy_note: memcpy_async averages copy-operator
// durations of mixed kinds (H2D/D2H, D2D, intra-device), so a few ranks can be
// structurally high without a real IO regression.
func memcpyNote(items map[string]float64) string {
	ids := make([]int, 0, len(items))
	for k := range items {
		if r, err := strconv.Atoi(k); err == nil {
			ids = append(ids, r)
		}
	}
	sort.Ints(ids)
	var who string
	switch len(ids) {
	case 0:
		who = "相关卡"
	case 1:
		who = fmt.Sprintf("rank%d", ids[0])
	case 2:
		who = fmt.Sprintf("rank%d 和 rank%d", ids[0], ids[1])
	default:
		parts := make([]string, len(ids))
		for i, r := range ids {
			parts[i] = fmt.Sprintf("rank%d", r)
		}
		who = strings.Join(parts, "、")
	}
	return `关于 memcpy_async 的说明：该指标统计的是「拷贝算子的平均耗时」，包括：设备拷贝（H2D/D2H）、
设备内拷贝、设备间拷贝（D2D）。部分情况下会让少数卡的均值结构性偏高，进而导致误报：

1. **PP>1 且 TP>1（Megatron 框架特有）**：只有首/末 stage 的 tp0 需要把 data/label 从主机搬到
   卡上、再广播给同 TP 组，其余卡为 0。例：PP4TP4 时就是 rank0 和 rank12。
2. **PP=1 且 TP>1（Megatron 框架特有，如 TP4DP4）**：各 DP 组的 tp0 承担同样的 H2D，
   例：rank0/4/8/12。
3. **任务数不均**：任务少的卡缺少短任务，平均值容易贴线偏高。

单从「拷贝算子的平均耗时」来看，` + who + ` 确实存在内存拷贝耗时长的问题，如需确定是否真实存在 IO 劣化，需进一步排查。`
}

// sortedResultKeys orders map keys by value descending, then key ascending, so
// rendered output is deterministic across runs.
func sortedResultKeys(data map[string]float64) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if data[keys[i]] != data[keys[j]] {
			return data[keys[i]] > data[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// mergeCategory counts the union of two categories (used for the compute group).
func mergeCategory(result config.DegradationData, cats ...string) map[string]float64 {
	out := make(map[string]float64)
	for _, c := range cats {
		for k, v := range result[c] {
			if prev, ok := out[k]; !ok || v > prev {
				out[k] = v
			}
		}
	}
	return out
}

// parseRankList parses a comma-separated rank key into ints.
func parseRankList(key string) []int {
	if key == "" {
		return nil
	}
	parts := strings.Split(key, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		r, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil
		}
		out = append(out, r)
	}
	return out
}

// joinRanks sorts and comma-joins ranks.
func joinRanks(ranks []int) string {
	cp := append([]int(nil), ranks...)
	sort.Ints(cp)
	parts := make([]string, len(cp))
	for i, r := range cp {
		parts[i] = strconv.Itoa(r)
	}
	return strings.Join(parts, ",")
}

// sortedDomains returns the parallel domain names in sorted order.
func sortedDomains(parallels map[string][][]int) []string {
	out := make([]string, 0, len(parallels))
	for d := range parallels {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
