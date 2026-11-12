package daemon

import (
	_ "embed"
	"net/http"
)

// consoleHTML is the embedded single-file web console. It talks to the existing
// REST endpoints (/status, /straggler/*, /daemon/*) via fetch, so the REST API
// remains the single source of truth; the UI is purely a browser frontend.
//
//go:embed console.html
var consoleHTML []byte

// handleConsole serves the web console at the root path.
func (d *Daemon) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(consoleHTML)
}
