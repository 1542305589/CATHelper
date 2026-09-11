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