package center

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBusinessProgressEndpoint(t *testing.T) {
	c := New(Config{DataDir: t.TempDir(), Port: 1})
	b := &Business{Name: "biz", Degradation: 0.3, progress: newProgressLog()}
	b.progress.begin(1, time.Now())
	b.progress.step("hello %d", 42)
	c.mu.Lock()
	c.biz["biz"] = b
	c.mu.Unlock()

	srv := httptest.NewServer(c.httpServer().Handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/center/business/biz/progress")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var snap progressSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Cycle != 1 || !snap.InFlight || len(snap.Lines) != 1 || snap.Lines[0].Text != "hello 42" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	// Unknown business → 404.
	bad, err := http.Get(srv.URL + "/center/business/nope/progress")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown business status = %d, want 404", bad.StatusCode)
	}
}
