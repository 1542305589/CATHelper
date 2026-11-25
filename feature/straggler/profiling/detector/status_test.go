package detector

import (
	"testing"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// CategoryStatus must never present a category with no usable data as "normal":
// all-zero input is no_data, a partially-populated group is partial, and only a
// fully-populated group is ok.
func TestCategoryStatusCalNoDataPartialOK(t *testing.T) {
	parallels := map[string][][]int{"tp": {{0, 1}}}
	valid := []int{0, 1}

	if got := CategoryStatus(nil, parallels, valid)["cal"]; got != config.CategoryNoData {
		t.Fatalf("empty stepData cal status = %q, want no_data", got)
	}
	zero := map[string]map[int]float64{zpKernelColumn: {0: 0, 1: 0}}
	if got := CategoryStatus(zero, parallels, valid)["cal"]; got != config.CategoryNoData {
		t.Fatalf("all-zero kernel cal status = %q, want no_data", got)
	}
	partial := map[string]map[int]float64{zpKernelColumn: {0: 10, 1: 0}}
	if got := CategoryStatus(partial, parallels, valid)["cal"]; got != config.CategoryPartial {
		t.Fatalf("partial kernel cal status = %q, want partial", got)
	}
	ok := map[string]map[int]float64{
		zpKernelColumn:   {0: 10, 1: 11},
		zpHostDataColumn: {0: 1, 1: 1},
	}
	status := CategoryStatus(ok, parallels, valid)
	if status["cal"] != config.CategoryOK {
		t.Fatalf("full kernel cal status = %q, want ok", status["cal"])
	}
	if status["cpu"] != config.CategoryOK {
		t.Fatalf("full host cpu status = %q, want ok", status["cpu"])
	}
	if AllNoData(status) {
		t.Fatalf("AllNoData = true for cal/cpu-ok status %v", status)
	}
}

func TestAllNoData(t *testing.T) {
	if !AllNoData(nil) {
		t.Fatalf("AllNoData(nil) = false, want true")
	}
	if !AllNoData(map[string]string{"cal": config.CategoryNoData, "comm": config.CategoryNoData}) {
		t.Fatalf("AllNoData(all no_data) = false, want true")
	}
	if AllNoData(map[string]string{"cal": config.CategoryOK, "comm": config.CategoryNoData}) {
		t.Fatalf("AllNoData(one ok) = true, want false")
	}
}
