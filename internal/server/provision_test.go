package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/provision"
)

const (
	provGolden = "../../tools/worker/testdata/aws/catalog-golden.json"
	provPF     = "../provision/testdata/preflight.json"
)

// fakeWorkerDir is a stand-in for the worker tool: it logs its argv and
// answers from files in its directory, which a test can swap or delete.
type fakeWorkerDir struct {
	dir, bin string
}

func newFakeWorker(t *testing.T) *fakeWorkerDir {
	t.Helper()
	d := t.TempDir()
	pf, _ := filepath.Abs(provPF)
	cat, _ := filepath.Abs(provGolden)
	os.WriteFile(filepath.Join(d, "preflight.json"), mustRead(t, pf), 0o644)
	os.WriteFile(filepath.Join(d, "catalog.json"), mustRead(t, cat), 0o644)
	bin := filepath.Join(d, "comebye-worker")
	script := `#!/bin/sh
echo "$*" >> ` + d + `/argv
case "$4" in
 preflight) cat ` + d + `/preflight.json ;;
 catalog)
   if [ -f ` + d + `/catalog.fail ]; then cat ` + d + `/catalog.fail; exit 1; fi
   cat ` + d + `/catalog.json ;;
 create)
   cat > ` + d + `/spec.in
   [ -f ` + d + `/create.stderr ] && cat ` + d + `/create.stderr >&2
   [ -f ` + d + `/create.events ] && cat ` + d + `/create.events
   [ -f ` + d + `/create.sleep ] && sleep $(cat ` + d + `/create.sleep)
   exit $(cat ` + d + `/create.exit 2>/dev/null || echo 0) ;;
 destroy)
   [ -f ` + d + `/destroy.events ] && cat ` + d + `/destroy.events
   exit $(cat ` + d + `/destroy.exit 2>/dev/null || echo 0) ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeWorkerDir{dir: d, bin: bin}
}

func (f *fakeWorkerDir) set(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeWorkerDir) argv() []string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "argv"))
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " \n "))
}

func (f *fakeWorkerDir) runs(sub string) int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "argv"))
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// provPeerServer is a peer with the provisioner role (no ledger).
func provPeerServer(t *testing.T, w *fakeWorkerDir) *Server {
	t.Helper()
	cfg := Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "prov", StateDir: t.TempDir(), NoLedger: true, AgentToken: testToken}
	if w != nil {
		cfg.Provision = ProvisionConfig{Tool: &provision.Tool{Bin: w.bin, Config: "/etc/worker.conf"}}
	}
	return New(cfg, nil, nil)
}

var peerProvCalls = []struct{ name, method, path string }{
	{"preflight", "GET", "/api/provisioner/preflight"},
	{"catalog", "GET", "/api/provisioner/catalog?kind=aws&region=us-east-1"},
	{"plan", "POST", "/api/provisioner/plan"},
	{"job create", "POST", "/api/provisioner/jobs"},
	{"job", "GET", "/api/provisioner/job?id=pj_0123456789abcdef"},
	{"retry", "POST", "/api/provisioner/job/retry"},
	{"cleanup", "POST", "/api/provisioner/job/cleanup"},
}

func TestPeerProvisionOpsAnswerOnlyAHub(t *testing.T) {
	w := newFakeWorker(t)
	s := provPeerServer(t, w)
	hub, _, _ := relServer(t) // a host that keeps the ledger
	callers := []struct {
		who  string
		srv  *Server
		hdr  map[string]string
		want int
		code string
	}{
		{"the login alone (a browser, a script)", s, userHdr, 403, "hub_only"},
		{"the marker alone", s, map[string]string{fleet.HeaderHubCall: "1"}, 401, ""},
		{"agent token with the marker", s, map[string]string{HeaderAgentToken: testToken, HeaderAgent: "w1:p1", fleet.HeaderHubCall: "1"}, 403, "agent_forbidden"},
		{"agent token, no pane, with the marker", s, map[string]string{HeaderAgentToken: testToken, fleet.HeaderHubCall: "1"}, 403, "agent_forbidden"},
		{"a peer forwarding for its agent", s, map[string]string{"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1", HeaderAgent: "w1:p1", HeaderAgentHost: "other", fleet.HeaderHubCall: "1"}, 403, "agent_forbidden"},
		{"a wrong token with the login and marker", s, map[string]string{HeaderAgentToken: "nope", "Tailscale-User-Login": "me@example.com", fleet.HeaderHubCall: "1"}, 401, ""},
		{"a login outside the allowlist", s, map[string]string{"Tailscale-User-Login": "eve@example.com", fleet.HeaderHubCall: "1"}, 401, ""},
		{"a host that keeps the ledger, with the hub's marker", hub, hubCall, 403, "hub_only"},
	}
	for _, c := range callers {
		for _, call := range peerProvCalls {
			rec := do(c.srv, call.method, call.path, c.hdr, "")
			if rec.Code != c.want || (c.code != "" && !strings.Contains(rec.Body.String(), c.code)) {
				t.Errorf("%s: %s = %d %s, want %d %s", c.who, call.name, rec.Code, rec.Body, c.want, c.code)
			}
		}
	}
	if n := len(w.argv()); n != 0 {
		t.Fatalf("a refused caller ran the tool: %v", w.argv())
	}
}

func TestPeerWithoutTheRoleAnswers404ToItsHub(t *testing.T) {
	s := provPeerServer(t, nil)
	for _, call := range peerProvCalls {
		rec := do(s, call.method, call.path, hubCall, "")
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "not_provisioner") {
			t.Errorf("%s = %d %s", call.name, rec.Code, rec.Body)
		}
	}
}

func TestPeerProvisionOpsRunTheToolForAHubCall(t *testing.T) {
	w := newFakeWorker(t)
	s := provPeerServer(t, w)
	rec := do(s, "GET", "/api/provisioner/preflight", hubCall, "")
	var pf provision.Preflight
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &pf) != nil || pf.AWS.State != "ok" || pf.Region != "us-east-2" {
		t.Fatalf("preflight = %d %s", rec.Code, rec.Body)
	}
	rec = do(s, "GET", "/api/provisioner/catalog?kind=aws&region=us-east-1&score=t3.large,m5.large&refresh=1", hubCall, "")
	var c provision.Catalog
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &c) != nil || len(c.Rows) != 10 {
		t.Fatalf("catalog = %d %s", rec.Code, rec.Body)
	}
	got := strings.Join(w.argv(), " ")
	for _, want := range []string{"-config /etc/worker.conf -no-prompt preflight --json", "catalog --kind aws --region us-east-1 --json --score t3.large,m5.large --refresh"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv lacks %q: %s", want, got)
		}
	}
	for name, path := range map[string]string{
		"kind":      "/api/provisioner/catalog?kind=gcp&region=us-east-1",
		"region":    "/api/provisioner/catalog?kind=aws&region=us-east-1%20ls",
		"no region": "/api/provisioner/catalog?kind=aws",
		"score":     "/api/provisioner/catalog?kind=aws&region=us-east-1&score=t3.large%20--x",
	} {
		if rec := do(s, "GET", path, hubCall, ""); rec.Code != 400 {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	// an expired login is a clean answer, not a crash
	w.set(t, "catalog.fail", `{"v":1,"error":{"class":"creds_expired","msg":"catalog: the instance types could not be read (creds_expired)"}}`)
	rec = do(s, "GET", "/api/provisioner/catalog?kind=aws&region=us-east-1", hubCall, "")
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "creds_expired") {
		t.Errorf("expired = %d %s", rec.Code, rec.Body)
	}
}

func TestProvisionRoutesAreUserOnly(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	psrc, err := os.ReadFile("provision.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(psrc), "\n") {
		if strings.Contains(line, `HandleFunc("`) && strings.Contains(line, "/api/provision") {
			n++
			if !strings.Contains(line, ", s.auth(s.") || strings.Contains(line, "authAgent") || strings.Contains(line, "authWorker") {
				t.Errorf("a provisioning route is not user-only: %s", strings.TrimSpace(line))
			}
		}
	}
	if n != 16 {
		t.Fatalf("found %d provisioning routes, want 16: update this test with the route", n)
	}
	if strings.Contains(string(src), "/api/provision") {
		t.Error("provisioning routes belong in provision.go, where this test checks them")
	}
	// ...and a browser cannot have the peer ops forwarded.
	for _, op := range []string{"provision-preflight", "provision-catalog", "provision-plan", "provision-job-create", "provision-job", "provision-job-retry", "provision-job-cleanup"} {
		rec := httptest.NewRecorder()
		fleet.New(nil, "desk", nil, fleet.Options{}).ProxyHost(rec, httptest.NewRequest("GET", "/", nil), "kube", op)
		if rec.Code != 404 {
			t.Errorf("ProxyHost %s = %d", op, rec.Code)
		}
	}
}

// ---- the hub's side ----

type provPeer struct {
	srv  *httptest.Server
	peer *Server
	w    *fakeWorkerDir

	mu    sync.Mutex
	paths []string
	hook  func(w http.ResponseWriter, r *http.Request) bool
}

func newProvPeer(t *testing.T, wk *fakeWorkerDir) *provPeer {
	t.Helper()
	p := &provPeer{w: wk, peer: provPeerServer(t, wk)}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		if strings.HasPrefix(r.URL.Path, "/api/provisioner/") {
			p.paths = append(p.paths, r.URL.RequestURI())
		}
		hook := p.hook
		p.mu.Unlock()
		if hook != nil && hook(w, r) {
			return
		}
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		p.peer.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *provPeer) asked(sub string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, x := range p.paths {
		if strings.Contains(x, sub) {
			n++
		}
	}
	return n
}

func (p *provPeer) setHook(h func(w http.ResponseWriter, r *http.Request) bool) {
	p.mu.Lock()
	p.hook = h
	p.mu.Unlock()
}

// provHub is a hub whose default provisioner is def, with the given peers.
func provHub(t *testing.T, def string, peers map[string]*provPeer) *Server {
	t.Helper()
	store, err := fleet.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fl := fleet.New(nil, "desk", nil, fleet.Options{HubMode: true, Logf: func(string, ...any) {}})
	fl.AttachStore(store, fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken, StateDir: t.TempDir(),
		Provision: ProvisionConfig{Host: def}}, nil, nil)
	for name, p := range peers {
		b, _ := json.Marshal(map[string]any{"name": name, "urls": []string{p.srv.URL}})
		if w := do(s, "POST", "/api/peers", userHdr, string(b)); w.Code != 201 {
			t.Fatalf("add peer = %d %s", w.Code, w.Body)
		}
	}
	for name := range peers {
		name := name
		waitUntil(t, name+" up", func() bool {
			for _, h := range fl.Hosts() {
				if h.Name == name && h.Status == fleet.StatusOK {
					return true
				}
			}
			return false
		})
	}
	return s
}

func getJSON(t *testing.T, s *Server, path string, hdr map[string]string, v any) int {
	t.Helper()
	rec := do(s, "GET", path, hdr, "")
	if v != nil && rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, rec.Body)
		}
	}
	return rec.Code
}

type nodesResp struct {
	Enabled bool   `json:"enabled"`
	Default string `json:"default"`
	CSRF    string `json:"csrf"`
	Nodes   []struct {
		Name      string               `json:"name"`
		Default   bool                 `json:"default"`
		State     string               `json:"state"`
		Error     string               `json:"error"`
		Preflight *provision.Preflight `json:"preflight"`
	} `json:"nodes"`
}

func TestHubListsTheProvisioners(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	other := newProvPeer(t, nil) // a peer that is not a provisioner
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov, "kube": other})
	var n nodesResp
	if code := getJSON(t, s, "/api/provision/nodes", userHdr, &n); code != 200 {
		t.Fatalf("%d", code)
	}
	if !n.Enabled || n.Default != "prov" || len(n.Nodes) != 1 || n.CSRF == "" {
		t.Fatalf("%+v", n)
	}
	got := n.Nodes[0]
	if got.Name != "prov" || !got.Default || got.State != "ok" || got.Preflight == nil || got.Preflight.AWS.State != "ok" || got.Preflight.Region != "us-east-2" {
		t.Errorf("%+v", got)
	}
	// the answer is cached for five minutes, including "kube is not a provisioner"
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	if prov.asked("preflight") != 1 || other.asked("preflight") != 1 {
		t.Errorf("preflight asked %d and %d times, want 1 each", prov.asked("preflight"), other.asked("preflight"))
	}
	now := time.Now().Add(6 * time.Minute)
	s.prov.now = func() time.Time { return now }
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	if prov.asked("preflight") != 2 {
		t.Errorf("not asked again after the cache expired: %d", prov.asked("preflight"))
	}
}

func TestHubWithNoProvisionerHidesTheFeature(t *testing.T) {
	other := newProvPeer(t, nil)
	s := provHub(t, "", map[string]*provPeer{"kube": other})
	var n nodesResp
	if code := getJSON(t, s, "/api/provision/nodes", userHdr, &n); code != 200 || n.Enabled || len(n.Nodes) != 0 {
		t.Fatalf("%d %+v", code, n)
	}
	var plain nodesResp
	s2 := provHub(t, "", nil)
	if code := getJSON(t, s2, "/api/provision/nodes", userHdr, &plain); code != 200 || plain.Enabled {
		t.Fatalf("%d %+v", code, plain)
	}
	// the hub keeps working: the app's other routes answer
	if rec := do(s, "GET", "/api/hosts", userHdr, ""); rec.Code != 200 {
		t.Errorf("hosts = %d", rec.Code)
	}
}

func TestHubUnknownProvisionersWithoutADefault(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "", map[string]*provPeer{"prov": prov}) // no -provision-host: a peer that answers is a candidate
	var n nodesResp
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	if !n.Enabled || len(n.Nodes) != 1 || n.Nodes[0].Default || n.Default != "" {
		t.Errorf("%+v", n)
	}
}

func TestHubShowsAnOfflineDefaultAndNotOtherOfflinePeers(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	prov.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return true
	})
	prov.srv.CloseClientConnections()
	waitUntil(t, "prov down", func() bool {
		for _, h := range s.fleet.Hosts() {
			if h.Name == "prov" && h.Status != fleet.StatusOK {
				return true
			}
		}
		return false
	})
	var n nodesResp
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	if len(n.Nodes) != 1 || n.Nodes[0].State != "offline" || !n.Nodes[0].Default || n.Nodes[0].Error == "" {
		t.Errorf("%+v", n)
	}
}

func TestHubKeepsAProvisionersExpiredLoginVisible(t *testing.T) {
	wk := newFakeWorker(t)
	pf := string(mustRead(t, provPF))
	wk.set(t, "preflight.json", strings.Replace(pf, `"aws": {"state": "ok", "note": "profile comebye-provision"}`, `"aws": {"state": "expired", "note": "profile comebye-provision: the login expired; run aws login"}`, 1))
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	var n nodesResp
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	if len(n.Nodes) != 1 || n.Nodes[0].State != "ok" || n.Nodes[0].Preflight.AWS.State != "expired" || !strings.Contains(n.Nodes[0].Preflight.AWS.Note, "aws login") {
		t.Errorf("%+v", n.Nodes)
	}
}

type catResp struct {
	Via       string             `json:"via"`
	Region    string             `json:"region"`
	Hidden    int                `json:"hidden_small"`
	Small     bool               `json:"small"`
	AgeS      int                `json:"age_s"`
	Stale     string             `json:"stale"`
	MinVCPU   int                `json:"min_vcpu"`
	MinMemGiB float64            `json:"min_memory_gib"`
	Catalog   *provision.Catalog `json:"catalog"`
}

// catalogWithSmallSizes is the recorded catalog plus a nano and a micro.
func catalogWithSmallSizes(t *testing.T) string {
	t.Helper()
	var c map[string]any
	if err := json.Unmarshal(mustRead(t, provGolden), &c); err != nil {
		t.Fatal(err)
	}
	rows := c["rows"].([]any)
	for _, r := range []map[string]any{
		{"type": "t3.nano", "vcpu": 2, "memory_gib": 0.5, "arch": "amd64", "gpus": 0, "on_demand_usd_hour": 0.0052, "spot_min_usd_hour": 0.0005, "estimate_spot_usd_hour": 0.0132, "estimate_on_demand_usd_hour": 0.0179, "selectable": true},
		{"type": "t3.micro", "vcpu": 2, "memory_gib": 1, "arch": "amd64", "gpus": 0, "on_demand_usd_hour": 0.0104, "spot_min_usd_hour": 0.0026, "estimate_spot_usd_hour": 0.0153, "estimate_on_demand_usd_hour": 0.0231, "selectable": true},
	} {
		rows = append(rows, r)
	}
	c["rows"] = rows
	b, _ := json.Marshal(c)
	return string(b)
}

func TestHubRelaysTheCatalogAndHidesSmallSizes(t *testing.T) {
	wk := newFakeWorker(t)
	wk.set(t, "catalog.json", catalogWithSmallSizes(t))
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})

	var c catResp
	if code := getJSON(t, s, "/api/provision/catalog?kind=aws&region=us-east-1", userHdr, &c); code != 200 {
		t.Fatalf("%d", code)
	}
	if c.Via != "prov" || c.Region != "us-east-1" || c.Small || c.Hidden != 2 || len(c.Catalog.Rows) != 10 || c.MinVCPU != 2 || c.MinMemGiB != 4 {
		t.Fatalf("via %s region %s hidden %d rows %d", c.Via, c.Region, c.Hidden, len(c.Catalog.Rows))
	}
	for _, r := range c.Catalog.Rows {
		if r.Type == "t3.nano" || r.Type == "t3.micro" {
			t.Errorf("%s shown by default", r.Type)
		}
	}
	var all catResp
	getJSON(t, s, "/api/provision/catalog?kind=aws&region=us-east-1&small=1", userHdr, &all)
	if !all.Small || all.Hidden != 0 || len(all.Catalog.Rows) != 12 {
		t.Errorf("small=1: hidden %d rows %d", all.Hidden, len(all.Catalog.Rows))
	}
	// one question to the node serves both views for five minutes
	if prov.asked("catalog") != 1 {
		t.Errorf("the node was asked %d times", prov.asked("catalog"))
	}
}

func TestHubCatalogRegionIsTheNodesDefaultUnlessChosen(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	// the fake tool always answers for us-east-1: asking for the node's own default (us-east-2) must be refused as a mismatch, not shown as another region
	rec := do(s, "GET", "/api/provision/catalog?kind=aws", userHdr, "")
	if rec.Code != 502 {
		t.Errorf("a node answering for another region = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(strings.Join(prov.paths, " "), "region=us-east-2") {
		t.Errorf("the node's default region was not asked for: %v", prov.paths)
	}
}

func TestHubCatalogRejectsBadRequestsBeforeAskingTheNode(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	for name, path := range map[string]string{
		"gcp":          "/api/provision/catalog?kind=gcp&region=us-east-1",
		"bad region":   "/api/provision/catalog?kind=aws&region=us-east-1%20x",
		"unknown node": "/api/provision/catalog?kind=aws&region=us-east-1&via=nobody",
		"hub itself":   "/api/provision/catalog?kind=aws&region=us-east-1&via=desk",
		"bad score":    "/api/provision/catalog?kind=aws&region=us-east-1&score=%27%3Bls",
	} {
		if rec := do(s, "GET", path, userHdr, ""); rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if prov.asked("catalog") != 0 {
		t.Errorf("the node was asked %d times for a bad request", prov.asked("catalog"))
	}
}

func TestHubServesAStaleCatalogWhenTheNodeFailsAndSaysWhy(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	var c catResp
	if code := getJSON(t, s, "/api/provision/catalog?kind=aws&region=us-east-1", userHdr, &c); code != 200 || c.Stale != "" {
		t.Fatalf("%d %+v", code, c.Stale)
	}
	wk.set(t, "catalog.fail", `{"v":1,"error":{"class":"creds_expired","msg":"login expired"}}`)
	now := time.Now().Add(7 * time.Minute)
	s.prov.now = func() time.Time { return now }
	var st catResp
	if code := getJSON(t, s, "/api/provision/catalog?kind=aws&region=us-east-1", userHdr, &st); code != 200 {
		t.Fatalf("a stale answer should still be served: %d", code)
	}
	if !strings.HasPrefix(st.Stale, "creds_expired") || st.AgeS < 400 || len(st.Catalog.Rows) == 0 {
		t.Errorf("stale %q age %d rows %d", st.Stale, st.AgeS, len(st.Catalog.Rows))
	}
	// and with nothing cached, an expired login is its own clear error
	rec := do(s, "GET", "/api/provision/catalog?kind=aws&region=us-west-2", userHdr, "")
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "creds_expired") {
		t.Errorf("no cache = %d %s", rec.Code, rec.Body)
	}
}

func TestHubDistrustsAProvisionersAnswer(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	evil := `{"v":1,"kind":"aws","region":"us-east-1","sources":{},"estimate":{},"secret":"tskey-auth-LEAK",
	  "rows":[{"type":"t3.xlarge","vcpu":4,"memory_gib":16,"arch":"amd64","note":"visit <script>alert(1)</script> tskey-auth-LEAK","interruption":"<5%","selectable":true,"leak":"AKIAABCDEFGHIJKLMNOP"}]}`
	wk.set(t, "catalog.json", evil)
	rec := do(s, "GET", "/api/provision/catalog?kind=aws&region=us-east-1", userHdr, "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"tskey", "LEAK", "AKIA", "secret"} {
		if strings.Contains(rec.Body.String(), bad) {
			t.Errorf("the answer carries %q:\n%s", bad, rec.Body)
		}
	}
	// garbage is refused, not passed on
	wk.set(t, "catalog.json", `<html>oops</html>`)
	s.prov.now = func() time.Time { return time.Now().Add(9 * time.Minute) }
	if rec := do(s, "GET", "/api/provision/catalog?kind=aws&region=us-east-1", userHdr, ""); strings.Contains(rec.Body.String(), "oops") {
		t.Errorf("garbage relayed: %s", rec.Body)
	}
	// an unprovisioned peer is not a provisioner, whatever the app asks
	other := newProvPeer(t, nil)
	s2 := provHub(t, "kube", map[string]*provPeer{"kube": other})
	if rec := do(s2, "GET", "/api/provision/catalog?kind=aws&region=us-east-1", userHdr, ""); rec.Code != 404 || !strings.Contains(rec.Body.String(), "not_provisioner") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestProvisionRefreshNeedsTheCSRFTokenAndForcesTheNode(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	var n nodesResp
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	body := `{"what":"catalog","via":"prov","kind":"aws","region":"us-east-1"}`
	for name, hdr := range map[string]map[string]string{
		"no token":    userHdr,
		"wrong token": {"Tailscale-User-Login": "me@example.com", HeaderCSRF: "0000"},
	} {
		if rec := do(s, "POST", "/api/provision/refresh", hdr, body); rec.Code != 403 || !strings.Contains(rec.Body.String(), `"csrf"`) {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if prov.asked("catalog") != 0 {
		t.Fatal("a refresh without a token reached the node")
	}
	ok := map[string]string{"Tailscale-User-Login": "me@example.com", HeaderCSRF: n.CSRF}
	if rec := do(s, "POST", "/api/provision/refresh", ok, body); rec.Code != 200 {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(strings.Join(wk.argvLines(), "\n"), "--refresh") {
		t.Errorf("the node was not told to refresh: %v", wk.argvLines())
	}
	// not twice in a row
	if rec := do(s, "POST", "/api/provision/refresh", ok, body); rec.Code != 429 {
		t.Errorf("second refresh = %d", rec.Code)
	}
	// nodes: a recheck asks the preflight again
	before := prov.asked("preflight")
	if rec := do(s, "POST", "/api/provision/refresh", ok, `{"what":"nodes"}`); rec.Code != 200 || prov.asked("preflight") != before+1 {
		t.Errorf("recheck = %d, preflight asked %d times more", rec.Code, prov.asked("preflight")-before)
	}
	if rec := do(s, "POST", "/api/provision/refresh", ok, `{"what":"everything"}`); rec.Code != 400 {
		t.Errorf("unknown what = %d", rec.Code)
	}
}

func (f *fakeWorkerDir) argvLines() []string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "argv"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestProvisionHubRoutesRefuseAgentsAndTaggedNodes(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	tok := s.csrfToken(httptestReq("GET", "/", "").WithContext(context.Background()))
	_ = tok
	calls := []struct{ method, path, body string }{
		{"GET", "/api/provision/nodes", ""},
		{"GET", "/api/provision/catalog?kind=aws&region=us-east-1", ""},
		{"POST", "/api/provision/refresh", `{"what":"nodes"}`},
		{"POST", "/api/provision/plan", `{}`},
		{"POST", "/api/provision", `{}`},
		{"GET", "/api/provision/jobs/hj_0123456789abcdef", ""},
		{"POST", "/api/provision/jobs/hj_0123456789abcdef/retry", ""},
		{"POST", "/api/provision/jobs/hj_0123456789abcdef/cleanup", ""},
		{"POST", "/api/provision/jobs/hj_0123456789abcdef/change", `{"zone":"us-east-1c"}`},
	}
	callers := map[string]map[string]string{
		"coordinator token": {HeaderAgentToken: testToken, HeaderAgent: "w1:p1"},
		"agent token":       {HeaderAgentToken: testToken, HeaderAgent: "w2:p1"},
		"token, no pane":    {HeaderAgentToken: testToken},
		"peer forwarding":   {"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1", HeaderAgent: "w1:p1", HeaderAgentHost: "kube"},
		"wrong token":       {HeaderAgentToken: "nope", "Tailscale-User-Login": "me@example.com"},
		"nobody":            {},
	}
	for who, hdr := range callers {
		for _, c := range calls {
			rec := do(s, c.method, c.path, hdr, c.body)
			want := http.StatusForbidden
			if who == "wrong token" || who == "nobody" {
				want = http.StatusUnauthorized
			}
			if rec.Code != want {
				t.Errorf("%s %s %s = %d %s, want %d", who, c.method, c.path, rec.Code, rec.Body, want)
			}
		}
	}
	if prov.asked("provisioner") != 0 {
		t.Fatalf("a refused caller reached the provisioner: %v", prov.paths)
	}
}

func TestProvisionRoutesRefuseTaggedNodes(t *testing.T) {
	r := newRigWith(t, false, func(c *Config) { c.AllowCap = testCap; c.Provision = ProvisionConfig{Host: ""} })
	r.who.setHolder(r.node, "tag:worker")
	r.addPeer("vm1", r.workerURL(), "tag:worker")
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/provision/nodes", ""},
		{"GET", "/api/provision/catalog?kind=aws&region=us-east-1", ""},
		{"POST", "/api/provision/refresh", `{"what":"nodes"}`},
		{"GET", "/api/provisioner/preflight", ""},
		{"GET", "/api/provisioner/catalog?kind=aws&region=us-east-1", ""},
		{"POST", "/api/provisioner/plan", `{}`},
		{"POST", "/api/provisioner/jobs", `{}`},
		{"GET", "/api/provisioner/job?id=pj_0123456789abcdef", ""},
		{"POST", "/api/provisioner/job/retry", `{}`},
		{"POST", "/api/provisioner/job/cleanup", `{}`},
		{"POST", "/api/provision/plan", `{}`},
		{"POST", "/api/provision", `{}`},
	} {
		code, body := r.serve(c.method, c.path, c.body, map[string]string{HeaderCSRF: "x", fleet.HeaderHubCall: "1"})
		if code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("tagged node %s %s = %d %s", c.method, c.path, code, body)
		}
	}
}

func TestProvisionCrossSiteIsRefused(t *testing.T) {
	wk := newFakeWorker(t)
	prov := newProvPeer(t, wk)
	s := provHub(t, "prov", map[string]*provPeer{"prov": prov})
	h := s.base()
	for name, hdr := range map[string]map[string]string{
		"cross-site":   {"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"},
		"wrong origin": {"Origin": "https://evil.example", "Content-Type": "application/json"},
		"form post":    {"Content-Type": "application/x-www-form-urlencoded"},
	} {
		r := httptestReq("POST", "/api/provision/refresh", `{"what":"nodes"}`)
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		r.Header.Set(HeaderCSRF, s.csrfToken(r))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		if w := serveTo(h, r); w.Code < 400 {
			t.Errorf("%s = %d %s", name, w.Code, w.Body)
		}
	}
	if prov.asked("provisioner") != 0 {
		t.Error("a cross-site request reached the provisioner")
	}
}

var _ = fmt.Sprint

func TestHubSaysWhenTheDefaultIsNotAProvisioner(t *testing.T) {
	other := newProvPeer(t, nil)
	s := provHub(t, "kube", map[string]*provPeer{"kube": other})
	var n nodesResp
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	if n.Enabled && len(n.Nodes) != 1 || len(n.Nodes) != 1 || n.Nodes[0].State != "error" || !strings.Contains(n.Nodes[0].Error, "-provision-config") || n.Nodes[0].Preflight != nil {
		t.Errorf("%+v", n)
	}
}

func TestHubIgnoresAPeerThatKeepsALedger(t *testing.T) {
	// a host with a ledger answers the hub's marker with hub_only: it is no provisioner and is not offered
	hubish := newProvPeer(t, nil)
	hubish.peer.ledger = nil
	hubish.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/api/provisioner/") {
			writeJSONStatus(w, 403, map[string]string{"code": "hub_only", "message": "x"})
			return true
		}
		return false
	})
	s := provHub(t, "", map[string]*provPeer{"other": hubish})
	var n nodesResp
	getJSON(t, s, "/api/provision/nodes", userHdr, &n)
	if n.Enabled || len(n.Nodes) != 0 {
		t.Errorf("%+v", n)
	}
}
