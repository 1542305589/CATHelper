package center

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// fakeControlDaemon records the control requests the center sends to a daemon.
type fakeControlDaemon struct {
	mu          sync.Mutex
	lastIter    int
	lastKey     string
	matchBody   map[string]any
	triggerBody map[string]any
	triggerKey  string
	srv         *httptest.Server
}

func newFakeControlDaemon(t *testing.T) *fakeControlDaemon {
	t.Helper()
	f := &fakeControlDaemon{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastKey = r.Header.Get("X-Match-Key")
		switch r.URL.Path {
		case "/daemon/iterations":
			var req struct {
				Iterations int `json:"iterations"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.lastIter = req.Iterations
			writeJSON(w, map[string]int{"iterations": req.Iterations})
		case "/daemon/match":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.matchBody = body
			writeJSON(w, map[string]any{"status": "matched", "collect_wait": 60, "report_eta_ms": 156000, "round_token_supported": true})
		case "/daemon/trigger":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.triggerBody = body
			f.triggerKey = r.Header.Get("X-Match-Key")
			writeJSON(w, map[string]string{"status": "triggered"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeControlDaemon) addr(t *testing.T) (string, int) {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatalf("parse fake url: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split fake host: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func (f *fakeControlDaemon) snapshot() (int, string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIter, f.lastKey, f.matchBody
}

func (f *fakeControlDaemon) triggerSnapshot() (map[string]any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.triggerBody, f.triggerKey
}

func newIterTestCenter(t *testing.T) *Center {
	t.Helper()
	c := New(Config{DataDir: t.TempDir(), Port: 1})
	t.Cleanup(func() {
		if c.kpi != nil {
			_ = c.kpi.Close()
		}
	})
	return c
}

func TestAddBusinessDefaultsIterations(t *testing.T) {
	c := newIterTestCenter(t)
	rr := serveCenter(c, http.MethodPost, "/center/business", `{"name":"b2","interval_sec":600,"kpi_interval_sec":60}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("add business status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := c.biz["b2"].Iterations; got != defaultBusinessIterations {
		t.Fatalf("new business iterations = %d, want %d", got, defaultBusinessIterations)
	}
}

// The business value is used only by center triggers; it must NOT be pushed to
// daemons (each daemon keeps its own self-run value).
func TestBusinessIterationsAPIUpdatesOnlyBusiness(t *testing.T) {
	f := newFakeControlDaemon(t)
	host, port := f.addr(t)
	c := newIterTestCenter(t)
	b := &Business{Name: "biz", Daemons: []*Daemon{{IP: host, Port: port, Key: "secret", matchState: "healthy"}}}
	c.biz["biz"] = b

	rr := serveCenter(c, http.MethodPost, "/center/business/biz/iterations", `{"iterations":5}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("set iterations status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if b.Iterations != 5 {
		t.Fatalf("business iterations = %d, want 5", b.Iterations)
	}
	got, key, _ := f.snapshot()
	if got != 0 || key != "" {
		t.Fatalf("daemon received a push (iterations=%d key=%q), want none", got, key)
	}

	rr = serveCenter(c, http.MethodPost, "/center/business/biz/iterations", `{"iterations":0}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid iterations status = %d, want 400", rr.Code)
	}
	rr = serveCenter(c, http.MethodPost, "/center/business/biz/iterations", `{"iterations":2}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("low iterations status = %d, want 400", rr.Code)
	}
	if b.Iterations != 5 {
		t.Fatalf("business iterations changed on invalid input: %d", b.Iterations)
	}
}

func TestBusinessIterationsPersist(t *testing.T) {
	dir := t.TempDir()
	c := &Center{cfg: Config{DataDir: dir}, biz: map[string]*Business{}}
	c.mu.Lock()
	c.biz["biz"] = &Business{Name: "biz", Iterations: 7}
	c.save()
	c.mu.Unlock()

	c2 := &Center{cfg: Config{DataDir: dir}, biz: map[string]*Business{}}
	c2.load()
	if got := c2.biz["biz"].Iterations; got != 7 {
		t.Fatalf("reloaded iterations = %d, want 7", got)
	}
}

func TestLoadDefaultsBusinessIterations(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "businesses.json"), []byte(`{"businesses":[{"name":"biz"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Center{cfg: Config{DataDir: dir}, biz: map[string]*Business{}}
	c.load()
	if got := c.biz["biz"].Iterations; got != defaultBusinessIterations {
		t.Fatalf("default iterations = %d, want %d", got, defaultBusinessIterations)
	}
}

// The match request must not carry iterations: matching does not overwrite the
// daemon's saved self-run value.
func TestMatchRequestDoesNotPushIterations(t *testing.T) {
	f := newFakeControlDaemon(t)
	host, port := f.addr(t)
	c := newIterTestCenter(t)
	b := &Business{Name: "biz", Iterations: 4}
	d := &Daemon{IP: host, Port: port}
	if err := c.matchDaemonLocked(b, d); err != nil {
		t.Fatalf("matchDaemonLocked: %v", err)
	}
	_, _, body := f.snapshot()
	if body == nil {
		t.Fatal("daemon did not receive /daemon/match")
	}
	if _, ok := body["iterations"]; ok {
		t.Fatalf("match body must not push iterations: %v", body["iterations"])
	}
}

// Auto-matching (not only the manual matchDaemonLocked path) must learn the
// daemon's round-token capability.
func TestTryMatchRestoresTokenCapability(t *testing.T) {
	f := newFakeControlDaemon(t)
	host, port := f.addr(t)
	c := newIterTestCenter(t)
	d := &Daemon{IP: host, Port: port}
	b := &Business{Name: "biz", Daemons: []*Daemon{d}}
	c.biz["biz"] = b

	c.tryMatch(b, d)
	if !d.tokenCapable {
		t.Fatalf("auto-matched daemon tokenCapable = false, want true")
	}
	if d.reportEtaMs != 156000 {
		t.Fatalf("auto-matched daemon reportEtaMs = %d, want 156000", d.reportEtaMs)
	}
	if d.matchState != "healthy" {
		t.Fatalf("matchState = %q, want healthy", d.matchState)
	}
}

// Every center trigger carries the current business iteration count and the
// round token, authenticated with the match key.
func TestTriggerDaemonCarriesIterations(t *testing.T) {
	f := newFakeControlDaemon(t)
	host, port := f.addr(t)
	c := newIterTestCenter(t)
	d := &Daemon{IP: host, Port: port, Key: "secret"}
	c.triggerDaemon(d, "round-1", 5)

	body, key := f.triggerSnapshot()
	if body == nil {
		t.Fatal("daemon did not receive /daemon/trigger")
	}
	if got, _ := body["iterations"].(float64); int(got) != 5 {
		t.Fatalf("trigger iterations = %v, want 5", body["iterations"])
	}
	if got, _ := body["round_token"].(string); got != "round-1" {
		t.Fatalf("trigger round_token = %q, want round-1", body["round_token"])
	}
	if key != "secret" {
		t.Fatalf("trigger key = %q, want secret", key)
	}
}
