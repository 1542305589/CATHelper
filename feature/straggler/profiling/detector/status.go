package detector

import "github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"

// CategoryStatus reports, for each profiling detection category, whether the
// input data was complete enough to judge:
//   - config.CategoryOK:      every expected rank/group had usable data
//   - config.CategoryPartial: some expected ranks/groups had no usable data
//   - config.CategoryNoData:  nothing usable at all (must NOT be shown as "正常")
//
// It is a pure function of the same single-step snapshot + topology the
// detector consumes, so callers can annotate reports/summaries without changing
// the detection result itself.
func CategoryStatus(stepData map[string]map[int]float64, parallels map[string][][]int, validRanks []int) map[string]string {
	status := map[string]string{
		"cal":  config.CategoryNoData,
		"comm": config.CategoryNoData,
		"cpu":  config.CategoryNoData,
	}
	if len(stepData) == 0 || len(validRanks) == 0 {
		return status
	}

	// cal: every rank in the primary detection group must have ZP_Kernel > 0.
	if _, groups := GetCalDetectionGroup(parallels, validRanks); len(groups) > 0 {
		kernel := stepData[zpKernelColumn]
		expected, usable := 0, 0
		for _, group := range groups {
			for _, rank := range group {
				expected++
				if v, ok := kernel[rank]; ok && v > 0 {
					usable++
				}
			}
		}
		status["cal"] = statusFor(expected, usable)
	}

	// cpu: every valid rank should carry a ZP_Host value.
	if host := stepData[zpHostDataColumn]; host != nil {
		usable := 0
		for _, rank := range validRanks {
			if v, ok := host[rank]; ok && v > 0 {
				usable++
			}
		}
		status["cpu"] = statusFor(len(validRanks), usable)
	}

	// comm: each non-pp/embd domain needs at least 2 groups, and every group
	// must have at least one bandwidth entry to take part in the comparison.
	expectedGroups, usableGroups := 0, 0
	for domain, groups := range parallels {
		if domain == ppParallelDomainName || domain == "embd" || len(groups) < 2 {
			continue
		}
		for _, group := range groups {
			expectedGroups++
			if len(bwSetForGroup(domain, group, stepData)) > 0 {
				usableGroups++
			}
		}
	}
	status["comm"] = statusFor(expectedGroups, usableGroups)

	return status
}

func statusFor(expected, usable int) string {
	switch {
	case expected == 0 || usable == 0:
		return config.CategoryNoData
	case usable < expected:
		return config.CategoryPartial
	default:
		return config.CategoryOK
	}
}

// AllNoData reports whether every category in status is CategoryNoData.
func AllNoData(status map[string]string) bool {
	if len(status) == 0 {
		return true
	}
	for _, s := range status {
		if s != config.CategoryNoData {
			return false
		}
	}
	return true
}
