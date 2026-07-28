// Command slowNodeDetection is the straggler (slow-node) detection tool. It
// lives as an independent Go module under CATHelper/feature/straggler and
// consumes CATMonitor's straggler_output KPI file (and Ascend Profiler .db
// data) to detect performance-degraded NPU cards. It does NOT import CATMonitor
// packages — it consumes its outputs externally.
//
// Build:
//
//	CGO_ENABLED=0 go build -o slowNodeDetection .
module github.com/Computing-Availability-Tools/CATHelper/feature/straggler

go 1.23.4

require modernc.org/sqlite v1.34.5

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.22.0 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
)
