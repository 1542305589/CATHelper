package center

import (
	_ "embed"
	"net/http"
)

// consoleHTML is the embedded single-file center console. It drives the REST
// endpoints below via fetch: business list/CRUD, daemon health/match state, and
// per-business merged detection results.
//
//go:embed console.html
var consoleHTML []byte

// handleConsole serves the center console at the root path.
func (c *Center) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(consoleHTML)
}