package center

import "testing"

// A daemon's match key must survive a center restart so the restarted center
// can re-authenticate to an already-matched daemon (whose own key lives in the
// daemon's memory) instead of being reported as "other".
func TestDaemonKeyPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	c1 := New(Config{DataDir: dir, Port: 1})
	b := &Business{Name: "biz", Daemons: []*Daemon{{IP: "1.2.3.4", Port: 8080, Key: "secret-key"}}}
	c1.biz["biz"] = b
	c1.save()

	c2 := New(Config{DataDir: dir, Port: 1})
	got := c2.biz["biz"]
	if got == nil || len(got.Daemons) != 1 {
		t.Fatalf("business not reloaded: %+v", c2.biz)
	}
	if got.Daemons[0].Key != "secret-key" {
		t.Errorf("key = %q, want secret-key", got.Daemons[0].Key)
	}
}
