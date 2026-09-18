package dataparse

import (
	"math"
	"testing"
)

func TestOpKind(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"hcom_allReduce__503_96_4", "allReduce"},
		{"hcom_allGather__182_142_4", "allGather"},
		{"hcom_reduceScatter__182_140_4", "reduceScatter"},
		{"HcclAllreduce__12_3_2", "Allreduce"},
		{"HcclAllGather__9_1_2", "AllGather"},
		{"reduceScatterFallbackName", "ReduceScatter"},
		{"allGatherFallbackName", "AllGather"},
	}
	for _, c := range cases {
		if got := opKind(c.name); got != c.want {
			t.Errorf("opKind(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOpSeqB(t *testing.T) {
	if got := opSeqB("hcom_allReduce__503_96_4"); got != 96 {
		t.Errorf("opSeqB = %d, want 96", got)
	}
	if got := opSeqB("hcom_allReduce__0_0_0"); got != 0 {
		t.Errorf("opSeqB = %d, want 0", got)
	}
	if got := opSeqB("HcclAllreduce"); got != -1 {
		t.Errorf("opSeqB = %d, want -1 (no sequence marker)", got)
	}
}

func TestIsCollectiveOpKind(t *testing.T) {
	for _, k := range []string{"Send", "send", "SEND", "Recv", "recv", "RECV"} {
		if isCollectiveOpKind(k) {
			t.Errorf("%q should not be collective", k)
		}
	}
	for _, k := range []string{"allReduce", "AllGather", "ReduceScatter"} {
		if !isCollectiveOpKind(k) {
			t.Errorf("%q should be collective", k)
		}
	}
}

// TestComputeBandwidthFromOps checks alignment, shortest-duration selection,
// and (opType,count) grouping with two ranks whose clocks overlap (wall-clock
// matching). Rank 1 is strictly slower (larger durations), so the shortest
// duration must come from rank 0.
func TestComputeBandwidthFromOps(t *testing.T) {
	members := map[int][]bwOp{
		0: {
			{opType: "allReduce", count: 1000, start: 0, end: 100},
			{opType: "allReduce", count: 1000, start: 1000, end: 1150},
			{opType: "allGather", count: 2000, start: 500, end: 600},
		},
		1: {
			{opType: "allReduce", count: 1000, start: 0, end: 200},
			{opType: "allReduce", count: 1000, start: 1000, end: 1300},
			{opType: "allGather", count: 2000, start: 500, end: 900},
		},
	}
	ranks := []int{0, 1}

	res := computeBandwidthFromOps(members, ranks)
	if len(res) != 2 {
		t.Fatalf("expected 2 combos, got %d", len(res))
	}

	// (allReduce,1000): shortest durations 100 and 150 -> bw mean.
	ar := res[bucketKey{"allReduce", 1000}]
	wantAR := (bandwidthFor(1000, 100) + bandwidthFor(1000, 150)) / 2
	if math.Abs(ar-wantAR) > 1e-9 {
		t.Errorf("allReduce bandwidth = %v, want %v", ar, wantAR)
	}

	// (allGather,2000): shortest duration 100 -> bw = 2000/100 = 20.
	ag := res[bucketKey{"allGather", 2000}]
	if math.Abs(ag-20) > 1e-9 {
		t.Errorf("allGather bandwidth = %v, want 20", ag)
	}
}

// TestComputeBandwidthFromOpsSeqAlignment verifies the sequence-B fallback:
// when a rank's clock does not overlap the base rank's at all, it is matched by
// op-name sequence index instead.
func TestComputeBandwidthFromOpsSeqAlignment(t *testing.T) {
	// Rank 1's timestamps share no overlap with rank 0 (different time base).
	members := map[int][]bwOp{
		0: {
			{opType: "allReduce", seqB: 7, count: 1000, start: 1000000000, end: 1000000100},
			{opType: "allReduce", seqB: 8, count: 1000, start: 1000001000, end: 1000001200},
		},
		1: {
			{opType: "allReduce", seqB: 7, count: 1000, start: 9000000000000, end: 9000000000200},
			{opType: "allReduce", seqB: 8, count: 1000, start: 9000001000000, end: 9000001000500},
		},
	}
	ranks := []int{0, 1}

	res := computeBandwidthFromOps(members, ranks)
	if len(res) != 1 {
		t.Fatalf("expected 1 combo, got %d", len(res))
	}
	// Two aligned occurrences: shortest durations 100 (bw 10) and 200 (bw 5),
	// bandwidth is their mean = 7.5 (mirrors the skill's mean-of-bandwidths).
	got := res[bucketKey{"allReduce", 1000}]
	want := (bandwidthFor(1000, 100) + bandwidthFor(1000, 200)) / 2
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("bandwidth = %v, want %v", got, want)
	}
}

// TestBandwidthFor checks the G-elements/s formula.
func TestBandwidthFor(t *testing.T) {
	// 1000 elements / 100 ns = 10 G elements/s.
	if got := bandwidthFor(1000, 100); math.Abs(got-10) > 1e-9 {
		t.Errorf("bandwidthFor(1000,100) = %v, want 10", got)
	}
	if got := bandwidthFor(1000, 0); got != 0 {
		t.Errorf("bandwidthFor with zero duration = %v, want 0", got)
	}
}
