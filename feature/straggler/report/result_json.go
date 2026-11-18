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
// degradation descending.
func WriteDetectionResult(dir string, result config.DegradationData, parallels map[string][][]int, thresholds config.Thresholds) error {
	raw, err := BuildDetectionResultJSON(result, parallels, thresholds)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, DetectionResultFile), raw, 0o644)
}

// BuildDetectionResultJSON marshals the Python flat-schema result document.
func BuildDetectionResultJSON(result config.DegradationData, parallels map[string][][]int, thresholds config.Thresholds) ([]byte, error) {
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
		sort.Slice(items, func(i, j int) bool { return items[i].MetricValue > items[j].MetricValue })
		out[cat.label] = items
	}
	return json.MarshalIndent(out, "", "  ")
}

// detectionDisplayKey maps an internal result key to the Python display key:
// comm groups become "tp[0,1]", pp_comm groups become "pp[0,1]"; other
// categories use the key as-is (rank or node name).
func detectionDisplayKey(category, key string, parallels map[string][][]int) string {
	switch category {
	case "comm":
		ranks := parseRankList(key)
		if len(ranks) == 0 {
			return key
		}
		target := joinRanks(ranks)
		for _, domain := range sortedDomains(parallels) {
			for _, g := range parallels[domain] {
				if joinRanks(g) == target {
					return domain + "[" + target + "]"
				}
			}
		}
		return "[" + target + "]"
	case "pp_comm":
		return "pp[" + key + "]"
	}
	return key
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
		sort.Slice(keys, func(i, j int) bool { return data[keys[i]] > data[keys[j]] })
		pointers := make([]string, 0, len(keys))
		degs := make([]string, 0, len(keys))
		for _, k := range keys {
			pointers = append(pointers, detectionDisplayKey(cat.label, k, parallels))
			degs = append(degs, fmt.Sprintf("%s:%.2f", detectionDisplayKey(cat.label, k, parallels), data[k]))
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
		return fmt.Sprintf("%.2fx", th.Cal)
	case "cpu", "memcpy_async":
		return fmt.Sprintf("%.2fx", th.CPU)
	case "comm":
		return fmt.Sprintf("%.2fx", th.CommThreshold)
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
func RenderConclusion(result config.DegradationData, thresholds config.Thresholds) string {
	total := 0
	for _, cat := range categoryOrder {
		total += len(result[cat.key])
	}
	if total == 0 {
		return "未检出异常。\n"
	}
	var b strings.Builder
	b.WriteString("检测结论：\n")
	writeLine := func(title string, key string) {
		data := result[key]
		if len(data) == 0 {
			return
		}
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return data[keys[i]] > data[keys[j]] })
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s(%.2fx)", k, data[k]))
		}
		b.WriteString(fmt.Sprintf("- %s：%d 项 —— %s\n", title, len(keys), strings.Join(parts, "，")))
	}
	// Compute group: KERNEL_AICORE + kernel_aivec.
	compute := mergeCategory(result, "cal", "kernel_aivec")
	if len(compute) > 0 {
		b.WriteString(fmt.Sprintf("- 慢计算（KERNEL_AICORE / kernel_aivec）：%d 项\n", len(compute)))
	}
	writeLine("慢通信 comm", "comm")
	writeLine("PP 通信 pp_comm", "pp_comm")
	writeLine("慢CPU 节点", "cpu")
	writeLine("搬运 memcpy_async", "memcpy_async")
	b.WriteString(fmt.Sprintf("阈值：计算 %.2fx / IO %.2fx / 通信 %.2fx / PP >5%%\n",
		thresholds.Cal, thresholds.CPU, thresholds.CommThreshold))
	return b.String()
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
