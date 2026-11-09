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
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/usage"
)

// usagePeer is a peer's Comebye that counts tokens: a real usage store
// filled by scanning a transcript directory, served the way a peer serves it.
type usagePeer struct {
	*readPeer
	store *usage.Store
}

func (p *usagePeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/usage/sessions" {
		p.readPeer.mu.Lock()
		p.readPeer.hits = append(p.readPeer.hits, "GET "+r.URL.RequestURI())
		p.readPeer.mu.Unlock()
		since, limit := int64(0), 100
		fmt.Sscan(r.URL.Query().Get("since"), &since)
		fmt.Sscan(r.URL.Query().Get("limit"), &limit)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p.store.SnapshotSince(since, limit))
		return
	}
	p.readPeer.ServeHTTP(w, r)
}

func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func usageLine(id, ts, model, cwd, branch string, in, out, cw, cr int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":%q,"gitBranch":%q,"message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`,
		ts, cwd, branch, id, model, in, out, cw, cr)
}

type usageRig struct {
	*readRig
	peer *usagePeer
}

func newUsageRig(t *testing.T) *usageRig {
	t.Helper()
	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	// The peer ("kube") ran a worker in its worktree and a plain session at home.
	kdir := t.TempDir()
	writeTranscript(t, filepath.Join(kdir, "p", "w.jsonl"),
		usageLine("a", now, "claude-opus-5-5", "/home/k/.worktrees/proj/research-x", "research/x", 100, 200, 300, 4000))
	writeTranscript(t, filepath.Join(kdir, "p", "h.jsonl"),
		usageLine("b", now, "claude-sonnet-5-5", "/home/k", "", 10, 20, 30, 400))
	ks, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"), "kube")
	if err != nil {
		t.Fatal(err)
	}
	ks.SetHome("/home/k")
	if _, err := ks.Scan(context.Background(), []usage.Source{{Kind: "claude", Root: kdir}}); err != nil {
		t.Fatal(err)
	}
	pr := &usagePeer{readPeer: &readPeer{}, store: ks}
	srv := httptest.NewServer(pr)
	t.Cleanup(srv.Close)

	store, err := fleet.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fl := fleet.New(nil, "desk", nil, fleet.Options{HubMode: true, Logf: func(string, ...any) {}})
	fl.AttachStore(store, fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	who := &fakeWhoIs{}
	// The hub's own transcripts.
	hdir := t.TempDir()
	writeTranscript(t, filepath.Join(hdir, "p", "hub.jsonl"),
		usageLine("c", now, "claude-haiku-5-5", "/srv/other", "", 1, 2, 3, 4))
	s := New(Config{
		AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken,
		WhoIs: who, AllowCap: testCap, StateDir: t.TempDir(),
		Usage: UsageConfig{Sources: []usage.Source{{Kind: "claude", Root: hdir}}},
	}, nil, nil)
	r := &readRig{
		tagRig: &tagRig{t: t, fl: fl, who: who, main: httptest.NewServer(s.Handler()), node: "127.0.0.1"},
		s:      s, w1: &readPeer{}, kube: pr.readPeer,
	}
	t.Cleanup(r.main.Close)
	r.addPeer("kube", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), "")
	deadline := time.Now().Add(5 * time.Second)
	for len(fl.Agents()) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("fleet sees %d agents", len(fl.Agents()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, id := range []string{"kube/wC:p1", "desk/wH:p1"} {
		if err := s.recordRole(id, "", RoleCoordinator, "test"); err != nil {
			t.Fatal(err)
		}
	}
	// The handoff the peer's worker was started by.
	if _, err := s.ledger.Add(ledger.Entry{Host: "kube", RepoName: "proj", Branch: "research/x", Purpose: "review", AgentID: "kube/wS:p1", AgentName: "the-worker",
		Worktree: "/home/k/.worktrees/proj/research-x", Status: ledger.Running}); err != nil {
		t.Fatal(err)
	}
	return &usageRig{readRig: r, peer: pr}
}

type usageAnswer struct {
	Totals struct {
		Fresh, CR int64
		USD       float64
		Unpriced  int64
	}
	Groups []struct {
		Key, Label string
		Fresh      int64
	}
	TopSessions []struct{ Host, Project, Role, Purpose, Handoff, Name string } `json:"top_sessions"`
	TopHandoffs []struct {
		Handoff, Name string
		Fresh         int64
	} `json:"top_handoffs"`
	Hosts []struct {
		Name     string
		Err      string
		Gathered int64
	}
	Code string
}

func (r *usageRig) get(hdr map[string]string, path string) (int, usageAnswer) {
	r.t.Helper()
	code, body := r.call(r.main, "GET", path, "", hdr)
	var a usageAnswer
	_ = json.Unmarshal([]byte(body), &a)
	return code, a
}

func TestUsageGatheredFromAPeerAndLabelled(t *testing.T) {
	r := newUsageRig(t)
	r.s.usageOnce(context.Background())

	code, a := r.get(userHdr, "/api/usage?by=host&since=2d")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	// fresh = in+out+cw: peer worker 600, peer home 60, hub 6; cache reads apart.
	if a.Totals.Fresh != 600+60+6 || a.Totals.CR != 4000+400+4 {
		t.Fatalf("totals %+v", a.Totals)
	}
	got := map[string]int64{}
	for _, g := range a.Groups {
		got[g.Key] = g.Fresh
	}
	if got["kube"] != 660 || got["desk"] != 6 {
		t.Fatalf("by host %v", got)
	}
	// The ledger labels the worker's session; its --purpose wins over the branch prefix.
	_, a = r.get(userHdr, "/api/usage?by=handoff&since=2d&project=proj")
	if len(a.Groups) != 1 || a.Groups[0].Label != "the-worker" || a.Groups[0].Fresh != 600 || len(a.TopHandoffs) != 1 {
		t.Fatalf("by handoff %+v", a)
	}
	if len(a.TopSessions) != 1 || a.TopSessions[0].Role != "worker" || a.TopSessions[0].Purpose != "review" || a.TopSessions[0].Host != "kube" {
		t.Fatalf("sessions %+v", a.TopSessions)
	}
	_, a = r.get(userHdr, "/api/usage?by=role&since=2d")
	roles := map[string]int64{}
	for _, g := range a.Groups {
		roles[g.Key] = g.Fresh
	}
	if roles["worker"] != 600 || roles["home"] != 66 {
		t.Fatalf("by role %v", roles)
	}
	// Both models of the priced Claude family get dollars; nothing is unpriced.
	_, a = r.get(userHdr, "/api/usage?by=model&since=2d")
	if a.Totals.USD <= 0 || a.Totals.Unpriced != 0 {
		t.Fatalf("usd %+v", a.Totals)
	}
	for _, h := range a.Hosts {
		if h.Err != "" || h.Gathered == 0 {
			t.Fatalf("host %+v", h)
		}
	}
	// Gathering again takes only what changed: the same totals, no double count.
	before := r.kube.count("/api/usage/sessions")
	r.s.usageOnce(context.Background())
	if _, b := r.get(userHdr, "/api/usage?since=2d"); b.Totals.Fresh != a.Totals.Fresh {
		t.Fatalf("after a second gather %d vs %d", b.Totals.Fresh, a.Totals.Fresh)
	}
	if r.kube.count("/api/usage/sessions") == before {
		t.Fatal("the peer was not asked again")
	}
	// The labels are what the store keeps; the peer never sees the ledger.
	for _, h := range r.kube.hits {
		if strings.Contains(h, "the-worker") {
			t.Fatalf("the ledger reached the peer: %s", h)
		}
	}
}

func TestUsageAuthMatrix(t *testing.T) {
	r := newUsageRig(t)
	r.s.usageOnce(context.Background())
	coordHere := map[string]string{HeaderAgentToken: testToken, HeaderAgent: "wH:p1"}
	workerHere := map[string]string{HeaderAgentToken: testToken, HeaderAgent: "wW:p1"}
	noPane := map[string]string{HeaderAgentToken: testToken}
	coordPeer := forwarded("kube", "wC:p1")
	workerPeer := forwarded("kube", "wS:p1")
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		hdr    map[string]string
		code   int
	}{
		{"the user reads usage", "GET", "/api/usage", "", userHdr, 200},
		{"a coordinator on the hub reads usage", "GET", "/api/usage", "", coordHere, 200},
		{"a coordinator on a peer reads usage", "GET", "/api/usage", "", coordPeer, 200},
		{"a worker on the hub cannot", "GET", "/api/usage", "", workerHere, 403},
		{"a worker on a peer cannot", "GET", "/api/usage", "", workerPeer, 403},
		{"an agent with no pane cannot", "GET", "/api/usage", "", noPane, 403},
		{"no login at all", "GET", "/api/usage", "", map[string]string{}, 401},
		{"a coordinator cannot gather sessions", "GET", "/api/usage/sessions", "", coordHere, 403},
		{"a coordinator cannot read prices", "GET", "/api/usage/prices", "", coordHere, 403},
		{"a coordinator cannot set prices", "PUT", "/api/usage/prices", `{"custom":{}}`, coordHere, 403},
		{"a worker cannot set prices", "PUT", "/api/usage/prices", `{"custom":{}}`, workerHere, 403},
		{"the user sets prices", "PUT", "/api/usage/prices", `{"custom":{"homelab/glm":{"in":1,"out":2,"cw":0,"cr":0.1}}}`, userHdr, 200},
		{"a bad price is refused", "PUT", "/api/usage/prices", `{"custom":{"x":{"in":-1}}}`, userHdr, 400},
		{"the user reads prices", "GET", "/api/usage/prices", "", userHdr, 200},
		{"a bad dimension", "GET", "/api/usage?by=nonsense", "", userHdr, 400},
		{"a bad window", "GET", "/api/usage?from=2026-11-05&to=2026-11-01", "", userHdr, 400},
	}
	for _, c := range cases {
		h := map[string]string{}
		for k, v := range c.hdr {
			h[k] = v
		}
		if c.method == "PUT" {
			h["Content-Type"] = "application/json"
		}
		code, body := r.call(r.main, c.method, c.path, c.body, h)
		if code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.code)
		}
	}
	// A tagged worker (through Serve with the capability) is the forwarded agent role only: no coordinator.
	if code, _ := r.serve("GET", "/api/usage", "", map[string]string{HeaderAgent: "wC:p1"}); code != 403 {
		t.Errorf("a tagged node read usage: %d", code)
	}
	// Prices set by the user apply: a Pi model gets dollars now.
	code, _ := r.call(r.main, "GET", "/api/usage?by=model", "", userHdr)
	if code != 200 {
		t.Fatal(code)
	}
}

func TestUsageOffWithoutStateDir(t *testing.T) {
	s := New(Config{AllowLogins: []string{"me@example.com"}}, nil, nil)
	req := httptest.NewRequest("GET", "/api/usage", nil)
	req.Header.Set("Tailscale-User-Login", "me@example.com")
	req.Host = "localhost"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("code %d %s", w.Code, w.Body)
	}
}

func TestUsagePurposeFlagWinsOverBranchPrefix(t *testing.T) {
	for _, c := range []struct{ flag, branch, want string }{
		{"review", "feat/x", "review"},
		{"spike", "feat/x", "research"},
		{"", "feat/x", "build"},
		{"", "fix-y", "build"},
		{"", "research/z", "research"},
		{"", "odd-name", ""},
		{"", "deploy-1", "rollout"},
	} {
		if got := usagePurpose(c.flag, c.branch); got != c.want {
			t.Errorf("usagePurpose(%q,%q) = %q, want %q", c.flag, c.branch, got, c.want)
		}
	}
}
