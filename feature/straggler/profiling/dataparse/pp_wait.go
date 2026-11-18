package dataparse

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// PPWaitGapColumn is the CSV column holding the per-rank PP wait gap.
const PPWaitGapColumn = "PP_WaitGap"

// ppOp is one communication operator inside the detection window.
type ppOp struct {
	name  string
	start int
	end   int
}

// isBatchSendRecv reports whether the op name is a BatchSendRecv.
func isBatchSendRecv(name string) bool {
	s := strings.ToLower(strings.ReplaceAll(stripVendorPrefix(name), "_", ""))
	return strings.Contains(s, "batchsendrecv")
}

// isPPSend reports whether the op is a Send.
func isPPSend(name string) bool {
	s := strings.ToLower(leadingLetters(stripVendorPrefix(name)))
	return strings.HasPrefix(s, "send")
}

// isPPRecv reports whether the op is a Receive.
func isPPRecv(name string) bool {
	s := strings.ToLower(leadingLetters(stripVendorPrefix(name)))
	return strings.HasPrefix(s, "recv") || strings.HasPrefix(s, "receive")
}

// p2pDir classifies a point-to-point op: "both", "send", "recv", or "" (not P2P).
func p2pDir(name string) string {
	switch {
	case isBatchSendRecv(name):
		return "both"
	case isPPSend(name):
		return "send"
	case isPPRecv(name):
		return "recv"
	}
	return ""
}

// computePPWaitValue sums, for every anchor P2P op, the gap between its end and
// the end of the NEXT non-P2P (collective) op. In batch mode only BatchSendRecv
// anchors count; otherwise Send/Recv do. Returns (value, found).
func computePPWaitValue(ops []ppOp, batchMode bool) (int, bool) {
	total := 0
	found := false
	for i, o := range ops {
		d := p2pDir(o.name)
		anchor := d == "both"
		if !batchMode {
			anchor = d == "send" || d == "recv"
		}
		if !anchor {
			continue
		}
		next := -1
		for j := i + 1; j < len(ops); j++ {
			if p2pDir(ops[j].name) == "" {
				next = j
				break
			}
		}
		if next < 0 {
			continue
		}
		if gap := ops[next].end - o.end; gap > 0 {
			total += gap
			found = true
		}
	}
	return total, found
}

// collectPPGroups returns the distinct PP rank groups (group_name == "pp"),
// sorted by (min rank, ranks).
func collectPPGroups(rankToInfo map[int]rankDB) [][]int {
	seen := make(map[string]bool)
	var groups [][]int
	for _, ri := range rankToInfo {
		for _, val := range ri.pgi {
			m, ok := val.(map[string]interface{})
			if !ok {
				continue
			}
			if name, _ := m["group_name"].(string); name != "pp" {
				continue
			}
			var grp []int
			if raw, ok := m["global_ranks"].([]interface{}); ok {
				for _, x := range raw {
					switch n := x.(type) {
					case float64:
						grp = append(grp, int(n))
					case int:
						grp = append(grp, n)
					}
				}
			}
			if len(grp) < 2 {
				continue
			}
			sort.Ints(grp)
			key := joinRankKey(grp)
			if seen[key] {
				continue
			}
			seen[key] = true
			groups = append(groups, grp)
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i][0] != groups[j][0] {
			return groups[i][0] < groups[j][0]
		}
		return joinRankKey(groups[i]) < joinRankKey(groups[j])
	})
	return groups
}

// loadAllCommOps loads every collective/P2P op in the step window, resolved to
// its STRING_IDS name and sorted by startNs.
func loadAllCommOps(db *sql.DB, step StepTime) []ppOp {
	if exists, _ := tableExists(db, "COMMUNICATION_OP"); !exists {
		return nil
	}
	rows, err := db.Query(`
		SELECT opName, startNs, endNs FROM COMMUNICATION_OP
		WHERE startNs >= ? AND endNs <= ?
		ORDER BY startNs ASC
	`, step.StartNs, step.EndNs)
	if err != nil {
		return nil
	}
	defer rows.Close()

	type rawOp struct {
		nameID int
		start  int
		end    int
	}
	var raws []rawOp
	var ids []int
	for rows.Next() {
		var r rawOp
		if err := rows.Scan(&r.nameID, &r.start, &r.end); err != nil {
			continue
		}
		raws = append(raws, r)
		ids = append(ids, r.nameID)
	}
	names := stringMapByIDs(db, ids)
	out := make([]ppOp, 0, len(raws))
	for _, r := range raws {
		out = append(out, ppOp{name: names[r.nameID], start: r.start, end: r.end})
	}
	return out
}

// BackfillPPWait computes the per-rank PP_WaitGap column and stores the PP
// session length in config.PPSessionNs. Mirrors Python's backfill_pp_wait.
func BackfillPPWait(inputPath string) error {
	dbPaths := discoverDBFiles(inputPath)
	if len(dbPaths) == 0 {
		return nil
	}
	rankToInfo := make(map[int]rankDB)
	for _, p := range dbPaths {
		rankStr, err := extractGlobalRankFromFilename(p)
		if err != nil {
			continue
		}
		r, err := strconv.Atoi(rankStr)
		if err != nil {
			continue
		}
		db, err := sql.Open("sqlite", p+"?mode=ro")
		if err != nil {
			continue
		}
		pgi, _, err := readGroupInfo(db, rankStr, inputPath)
		db.Close()
		if err != nil {
			pgi = nil
		}
		rankToInfo[r] = rankDB{rankStr: rankStr, path: p, pgi: pgi}
	}
	if len(rankToInfo) == 0 {
		return nil
	}

	ppGroups := collectPPGroups(rankToInfo)
	if len(ppGroups) == 0 {
		return nil
	}
	rankSet := make(map[int]bool)
	for _, g := range ppGroups {
		for _, r := range g {
			rankSet[r] = true
		}
	}
	allRanks := make([]int, 0, len(rankSet))
	for r := range rankSet {
		allRanks = append(allRanks, r)
	}
	sort.Ints(allRanks)

	loaded := make(map[int][]ppOp, len(allRanks))
	var sessionNs int64
	for _, r := range allRanks {
		ri, ok := rankToInfo[r]
		if !ok {
			continue
		}
		db, err := sql.Open("sqlite", ri.path+"?mode=ro")
		if err != nil {
			continue
		}
		step := mergedStep(db)
		loaded[r] = loadAllCommOps(db, step)
		db.Close()
		if d := int64(step.EndNs - step.StartNs); d > 0 && d < (1<<62) && d > sessionNs {
			sessionNs = d
		}
	}

	batchMode := false
	for _, ops := range loaded {
		for _, o := range ops {
			if p2pDir(o.name) == "both" {
				batchMode = true
			}
		}
	}
	config.PPSessionNs = sessionNs

	for _, r := range allRanks {
		val, ok := computePPWaitValue(loaded[r], batchMode)
		if !ok {
			continue
		}
		path := filepath.Join(inputPath, "op_metric", "global_rank_"+strconv.Itoa(r)+".csv")
		cols := map[string]string{PPWaitGapColumn: strconv.FormatFloat(float64(val), 'f', -1, 64)}
		if err := backfillBandwidthCSV(path, cols); err != nil {
			fmt.Fprintf(os.Stderr, "[SLOW-DOMAIN] pp wait backfill rank %d: %v\n", r, err)
		}
	}
	return nil
}
