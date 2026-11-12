package center

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newKPITestCenter(t *testing.T) *Center {
	t.Helper()
	c := New(Config{DataDir: t.TempDir(), Port: 1})
	t.Cleanup(func() {
		if c.kpi != nil {
			_ = c.kpi.Close()
		}
	})
	c.biz["biz"] = &Business{
		Name: "biz",
		Daemons: []*Daemon{{
			IP:   "1.2.3.4",
			Port: 8080,
			Key:  "secret",
		}},
	}
	return c
}

func serveCenter(c *Center, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	c.httpServer().Handler.ServeHTTP(rr, req)
	return rr
}

func TestKPIIngestAndQuery(t *testing.T) {
	c := newKPITestCenter(t)
	body := `{"node":"node-a","samples":[{"ts":100,"vals":{"0":{"temp":49,"power":170},"1":{"temp":50,"power":171}}},{"ts":101,"vals":{"0":{"temp":51,"power":172},"1":{"temp":52,"power":173}}}]}`
	rr := serveCenter(c, http.MethodPost, "/center/kpi/biz/1.2.3.4:8080", body, map[string]string{"X-Match-Key": "secret"})
	if rr.Code != http.StatusOK {
		t.Fatalf("ingest status = %d, body = %s", rr.Code, rr.Body.String())
	}

	latest, err := c.kpi.Latest("biz", "", nil)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest rows = %d, want 2: %+v", len(latest), latest)
	}
	for _, row := range latest {
		if row.Ts != 101 {
			t.Errorf("latest device %d ts = %d, want 101", row.Device, row.Ts)
		}
		if row.Node != "node-a(1.2.3.4:8080)" || row.Daemon != "1.2.3.4:8080" {
			t.Errorf("latest row identity = %+v", row)
		}
	}

	history, err := c.kpi.History("biz", 100, 101, "", nil, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("history rows = %d, want 4", len(history))
	}
}

func TestKPIFilterAPI(t *testing.T) {
	c := newKPITestCenter(t)
	body := `{"node":"node-a","samples":[{"ts":100,"vals":{"0":{"temp":49},"1":{"temp":50},"2":{"temp":51}}}]}`
	if rr := serveCenter(c, http.MethodPost, "/center/kpi/biz/1.2.3.4:8080", body, map[string]string{"X-Match-Key": "secret"}); rr.Code != http.StatusOK {
		t.Fatalf("ingest status = %d, body = %s", rr.Code, rr.Body.String())
	}

	rr := serveCenter(c, http.MethodPost, "/center/business/biz/kpi/filter", `{"node":"node-a(1.2.3.4:8080)","devices":[1]}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("set filter status = %d, body = %s", rr.Code, rr.Body.String())
	}
	latest, err := c.kpi.Latest("biz", "", nil)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 1 || latest[0].Device != 1 {
		t.Fatalf("filtered latest = %+v, want only device 1", latest)
	}

	rr = serveCenter(c, http.MethodGet, "/center/business/biz/kpi/filter", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get filter status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var filters struct {
		Filters []kpiFilter `json:"filters"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &filters); err != nil {
		t.Fatal(err)
	}
	if len(filters.Filters) != 1 || filters.Filters[0].Node != "node-a(1.2.3.4:8080)" || filters.Filters[0].Devices[0] != 1 {
		t.Errorf("filters = %+v", filters.Filters)
	}
}

func TestKPIIngestAuth(t *testing.T) {
	c := newKPITestCenter(t)
	body := `{"node":"node-a","samples":[{"ts":100,"vals":{"0":{"temp":49}}}]}`
	rr := serveCenter(c, http.MethodPost, "/center/kpi/biz/1.2.3.4:8080", body, map[string]string{"X-Match-Key": "wrong"})
	if rr.Code != http.StatusForbidden {
		t.Errorf("wrong key status = %d, want 403", rr.Code)
	}
	rr = serveCenter(c, http.MethodPost, "/center/kpi/missing/1.2.3.4:8080", body, map[string]string{"X-Match-Key": "secret"})
	if rr.Code != http.StatusForbidden {
		t.Errorf("missing business status = %d, want 403", rr.Code)
	}
}

func TestKPIDevicesEndpointIgnoresFilter(t *testing.T) {
	c := newKPITestCenter(t)
	body := `{"node":"node-a","samples":[{"ts":100,"vals":{"0":{"temp":49},"1":{"temp":50},"2":{"temp":51}}}]}`
	if rr := serveCenter(c, http.MethodPost, "/center/kpi/biz/1.2.3.4:8080", body, map[string]string{"X-Match-Key": "secret"}); rr.Code != http.StatusOK {
		t.Fatalf("ingest status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr := serveCenter(c, http.MethodPost, "/center/business/biz/kpi/filter", `{"node":"node-a(1.2.3.4:8080)","devices":[1]}`, nil); rr.Code != http.StatusOK {
		t.Fatalf("set filter status = %d, body = %s", rr.Code, rr.Body.String())
	}
	rr := serveCenter(c, http.MethodGet, "/center/business/biz/kpi/devices", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("devices status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Nodes []kpiFilter `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 1 || len(resp.Nodes[0].Devices) != 3 {
		t.Fatalf("nodes = %+v, want node-a(1.2.3.4:8080) with all 3 devices", resp.Nodes)
	}
}

func TestKPINodeLabel(t *testing.T) {
	cases := []struct {
		node, daemon, want string
	}{
		{"master", "51.62.10.87:15424", "master(51.62.10.87:15424)"},
		{"", "51.62.10.87:15424", "51.62.10.87:15424"},
		{"master(51.62.10.87:15424)", "51.62.10.87:15424", "master(51.62.10.87:15424)"},
		{"node-a", "", "node-a"},
	}
	for _, tc := range cases {
		if got := kpiNodeLabel(tc.node, tc.daemon); got != tc.want {
			t.Errorf("kpiNodeLabel(%q, %q) = %q, want %q", tc.node, tc.daemon, got, tc.want)
		}
	}
}

func TestKPIDuplicateHostnameStaysDistinct(t *testing.T) {
	c := newKPITestCenter(t)
	c.biz["biz"].Daemons = append(c.biz["biz"].Daemons, &Daemon{
		IP: "5.6.7.8", Port: 9090, Key: "secret2",
	})
	body := `{"node":"master","samples":[{"ts":100,"vals":{"0":{"temp":49}}}]}`
	if rr := serveCenter(c, http.MethodPost, "/center/kpi/biz/1.2.3.4:8080", body, map[string]string{"X-Match-Key": "secret"}); rr.Code != http.StatusOK {
		t.Fatalf("daemon1 ingest status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr := serveCenter(c, http.MethodPost, "/center/kpi/biz/5.6.7.8:9090", body, map[string]string{"X-Match-Key": "secret2"}); rr.Code != http.StatusOK {
		t.Fatalf("daemon2 ingest status = %d, body = %s", rr.Code, rr.Body.String())
	}
	rows, err := c.kpi.Latest("biz", "", nil)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	nodes := map[string]bool{}
	for _, row := range rows {
		nodes[row.Node] = true
	}
	if !nodes["master(1.2.3.4:8080)"] || !nodes["master(5.6.7.8:9090)"] {
		t.Errorf("duplicate hostname nodes = %v, want daemon-annotated labels", nodes)
	}
}

func TestKPIFilterAllAPI(t *testing.T) {
	c := newKPITestCenter(t)
	body := `{"node":"node-a","samples":[{"ts":100,"vals":{"0":{"temp":49},"1":{"temp":50},"2":{"temp":51}}}]}`
	if rr := serveCenter(c, http.MethodPost, "/center/kpi/biz/1.2.3.4:8080", body, map[string]string{"X-Match-Key": "secret"}); rr.Code != http.StatusOK {
		t.Fatalf("ingest status = %d, body = %s", rr.Code, rr.Body.String())
	}
	node := "node-a(1.2.3.4:8080)"
	if rr := serveCenter(c, http.MethodPost, "/center/business/biz/kpi/filter", `{"node":"`+node+`","devices":[],"filter_all":true}`, nil); rr.Code != http.StatusOK {
		t.Fatalf("filter-all status = %d, body = %s", rr.Code, rr.Body.String())
	}
	latest, err := c.kpi.Latest("biz", "", nil)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 0 {
		t.Errorf("filter-all latest rows = %d, want 0", len(latest))
	}
	rr := serveCenter(c, http.MethodGet, "/center/business/biz/kpi/devices", "", nil)
	var devices struct {
		Nodes []kpiFilter `json:"nodes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &devices); err != nil {
		t.Fatal(err)
	}
	if len(devices.Nodes) != 1 || len(devices.Nodes[0].Devices) != 3 {
		t.Errorf("devices endpoint must ignore filter-all, got %+v", devices.Nodes)
	}
	rr = serveCenter(c, http.MethodGet, "/center/business/biz/kpi/filter", "", nil)
	var filters struct {
		Filters []kpiFilter `json:"filters"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &filters); err != nil {
		t.Fatal(err)
	}
	if len(filters.Filters) != 1 || !filters.Filters[0].FilterAll {
		t.Errorf("filters = %+v, want filter_all", filters.Filters)
	}
}

func TestKPIConsoleEndpoint(t *testing.T) {
	c := newKPITestCenter(t)
	if err := c.kpi.InsertDetectRun(kpiDetectRun{
		Business: "biz", Node: "node-a(1.2.3.4:8080)", Ts: 100, Devices: 8, Anomalies: 1, Flagged: 1,
	}); err != nil {
		t.Fatal(err)
	}
	rr := serveCenter(c, http.MethodGet, "/center/business/biz/console/kpi?limit=10&from=1&to=1000", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("console kpi status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Runs []kpiDetectRun `json:"runs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Runs) != 1 || resp.Runs[0].Devices != 8 {
		t.Fatalf("runs = %+v", resp.Runs)
	}
}

func TestBusinessKPIIntervalAPI(t *testing.T) {
	c := newKPITestCenter(t)
	rr := serveCenter(c, http.MethodPost, "/center/business/biz/kpi_interval", `{"interval_sec":5}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("set kpi interval status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if c.biz["biz"].KPIDetectIntervalSec != 5 {
		t.Errorf("business kpi interval = %d, want 5", c.biz["biz"].KPIDetectIntervalSec)
	}
	rr = serveCenter(c, http.MethodPost, "/center/business/biz/kpi_interval", `{"interval_sec":1}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid interval status = %d, want 400", rr.Code)
	}
}

func TestBusinessKPINSigmaAPI(t *testing.T) {
	c := newKPITestCenter(t)
	body := `{"power_time":5,"power_space":6,"util_time":4.5,"util_space":4.5,"freq_time":3.5,"freq_space":3.5}`
	rr := serveCenter(c, http.MethodPost, "/center/business/biz/kpi_nsigma", body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("set nsigma status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if c.biz["biz"].KPINSigma.PowerTime != 5 || c.biz["biz"].KPINSigma.FreqSpace != 3.5 {
		t.Errorf("nsigma = %+v", c.biz["biz"].KPINSigma)
	}
	rr = serveCenter(c, http.MethodPost, "/center/business/biz/kpi_nsigma", `{"power_time":21}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid nsigma status = %d, want 400", rr.Code)
	}
}
