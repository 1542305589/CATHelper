package center

import (
	_ "embed"
	"net/http"
)

// consoleHTML is the embedded single-file center console (business list). It
// drives the /center/businesses + /center/business/* REST endpoints via fetch.
//
//go:embed console.html
var consoleHTML []byte

// businessHTML is the embedded per-business console (daemon list + merged
// detection history/report/result/op_metric).
//
//go:embed business.html
var businessHTML []byte

// chartJS is the embedded Chart.js UMD bundle served locally (no external CDN)
// for the per-business TTFT/TPOT charts.
//
//go:embed chart.umd.min.js
var chartJS []byte

// handleChartJS serves the embedded Chart.js bundle.
func (c *Center) handleChartJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(chartJS)
}

// handleConsole serves the center console at the root path.
func (c *Center) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(consoleHTML)
}

// handleBusinessConsole serves the per-business console page.
func (c *Center) handleBusinessConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(businessHTML)
}