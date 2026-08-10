// Package utils provides result-writing and utility functions for the
// straggler detection system.
package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// ---------------------------------------------------------------------------
// Node-based output types
// ---------------------------------------------------------------------------

// ScoreResult is a single anomaly score for one detection aspect.
type ScoreResult struct {
	Score float64 `json:"score"`
}

// NpuResult aggregates per-NPU anomalies (cal, npu_bubble). Only fields with
// anomalies are populated (omitempty).
type NpuResult struct {
	ID        int          `json:"id"`
	Cal       *ScoreResult `json:"cal,omitempty"`
	NPUBubble *ScoreResult `json:"npu_bubble,omitempty"`
}

// NodeResult aggregates anomalies of one physical node. CPU is node-level;
// npu lists only the NPUs with anomalies.
type NodeResult struct {
	Hostname string      `json:"hostname"`
	Npu      []NpuResult `json:"npu"`
	CPU      *ScoreResult `json:"cpu,omitempty"`
}

// NodeOutput is the new straggler_detection_result.json structure.
type NodeOutput struct {
	NodeResult       []NodeResult                  `json:"node_result"`
	CommDomainResult map[string]map[string]float64 `json:"comm_domain_result"`
}

// ---------------------------------------------------------------------------
// WriteNodeResult — node-aggregated JSON output
// ---------------------------------------------------------------------------

// nodeAccumulator builds up one node's entries while scanning results.
type nodeAccumulator struct {
	hostname string
	npus     map[int]*NpuResult
	cpu      float64
	hasCPU   bool
}

// WriteNodeResult writes straggler_detection_result.json in the node-aggregated
// format: results are grouped by physical node (hostname from HOST_INFO.hostName,
// NPU id from NPU_INFO.id), and communication results by domain name. Only
// anomalous nodes/NPUs are included.
func WriteNodeResult(finalResult map[string]map[string]float64, parallels map[string][][]int) error {
	meta := loadRankMeta(finalResult)
	nodes := make(map[string]*nodeAccumulator)

	// cal / npu_bubble: per rank → node.npu[id].
	for _, cat := range []string{"cal", "npu_bubble"} {
		for rankStr, score := range finalResult[cat] {
			rank, err := strconv.Atoi(rankStr)
			if err != nil {
				continue
			}
			m, ok := meta[rank]
			if !ok {
				continue
			}
			acc := ensureNodeAcc(nodes, m.hostname)
			npu := ensureNpuAcc(acc, m.npuID)
			if cat == "cal" {
				npu.Cal = &ScoreResult{Score: score}
			} else {
				npu.NPUBubble = &ScoreResult{Score: score}
			}
		}
	}

	// cpu: per rank → node-level score (all ranks of a slow node share the
	// trimmed-mean value, so any of them gives the node's score).
	for rankStr, score := range finalResult["cpu"] {
		rank, err := strconv.Atoi(rankStr)
		if err != nil {
			continue
		}
		m, ok := meta[rank]
		if !ok {
			continue
		}
		acc := ensureNodeAcc(nodes, m.hostname)
		acc.cpu = score
		acc.hasCPU = true
	}

	// comm_domain_result: group slow communication groups by domain name.
	commDomains := make(map[string]map[string]float64)
	for groupKey, score := range finalResult["comm"] {
		ranks := stringToRanks(groupKey)
		domain := findDomainForRanks(ranks, parallels)
		if domain == "" {
			domain = "[" + groupKey + "]"
		}
		if commDomains[domain] == nil {
			commDomains[domain] = make(map[string]float64)
		}
		commDomains[domain][groupKey] = score
	}

	// Build node_result (sorted by hostname, npu by id for determinism).
	hostnames := sortedNodeHosts(nodes)
	nodeResults := make([]NodeResult, 0, len(hostnames))
	for _, hn := range hostnames {
		acc := nodes[hn]
		nr := NodeResult{Hostname: hn}
		if acc.hasCPU {
			nr.CPU = &ScoreResult{Score: acc.cpu}
		}
		for _, id := range sortedNpuIDs(acc.npus) {
			nr.Npu = append(nr.Npu, *acc.npus[id])
		}
		nodeResults = append(nodeResults, nr)
	}

	out := NodeOutput{NodeResult: nodeResults, CommDomainResult: commDomains}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal node result: %w", err)
	}
	outPath := filepath.Join(config.FilePath, "straggler_detection_result.json")
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return fmt.Errorf("write result file: %w", err)
	}
	printNodeSummary(nodeResults, commDomains)
	fmt.Printf("[SLOWNODE ALGO] Result written to %s\n", outPath)
	return nil
}

// ---------------------------------------------------------------------------
// Rank metadata (hostname + NPU id from op_metric intermediates)
// ---------------------------------------------------------------------------

type rankMeta struct {
	hostname string
	npuID    int
}

// loadRankMeta reads host_info_{N}.json (hostName) and npu_info_{N}.json (id)
// for every anomalous rank referenced by the result.
func loadRankMeta(finalResult map[string]map[string]float64) map[int]rankMeta {
	meta := make(map[int]rankMeta)
	seen := make(map[int]bool)
	for _, cat := range []string{"cal", "cpu", "npu_bubble"} {
		for rankStr := range finalResult[cat] {
			rank, err := strconv.Atoi(rankStr)
			if err != nil || seen[rank] {
				continue
			}
			seen[rank] = true
			meta[rank] = readRankMeta(rank)
		}
	}
	return meta
}

func readRankMeta(rank int) rankMeta {
	var m rankMeta
	metricDir := filepath.Join(config.FilePath, "op_metric")

	if raw, err := os.ReadFile(filepath.Join(metricDir, "host_info_"+strconv.Itoa(rank)+".json")); err == nil {
		var hi struct {
			HostUid  string `json:"hostUid"`
			HostName string `json:"hostName"`
		}
		if json.Unmarshal(raw, &hi) == nil {
			m.hostname = hi.HostName
			if m.hostname == "" {
				m.hostname = hi.HostUid // fallback to the physical-node id
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(metricDir, "npu_info_"+strconv.Itoa(rank)+".json")); err == nil {
		var ni struct {
			ID int `json:"id"`
		}
		if json.Unmarshal(raw, &ni) == nil {
			m.npuID = ni.ID
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// Accumulator helpers
// ---------------------------------------------------------------------------

func ensureNodeAcc(nodes map[string]*nodeAccumulator, hostname string) *nodeAccumulator {
	acc, ok := nodes[hostname]
	if !ok {
		acc = &nodeAccumulator{hostname: hostname, npus: make(map[int]*NpuResult)}
		nodes[hostname] = acc
	}
	return acc
}

func ensureNpuAcc(acc *nodeAccumulator, id int) *NpuResult {
	npu, ok := acc.npus[id]
	if !ok {
		npu = &NpuResult{ID: id}
		acc.npus[id] = npu
	}
	return npu
}

func sortedNodeHosts(nodes map[string]*nodeAccumulator) []string {
	keys := make([]string, 0, len(nodes))
	for k := range nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedNpuIDs(npus map[int]*NpuResult) []int {
	ids := make([]int, 0, len(npus))
	for id := range npus {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// ---------------------------------------------------------------------------
// Print helpers
// ---------------------------------------------------------------------------

func printNodeSummary(nodeResults []NodeResult, commDomains map[string]map[string]float64) {
	if len(nodeResults) == 0 {
		fmt.Printf("慢节点 (node): 无异常\n")
	} else {
		fmt.Printf("慢节点 (node): 发现 %d 个异常节点\n", len(nodeResults))
		for _, nr := range nodeResults {
			fmt.Printf("  %s\n", nr.Hostname)
			for _, npu := range nr.Npu {
				fmt.Printf("    NPU %d\n", npu.ID)
				if npu.Cal != nil {
					fmt.Printf("      cal        %.2f\n", npu.Cal.Score)
				}
				if npu.NPUBubble != nil {
					fmt.Printf("      npu_bubble %.2f\n", npu.NPUBubble.Score)
				}
			}
			if nr.CPU != nil {
				fmt.Printf("    cpu         %.2f\n", nr.CPU.Score)
			}
		}
	}
	if len(commDomains) == 0 {
		fmt.Printf("慢通信 (comm): 无异常\n")
	} else {
		fmt.Printf("慢通信 (comm): 发现 %d 个异常域\n", len(commDomains))
		for domain, groups := range commDomains {
			fmt.Printf("  %s\n", domain)
			for groupKey, score := range groups {
				fmt.Printf("    %s %.2f\n", groupKey, score)
			}
		}
	}
}
