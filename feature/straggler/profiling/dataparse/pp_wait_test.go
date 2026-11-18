package dataparse

import "testing"

// P2P direction classification mirrors Python: BatchSendRecv is "both",
// Send* is "send", Recv*/Receive* is "recv", everything else is not P2P.
func TestP2PDir(t *testing.T) {
	cases := map[string]string{
		"hcom_batchSendRecv_1_2": "both",
		"hcom_send_1_2":          "send",
		"hcom_recv_1_2":          "recv",
		"hcom_receive_1_2":       "recv",
		"hcom_allReduce_1_2":     "",
	}
	for name, want := range cases {
		if got := p2pDir(name); got != want {
			t.Errorf("p2pDir(%q) = %q, want %q", name, got, want)
		}
	}
}

// PP wait = sum over anchors of (next collective's end - anchor's end).
func TestComputePPWaitValue(t *testing.T) {
	ops := []ppOp{
		{name: "hcom_send_1_2", start: 100, end: 200},
		{name: "hcom_allReduce_1_2", start: 300, end: 400},
	}
	v, ok := computePPWaitValue(ops, false)
	if !ok || v != 200 {
		t.Fatalf("computePPWaitValue = (%d, %v), want (200, true)", v, ok)
	}

	// In batch mode only BatchSendRecv anchors; plain Send is ignored.
	batchOps := []ppOp{
		{name: "hcom_send_1_2", start: 100, end: 200},
		{name: "hcom_allReduce_1_2", start: 300, end: 400},
	}
	if _, ok := computePPWaitValue(batchOps, true); ok {
		t.Fatalf("batch mode anchored a plain Send, want no value")
	}
	batchOps2 := []ppOp{
		{name: "hcom_batchSendRecv_1_2", start: 100, end: 200},
		{name: "hcom_allReduce_1_2", start: 300, end: 400},
	}
	if v, ok := computePPWaitValue(batchOps2, true); !ok || v != 200 {
		t.Fatalf("batch mode = (%d, %v), want (200, true)", v, ok)
	}

	// No following collective → no value.
	if _, ok := computePPWaitValue([]ppOp{{name: "hcom_send_1_2", start: 1, end: 2}}, false); ok {
		t.Fatalf("anchor without a following collective should yield no value")
	}
}
