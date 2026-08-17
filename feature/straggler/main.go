// Command slowNodeDetection is the straggler (slow-node) detection tool for
// AI training clusters. It reads Ascend PyTorch Profiler Level0 data (one
// SQLite .db file per NPU device), detects performance-degraded devices
// across four dimensions (compute, communication, CPU, NPU bubble), and
// outputs results as JSON and a human-readable text report.
//
// Optionally, a KPI resource CSV can be provided for lightweight NPU resource
// anomaly detection before the heavy Profiler analysis.
//
// Usage:
//
//	go run . path=/data/dir [degradation=0.3] [--kpi-path=/dir/of/kpi_csvs]
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/dataparse"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/detector"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/report"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/resource"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/utils"
)

// combinedOutput is the single output JSON: the KPI resource result and the
// Profiler result under two keys. Either section is omitted when that dimension
// did not run (e.g. KPI-only → only "kpi"; Profiler-only → only "profiler").
type combinedOutput struct {
	KPI      *resource.DetectionResult `json:"kpi,omitempty"`
	Profiler *utils.NodeOutput         `json:"profiler,omitempty"`
}

// writeCombinedJSON marshals the combined KPI+Profiler result into one JSON
// file at path (the current working directory).
func writeCombinedJSON(kpi *resource.DetectionResult, profiler *utils.NodeOutput, path string) error {
	out := combinedOutput{KPI: kpi, Profiler: profiler}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal combined output: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write combined output: %w", err)
	}
	return nil
}

func main() {
	// 1. Parse CLI arguments.
	var inputPath string
	var kpiPath string
	var kpiJSONLDir string
	var faultsubURL string
	degradation := 0.3
	spaceRatioThreshold := 0.0 // 0 = use the default SpaceRatioThreshold (2.0)
	debugOutput := false       // --debug-output: include all normal+abnormal data (kpi.debug / profiler.debug) in straggler_output.json

	for _, arg := range os.Args[1:] {
		// Bare boolean flag (no "=value").
		if arg == "--debug-output" {
			debugOutput = true
			continue
		}
		parts := strings.SplitN(arg, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "--debug-output":
			debugOutput = val == "true" || val == "1"
		case "path":
			inputPath = val
		case "degradation":
			if parsed, err := strconv.ParseFloat(val, 64); err == nil {
				if parsed < 0 {
					fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] WARNING: degradation < 0, using default 0.3\n")
				} else {
					if parsed > 1 {
						fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] WARNING: degradation > 1 may produce unexpected results\n")
					}
					degradation = parsed
				}
			} else {
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] WARNING: invalid degradation value, using default 0.3\n")
			}
		case "--kpi-path":
			kpiPath = val
		case "--kpi-jsonl-dir":
			kpiJSONLDir = val
		case "--faultsub-url":
			faultsubURL = val
		case "--space-ratio-threshold":
			if parsed, err := strconv.ParseFloat(val, 64); err == nil && parsed > 0 {
				spaceRatioThreshold = parsed
			} else {
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] WARNING: invalid --space-ratio-threshold value, using default\n")
			}
		}
	}

	// ─────────────────────────────────────────────────────────────────
	// First line of defense: KPI resource anomaly detection (lightweight)
	// ─────────────────────────────────────────────────────────────────
	// KPI input: --kpi-jsonl-dir (CATMonitor straggler_output JSONL) takes
	// precedence over --kpi-path (legacy kpi_collect.sh CSV directory). Either is optional.
	kpiInput := kpiPath
	if kpiJSONLDir != "" {
		kpiInput = kpiJSONLDir
	}

	// No input at all → usage error before anything runs.
	if inputPath == "" && kpiInput == "" {
		fmt.Fprintf(os.Stderr, "Usage: slowNodeDetection path=/your/data/dir [degradation=0.3] [--kpi-path=/dir/of/kpi_csvs | --kpi-jsonl-dir=/dir] [--faultsub-url=http://host:9101] [--space-ratio-threshold=2.0]\n")
		fmt.Fprintf(os.Stderr, "ERROR: Missing required parameter: path=/your/data/dir (or a KPI input)\n")
		os.Exit(1)
	}

	var kpiResult *resource.DetectionResult
	var profilerOut *utils.NodeOutput

	if kpiInput != "" {
		kpiCfg := resource.DefaultDetectionConfig()
		kpiCfg.EnableDebug = debugOutput // --debug-output: kpi result includes all cards × metrics
		if spaceRatioThreshold > 0 {
			// Space ratio threshold is an independent knob; only override
			// the default (2.0) when --space-ratio-threshold is provided.
			kpiCfg.SpaceRatioThreshold = spaceRatioThreshold
		}

		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] === KPI Resource Detection ===\n")
		var err error
		if kpiJSONLDir != "" {
			// Read all CATMonitor straggler_kpi_{date}.jsonl files in the directory.
			ts, rerr := resource.ReadKPIFiles(kpiJSONLDir)
			if rerr != nil {
				err = rerr
			} else {
				kpiResult, err = resource.RunDetectionFromData(ts, kpiJSONLDir, kpiCfg)
			}
		} else {
			// --kpi-path is a directory of per-node CSV files + node_config.json.
			kpiResult, err = resource.RunDetectionFromDir(kpiPath, kpiCfg)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI detection failed: %v\n", err)
			if inputPath != "" {
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Falling through to Profiler detection...\n")
			}
		} else {
			// KPI text report (stdout only; no file is written).
			fmt.Print(resource.WriteReport(kpiResult))

			// Emit anomalous cards back to CATMonitor faultsub (closed loop).
			if faultsubURL != "" {
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Emitting straggler_detected events to faultsub: %s\n", faultsubURL)
				resource.EmitToFaultSub(kpiResult, resource.EmitConfig{URL: faultsubURL})
			}

			// Cross-validation decision messages (the combined JSON is written at
			// the end of main, after the Profiler step, when this is the only
			// KPI result it still gets emitted under the "kpi" key).
			switch {
			case resource.HasAnomaly(kpiResult) && inputPath == "":
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI detection found anomalies. Done.\n")
			case resource.HasAnomaly(kpiResult):
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI found anomalies, proceeding to Profiler for cross-validation...\n")
			case inputPath != "":
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI found no anomalies, falling back to Profiler...\n")
			}
		}
	}

	// ─────────────────────────────────────────────────────────────────
	// Second line of defense: Profiler slow-node detection (deep analysis)
	// ─────────────────────────────────────────────────────────────────
	if inputPath != "" {
		// Validate required path.
		if info, err := os.Stat(inputPath); err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "ERROR: Invalid directory: %s (err: %v)\n", inputPath, err)
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Input path: %s\n", inputPath)
		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Degradation: %.2f\n", degradation)

		// 2. Initialize global configuration.
		config.FilePath = inputPath
		config.CalThreshold = 1 + degradation
		config.CommThreshold = 1 + degradation*5

		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] CalThreshold: %.2f, CommThreshold: %.2f\n",
			config.CalThreshold, config.CommThreshold)

		// 3. Data parsing: SQLite → CSV + JSON intermediates.
		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Starting data parsing...\n")
		dataparse.DataParsing(inputPath)

		// 4. Get parallel topology from group_info JSON files.
		parallels, validRanks := detector.GetCurDetectionInfo(inputPath)
		if len(validRanks) == 0 {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] FATAL: Failed to get valid ranks\n")
			os.Exit(1)
		}
		if len(parallels) == 0 {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] WARNING: no parallel topology (group names not registered), degrading to cal-only detection\n")
		}
		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Valid ranks: %d, Parallel domains: %d\n",
			len(validRanks), len(parallels))

		// 5. Get single-snapshot step data from CSV files.
		stepData := detector.GetCurJobLastStepData(validRanks)
		if len(stepData) == 0 {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] FATAL: No valid step data\n")
			os.Exit(1)
		}

		// 6. Run detection pipeline.
		result := detector.DelimitDetection(stepData, parallels, validRanks)
		if result == nil {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] FATAL: Detection returned no results\n")
			os.Exit(1)
		}

		// 7. Build node-aggregated result (stdout summary; the JSON goes into the
		//    combined output written at the end of main). With --debug-output, all
		//    nodes (even normal) are included with their diagnostic scores.
		var buildErr error
		if debugOutput {
			debug := &utils.DebugInfo{
				ValidRanks: validRanks,
				RankScores: detector.DebugRankScores(stepData, validRanks),
				CommScores: detector.DebugCommScores(stepData, parallels),
			}
			profilerOut, buildErr = utils.BuildNodeResult(result, parallels, debug)
		} else {
			profilerOut, buildErr = utils.BuildNodeResult(result, parallels, nil)
		}
		if buildErr != nil {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Failed to build node result: %v\n", buildErr)
		}

		// 8. Generate text report.
		report.WriteReport(stepData, parallels, validRanks, inputPath, result, inputPath, degradation)
	}

	// ─────────────────────────────────────────────────────────────────
	// Combined JSON output: one file in the running directory holding both the
	// KPI and Profiler results under the "kpi"/"profiler" keys. A section is
	// absent when that dimension did not run (e.g. KPI-only → only "kpi").
	// ─────────────────────────────────────────────────────────────────
	if kpiResult != nil || profilerOut != nil {
		const combinedPath = "straggler_output.json"
		if err := writeCombinedJSON(kpiResult, profilerOut, combinedPath); err != nil {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Failed to write combined output: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Result written to %s\n", combinedPath)
		}
	}

	if inputPath != "" {
		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Detection complete.\n")
	}
}
