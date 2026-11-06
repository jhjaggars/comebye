package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// A marker the fake peers put in everything they serve: a test that finds it
// in a refused caller's answer has found a leak.
const peerText = "text-only-the-user-may-see"

// readPeer is a peer's Comebye, with a coordinator and a worker.
type readPeer struct {
	mu     sync.Mutex
	hits   []string
	claims []string // agent-role headers the hub sent on (there must be none)
}

func (p *readPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hits = append(p.hits, r.Method+" "+r.URL.EscapedPath())
	for _, h := range []string{HeaderForwarded, HeaderAgent, HeaderAgentHost, HeaderAgentToken} {
		if v := r.Header.Get(h); v != "" {
			p.claims = append(p.claims, h+"="+v)
		}
	}
	p.mu.Unlock()
	switch {
	case r.URL.Path == "/api/events":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: agents\n"+`data: {"type":"agents","version":"v","agents":[{"pane_id":"wS:p1","name":"worker","agent_status":"idle"},{"pane_id":"wC:p1","name":"coordinator","agent_status":"idle"}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case strings.HasSuffix(r.URL.Path, "/transcript"), strings.HasSuffix(r.URL.Path, "/output"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"text":%q,"messages":[]}`, peerText)
	case strings.HasSuffix(r.URL.Path, "/prompt"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	default:
		http.NotFound(w, r)
	}
}

func (p *readPeer) count(sub string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, h := range p.hits {
		if strings.Contains(h, sub) {
			n++
		}
	}
	return n
}

// readRig is a hub ("desk", with a ledger) and two peers: "w1", a tagged
// spot worker, and "kube", another machine. The coordinators of record are
// kube/wC:p1 and desk/wH:p1. The hub's handlers for its own agents are not
// callable here (it has no herdr): that side is in TestReadRuleOnTheHubsOwnAgents.
type readRig struct {
	*tagRig
	s    *Server
	w1   *readPeer
	kube *readPeer
}

func newReadRig(t *testing.T) *readRig {
	t.Helper()
	w1, kube := &readPeer{}, &readPeer{}
	ws, ks := httptest.NewServer(w1), httptest.NewServer(kube)
	t.Cleanup(ws.Close)
	t.Cleanup(ks.Close)
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
	s := New(Config{
		AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken,
		WhoIs: who, AllowCap: testCap, StateDir: t.TempDir(),
	}, nil, nil)
	r := &readRig{
		tagRig: &tagRig{t: t, fl: fl, who: who, main: httptest.NewServer(s.Handler()), node: "127.0.0.1"},
		s:      s, w1: w1, kube: kube,
	}
	t.Cleanup(r.main.Close)
	// fakeWhoIs says the caller is the node at 127.0.0.1, which is w1; kube
	// is the same fake machine under another name, so it is not that node.
	r.addPeer("w1", ws.URL, "tag:worker")
	r.addPeer("kube", strings.Replace(ks.URL, "127.0.0.1", "localhost", 1), "")
	r.who.setHolder("127.0.0.1", "tag:worker")
	deadline := time.Now().Add(5 * time.Second)
	for len(fl.Agents()) != 4 {
		if time.Now().After(deadline) {
			t.Fatalf("fleet sees %d agents, want 4: %+v", len(fl.Agents()), fl.Agents())
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, id := range []string{"kube/wC:p1", "desk/wH:p1"} {
		if err := s.recordRole(id, "", RoleCoordinator, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// forwarded is a peer's forwarded agent request, as a login-owning peer sends it.
func forwarded(host, pane string) map[string]string {
	return map[string]string{"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1", HeaderAgentHost: host, HeaderAgent: pane}
}

// asTagged runs one request by the tagged worker, through Serve with the
// capability.
func (r *readRig) asTagged(method, path, body string) (int, string) {
	r.t.Helper()
	return r.serve(method, path, body, map[string]string{HeaderAgent: "wS:p1"})
}

var readRoutes = []string{
	"/api/hosts/kube/agents/wS:p1/output",
	"/api/hosts/kube/agents/wS:p1/transcript",
	"/api/hosts/kube/agents/wS:p1/subagents/a1234567/transcript",
}

// A tagged worker gets 403 for every agent's text: another host's, the hub's
// and its own host's, and nothing reaches the peer.
func TestTaggedWorkerCannotReadAnyAgent(t *testing.T) {
	r := newReadRig(t)
	paths := append([]string{
		"/api/agents/wH:p1/output",
		"/api/agents/wH:p1/transcript",
		"/api/agents/wH:p1/subagents/a1234567/transcript",
		"/api/hosts/desk/agents/wH:p1/output",
		"/api/hosts/desk/agents/wH:p1/transcript",
		"/api/hosts/desk/agents/wH:p1/subagents/a1234567/transcript",
		// Its own host's panes, itself included.
		"/api/hosts/w1/agents/wS:p1/output",
		"/api/hosts/w1/agents/wS:p1/transcript",
		"/api/hosts/w1/agents/wS:p1/subagents/a1234567/transcript",
		// User-only already; here so a change to them shows.
		"/api/hosts/kube/agents/wS:p1/dialog",
		"/api/hosts/kube/agents/wS:p1/activity",
		"/api/agents/wS:p1/dialog",
		"/api/agents/wS:p1/activity",
	}, readRoutes...)
	for _, p := range paths {
		code, body := r.asTagged("GET", p, "")
		if code != http.StatusForbidden || strings.Contains(body, peerText) {
			t.Errorf("GET %s: %d %s", p, code, body)
		}
	}
	if r.kube.count("/agents/") != 0 || r.w1.count("/agents/") != 0 {
		t.Errorf("a refused read reached a peer: kube %v, w1 %v", r.kube.hits, r.w1.hits)
	}
}

// The tagged worker's claims don't help it.
func TestTaggedWorkerClaimsDoNotHelpItRead(t *testing.T) {
	r := newReadRig(t)
	// A login means Serve vouched for a user (Serve strips a client's own),
	// so capPeers lets it by; everything else is still the tagged node.
	for _, h := range []map[string]string{
		{HeaderAgentToken: testToken},
		{HeaderForwarded: "1", HeaderAgentHost: "kube", HeaderAgent: "wC:p1"},
		{HeaderAgent: "wC:p1"},
	} {
		code, body := r.serve("GET", readRoutes[1], "", h)
		if code != http.StatusForbidden || strings.Contains(body, peerText) {
			t.Errorf("claims %v: %d %s", h, code, body)
		}
	}
}

// Every other role against a peer's agents, through the hub.
func TestReadRuleThroughTheHub(t *testing.T) {
	r := newReadRig(t)
	cases := []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"the user", userHdr, 200},
		{"an agent on the target's host", forwarded("kube", "wS:p1"), 200},
		{"the target's host, in another case", forwarded("KUBE", "wS:p1"), 200},
		{"the coordinator, on the target's host", forwarded("kube", "wC:p1"), 200},
		{"a coordinator on the hub, by the token", agentHdr("wH:p1"), 200},
		{"an agent on the hub, by the token", agentHdr("wZ:p1"), 403},
		{"a token holder with no pane", agentHdr(""), 403},
		{"a worker on another host", forwarded("laharl", "wZ:p1"), 403},
		{"a coordinator's pane on the wrong host", forwarded("laharl", "wC:p1"), 403},
		{"a forwarded request naming no host", forwarded("", "wZ:p1"), 403},
	}
	for _, c := range cases {
		for _, p := range readRoutes {
			code, body := r.call(r.main, "GET", p, "", c.hdr)
			if code != c.code {
				t.Errorf("%s: GET %s: %d, want %d: %s", c.name, p, code, c.code, body)
			}
			if c.code == 403 && (strings.Contains(body, peerText) || !strings.Contains(body, "read_forbidden")) {
				t.Errorf("%s: GET %s: %s", c.name, p, body)
			}
		}
	}
	// A coordinator on a third host reads this one.
	if err := r.s.recordRole("laharl/w21:p1", "", RoleCoordinator, "test"); err != nil {
		t.Fatal(err)
	}
	if code, body := r.call(r.main, "GET", readRoutes[1], "", forwarded("laharl", "w21:p1")); code != 200 {
		t.Errorf("a coordinator on another host: %d %s", code, body)
	}
	// The hub speaks for itself: no caller's agent claims reach the peer.
	if len(r.kube.claims) != 0 {
		t.Errorf("headers passed on to the peer: %v", r.kube.claims)
	}
}

// A recorded coordinator's pane that now runs something else isn't one.
func TestReadRuleCoordinatorPaneReused(t *testing.T) {
	r := newReadRig(t)
	if err := r.s.recordRole("kube/wS:p1", "gone", RoleCoordinator, "test"); err != nil {
		t.Fatal(err)
	}
	if code, body := r.call(r.main, "GET", "/api/hosts/w1/agents/wS:p1/transcript", "", forwarded("kube", "wS:p1")); code != http.StatusForbidden {
		t.Errorf("%d %s", code, body)
	}
}

// The hub's own agents, on both its bare and its host-qualified routes.
func TestReadRuleOnTheHubsOwnAgents(t *testing.T) {
	s, _, _ := coordServer(t)
	paths := []string{
		"/api/agents/w2:p1/output", "/api/agents/w2:p1/transcript", "/api/agents/w2:p1/subagents/abc/transcript",
		"/api/hosts/desk/agents/w2:p1/output", "/api/hosts/desk/agents/w2:p1/transcript", "/api/hosts/desk/agents/w2:p1/subagents/abc/transcript",
	}
	if err := s.recordRole("laharl/w21:p1", "", RoleCoordinator, "test"); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		for _, c := range []struct {
			name    string
			hdr     map[string]string
			allowed bool
		}{
			{"the user", userHdr, true},
			{"a local agent", agentHdr("w1:p1"), true},
			{"a local agent reading itself", agentHdr("w2:p1"), true},
			{"a coordinator on a peer", forwarded("laharl", "w21:p1"), true},
			{"a worker on a peer", forwarded("laharl", "w9:p1"), false},
		} {
			w := do(s, "GET", p, c.hdr, "")
			// Allowed means past the rule: this fake has no output or session.
			if refused := w.Code == http.StatusForbidden; refused == c.allowed {
				t.Errorf("%s: GET %s: %d %s", c.name, p, w.Code, w.Body)
			}
		}
	}
}

// A peer still forwards an agent's read to the hub, where the rule is.
func TestPeerForwardsReadsToTheHub(t *testing.T) {
	hub := &fakeHub{}
	hs := httptest.NewServer(hub)
	defer hs.Close()
	up, err := fleet.NewUpstream(hs.URL, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{AllowLogins: []string{"me@example.com"}, HostName: "kube", StateDir: t.TempDir(), Upstream: up, AgentToken: testToken}, nil, nil)
	for path, hubPath := range map[string]string{
		"/api/agents/w2:p1/output":                   "/api/hosts/kube/agents/w2:p1/output",
		"/api/agents/w2:p1/transcript":               "/api/hosts/kube/agents/w2:p1/transcript",
		"/api/agents/w2:p1/subagents/abc/transcript": "/api/hosts/kube/agents/w2:p1/subagents/abc/transcript",
		"/api/hosts/desk/agents/w3:p1/transcript":    "/api/hosts/desk/agents/w3:p1/transcript",
	} {
		w := do(s, "GET", path, agentHdr("w1:p1"), "")
		got, _ := hub.last()
		if w.Code != 200 || got == nil || got.URL.Path != hubPath || got.Header.Get(HeaderAgentHost) != "kube" {
			t.Errorf("%s: %d, hub saw %v", path, w.Code, got)
		}
	}
}

// What a tagged worker may send, and what it may not.
func TestTaggedWorkerWrites(t *testing.T) {
	r := newReadRig(t)
	// Prompts: a recorded coordinator, on any host, by pane or name; agents
	// on its own host.
	for _, p := range []string{
		"/api/hosts/kube/agents/wC:p1/prompt",
		"/api/hosts/kube/agents/coordinator/prompt",
		"/api/hosts/w1/agents/wC:p1/prompt",
	} {
		if code, body := r.asTagged("POST", p, `{"text":"hi"}`); code != 200 {
			t.Errorf("POST %s: %d %s", p, code, body)
		}
	}
	// Not itself (the existing rule), nor anyone else elsewhere.
	if code, body := r.asTagged("POST", "/api/hosts/w1/agents/wS:p1/prompt", `{"text":"hi"}`); code != http.StatusConflict {
		t.Errorf("itself: %d %s", code, body)
	}
	before := r.kube.count("/prompt")
	for _, p := range []string{
		"/api/hosts/kube/agents/wS:p1/prompt",
		"/api/hosts/kube/agents/worker/prompt",
		"/api/hosts/desk/agents/wZ:p1/prompt",
		"/api/agents/wZ:p1/prompt",
	} {
		if code, body := r.asTagged("POST", p, `{"text":"hi"}`); code != http.StatusForbidden || !strings.Contains(body, "prompt_forbidden") {
			t.Errorf("POST %s: %d %s", p, code, body)
		}
	}
	if n := r.kube.count("/prompt"); n != before {
		t.Errorf("a refused prompt reached the peer (%d, was %d)", n, before)
	}
	// The queue is a prompt that waits: the same rule.
	if code, body := r.asTagged("POST", "/api/queue", `{"agent":"kube/wS:p1","text":"hi"}`); code != http.StatusForbidden || !strings.Contains(body, "prompt_forbidden") {
		t.Errorf("queue to a worker elsewhere: %d %s", code, body)
	}
	if code, body := r.serve("POST", "/api/queue", `{"agent":"kube/wC:p1","text":"queued to the coordinator"}`, map[string]string{HeaderAgent: "wS:p1"}); code != http.StatusAccepted {
		t.Errorf("queue to the coordinator: %d %s", code, body)
	}
	// Everything else a worker has no use for is refused, with no effect.
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/handoffs", `{"host":"w1","repo":"/r","branch":"b","brief":"x"}`},
		{"POST", "/api/handoffs", `{"host":"kube","repo":"/r","branch":"b","brief":"x"}`},
		{"POST", "/api/handoffs/x/release", `{}`},
		{"POST", "/api/handoffs/x/cancel", `{}`},
		{"POST", "/api/landings", `{"agent_id":"kube/wS:p1","note":"ok"}`},
		{"POST", "/api/plans", `{"title":"t","size":1}`},
		{"GET", "/api/plans", ""},
		{"GET", "/api/plans/x", ""},
		{"POST", "/api/marks", `{"agent_id":"kube/wS:p1","reason":"r"}`},
		{"POST", "/api/marks/clear", `{"agent_id":"kube/wS:p1"}`},
		{"POST", "/api/sweep", `{"dry_run":true}`},
		{"POST", "/api/agents/wZ:p1/model", `{"model":"m"}`},
		{"POST", "/api/hosts/kube/agents/wS:p1/model", `{"model":"m"}`},
		{"POST", "/api/brief", `{"markdown":"x"}`},
		{"GET", "/api/brief", ""},
		{"GET", "/api/brief/schedule", ""},
		{"POST", "/api/brief/schedule", `{"enabled":false}`},
		{"POST", "/api/brief/schedule/dismiss", `{"id":"x"}`},
		{"POST", "/api/todos/x/answer", `{"text":"t"}`},
		{"POST", "/api/todos/x/done", `{}`},
		{"POST", "/api/todos/x/drop", `{}`},
		{"GET", "/api/models", ""},
		{"GET", "/api/hosts/kube/models", ""},
	} {
		if code, body := r.asTagged(c.m, c.p, c.b); code != http.StatusForbidden || !strings.Contains(body, "worker_forbidden") {
			t.Errorf("%s %s: %d %s", c.m, c.p, code, body)
		}
	}
	// Keys, answers, images, removals: the user's, as ever.
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/agents/wZ:p1/keys", `{"keys":["esc"]}`},
		{"POST", "/api/hosts/kube/agents/wS:p1/keys", `{"keys":["esc"]}`},
		{"POST", "/api/agents/wZ:p1/answer", `{}`},
		{"POST", "/api/hosts/kube/agents/wS:p1/answer", `{}`},
		{"POST", "/api/agents/wZ:p1/image", `{}`},
		{"POST", "/api/hosts/kube/agents/wS:p1/image", `{}`},
		{"POST", "/api/remove", `{}`},
		{"POST", "/api/worker", `{}`},
		{"POST", "/api/removals/confirm", `{}`},
		{"GET", "/api/removals", ""},
	} {
		if code, body := r.asTagged(c.m, c.p, c.b); code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("%s %s: %d %s", c.m, c.p, code, body)
		}
	}
	// What it needs still works. A report is refused only for not being a
	// handoff worker, past the gate.
	if code, body := r.asTagged("POST", "/api/report", `{"text":"done"}`); code != http.StatusNotFound || !strings.Contains(body, "no_handoff") {
		t.Errorf("report: %d %s", code, body)
	}
	for _, p := range []string{"/api/whoami", "/api/version", "/api/playbook", "/api/agents", "/api/hosts", "/api/roles", "/api/ledger", "/api/queue", "/api/todos"} {
		if code, body := r.asTagged("GET", p, ""); code != 200 {
			t.Errorf("GET %s: %d %s", p, code, body)
		}
	}
}

// Everyone else's writes are as they were.
func TestUntaggedWritesUnchanged(t *testing.T) {
	r := newReadRig(t)
	for name, hdr := range map[string]map[string]string{
		"the user":               userHdr,
		"a worker on a peer":     forwarded("laharl", "wZ:p1"),
		"a coordinator":          forwarded("kube", "wC:p1"),
		"an agent, by the token": agentHdr("wZ:p1"),
	} {
		for _, p := range []string{"/api/hosts/kube/agents/wS:p1/prompt", "/api/hosts/w1/agents/wS:p1/prompt"} {
			if code, body := r.call(r.main, "POST", p, `{"text":"hi"}`, hdr); code != 200 {
				t.Errorf("%s: POST %s: %d %s", name, p, code, body)
			}
		}
		if code, body := r.call(r.main, "GET", "/api/brief", "", hdr); code != 200 {
			t.Errorf("%s: GET /api/brief: %d %s", name, code, body)
		}
	}
}

func (r *readRig) ledgerEntry(e ledger.Entry) ledger.Entry {
	r.t.Helper()
	got, err := r.s.ledger.Add(e)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// The ledger keeps only the outline of other hosts' work for a tagged
// worker: its own host's in full, and the reports its steps wait on.
func TestTaggedLedgerHidesOtherHostsText(t *testing.T) {
	r := newReadRig(t)
	other := r.ledgerEntry(ledger.Entry{Host: "kube", Repo: "/r", RepoName: "r", Branch: "other", Brief: peerText, AgentID: "kube/wS:p1", PaneID: "wS:p1", Status: ledger.Running})
	wanted := r.ledgerEntry(ledger.Entry{Host: "kube", Repo: "/r", RepoName: "r", Branch: "wanted", Brief: peerText, AgentID: "kube/wC:p1", PaneID: "wC:p1", Status: ledger.Running})
	own := r.ledgerEntry(ledger.Entry{Host: "w1", Repo: "/r", RepoName: "r", Branch: "own", Brief: "own brief", AgentID: "w1/wS:p1", PaneID: "wS:p1", Status: ledger.Running, After: []string{wanted.ID}})
	for _, id := range []string{"kube/wS:p1", "kube/wC:p1"} {
		if _, _, err := r.s.ledger.Report(id, "report of "+id+" "+peerText, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.s.ledger.SetMark(ledger.Mark{AgentID: "kube/wZ:p1", Host: "kube", PaneID: "wZ:p1", Reason: peerText, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.ledger.SetMark(ledger.Mark{AgentID: "w1/wY:p1", Host: "w1", PaneID: "wY:p1", Reason: "own mark", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	code, body := r.asTagged("GET", "/api/ledger", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var lg struct {
		Entries []entryView       `json:"entries"`
		Marks   []ledger.Mark     `json:"marks"`
		Plans   []json.RawMessage `json:"plans"`
		Roles   map[string]Role   `json:"roles"`
	}
	if err := json.Unmarshal([]byte(body), &lg); err != nil {
		t.Fatal(err)
	}
	byID := map[string]entryView{}
	for _, e := range lg.Entries {
		byID[e.ID] = e
	}
	if e := byID[other.ID]; e.Branch != "other" || e.Status == "" || e.Brief != "" || e.FinalReport != "" {
		t.Errorf("another host's entry: %+v", e)
	}
	if e := byID[own.ID]; e.Brief != "own brief" {
		t.Errorf("its own entry: %+v", e)
	}
	if e := byID[wanted.ID]; !strings.HasPrefix(e.FinalReport, "report of kube/wC:p1") || e.Brief != "" {
		t.Errorf("the step it waits on: %+v", e)
	}
	if len(lg.Marks) != 1 || lg.Marks[0].AgentID != "w1/wY:p1" {
		t.Errorf("marks: %+v", lg.Marks)
	}
	if len(lg.Plans) != 0 {
		t.Errorf("plans: %s", lg.Plans)
	}
	if _, ok := lg.Roles["kube/wC:p1"]; !ok {
		t.Errorf("roles: it has to be able to find its coordinator: %+v", lg.Roles)
	}
	if strings.Count(body, peerText) != 1 {
		t.Errorf("other hosts' text in the answer (the one report it waits on is allowed): %s", body)
	}
	// Everyone else sees it all.
	for name, hdr := range map[string]map[string]string{"the user": userHdr, "a worker on a peer": forwarded("laharl", "wZ:p1"), "a local agent": agentHdr("wZ:p1")} {
		_, b := r.call(r.main, "GET", "/api/ledger", "", hdr)
		if strings.Count(b, peerText) < 4 {
			t.Errorf("%s: the ledger lost text: %s", name, b)
		}
	}
}

func TestTaggedQueueOnlyItsOwnHost(t *testing.T) {
	r := newReadRig(t)
	mine := map[string]string{HeaderAgent: "wS:p1"}
	if code, body := r.call(r.main, "POST", "/api/queue", `{"agent":"kube/wS:p1","text":"`+peerText+`"}`, forwarded("laharl", "wZ:p1")); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	_, body := r.serve("POST", "/api/queue", `{"agent":"kube/wC:p1","text":"from the worker"}`, mine)
	var q struct {
		Queued QueuedPrompt `json:"queued"`
	}
	json.Unmarshal([]byte(body), &q)
	if q.Queued.ID == "" {
		t.Fatalf("queue: %s", body)
	}
	_, list := r.serve("GET", "/api/queue?all=1", "", mine)
	if strings.Contains(list, peerText) || !strings.Contains(list, "from the worker") {
		t.Errorf("list: %s", list)
	}
	_, list = r.serve("GET", "/api/queue?all=1&agent=kube/wS:p1", "", mine)
	if strings.Contains(list, peerText) {
		t.Errorf("list by agent: %s", list)
	}
	// It can withdraw what it queued, and nothing else.
	var all struct {
		Queue []QueuedPrompt `json:"queue"`
	}
	_, ulist := r.call(r.main, "GET", "/api/queue?all=1", "", userHdr)
	json.Unmarshal([]byte(ulist), &all)
	refused := 0
	for _, it := range all.Queue {
		if strings.Contains(it.Text, peerText) {
			refused++
			if code, b := r.serve("DELETE", "/api/queue/"+it.ID, "", mine); code != http.StatusForbidden || !strings.Contains(b, "worker_forbidden") {
				t.Errorf("withdraw another's: %d %s", code, b)
			}
		}
	}
	if refused != 1 {
		t.Errorf("the other prompt is not on the list: %s", ulist)
	}
	// Its own passes the gate (an idle target has it already: 404, not 403).
	if code, b := r.serve("DELETE", "/api/queue/"+q.Queued.ID, "", mine); code == http.StatusForbidden {
		t.Errorf("withdraw its own: %d %s", code, b)
	}
	if code, b := r.serve("DELETE", "/api/queue/nope", "", mine); code != http.StatusForbidden {
		t.Errorf("withdraw an unknown id: %d %s", code, b)
	}
}

func TestTaggedInspectOnlyItsOwnHost(t *testing.T) {
	r := newReadRig(t)
	for _, id := range []string{"kube/wS:p1", "desk/wH:p1", "kube/wC:p1"} {
		for _, q := range []string{"", "&diff=1"} {
			if code, body := r.asTagged("GET", "/api/inspect?agent="+id+q, ""); code != http.StatusForbidden || !strings.Contains(body, "read_forbidden") {
				t.Errorf("%s%s: %d %s", id, q, code, body)
			}
		}
	}
	if code, body := r.asTagged("GET", "/api/inspect?agent=nothing", ""); code != http.StatusForbidden {
		t.Errorf("no host: %d %s", code, body)
	}
	// Past the gate, on its own host (the fake peer has no checks to give).
	if code, body := r.asTagged("GET", "/api/inspect?agent=w1/wS:p1", ""); code == http.StatusForbidden {
		t.Errorf("its own host: %d %s", code, body)
	}
	if code, body := r.call(r.main, "GET", "/api/inspect?agent=kube/wS:p1", "", forwarded("laharl", "wZ:p1")); code == http.StatusForbidden {
		t.Errorf("an untagged agent: %d %s", code, body)
	}
}

func TestTaggedTodosOnlyItsOwn(t *testing.T) {
	r := newReadRig(t)
	add := func(hdr map[string]string, q, extra string) string {
		t.Helper()
		var res struct {
			Todo Todo `json:"todo"`
		}
		_, body := r.call(r.main, "POST", "/api/todos", `{"question":"`+q+`","context":"`+peerText+`"`+extra+`}`, hdr)
		if err := json.Unmarshal([]byte(body), &res); err != nil || res.Todo.ID == "" {
			t.Fatalf("add %q: %s", q, body)
		}
		return res.Todo.ID
	}
	elsewhere := add(forwarded("kube", "wS:p1"), "elsewhere", "")
	waitingHere := add(forwarded("kube", "wC:p1"), "waiting here", `,"waiting":"w1/wS:p1"`)
	if code, body := r.call(r.main, "POST", "/api/todos/"+elsewhere+"/answer", `{"text":"the answer elsewhere"}`, userHdr); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	mine := map[string]string{HeaderAgent: "wS:p1"}
	code, body := r.serve("POST", "/api/todos", `{"question":"mine"}`, mine)
	if code != http.StatusCreated {
		t.Fatalf("a worker adds: %d %s", code, body)
	}
	var res struct {
		Todo Todo `json:"todo"`
	}
	json.Unmarshal([]byte(body), &res)
	_, list := r.serve("GET", "/api/todos?state=all", "", mine)
	var got struct {
		Todos []Todo `json:"todos"`
		Open  int    `json:"open"`
		Ans   int    `json:"answered"`
	}
	json.Unmarshal([]byte(list), &got)
	if len(got.Todos) != 2 || got.Open != 2 || got.Ans != 0 || strings.Contains(list, "the answer elsewhere") || strings.Contains(list, "elsewhere") {
		t.Errorf("list: %s", list)
	}
	for id, want := range map[string]int{res.Todo.ID: 200, waitingHere: 200, elsewhere: http.StatusNotFound, "nope": http.StatusNotFound} {
		if code, b := r.serve("GET", "/api/todos/"+id, "", mine); code != want || (want != 200 && strings.Contains(b, "elsewhere")) {
			t.Errorf("show %s: %d %s", id, code, b)
		}
	}
	// Everyone else lists them all.
	for name, hdr := range map[string]map[string]string{"the user": userHdr, "a worker on a peer": forwarded("laharl", "wZ:p1")} {
		_, b := r.call(r.main, "GET", "/api/todos?state=all", "", hdr)
		if !strings.Contains(b, "the answer elsewhere") {
			t.Errorf("%s: %s", name, b)
		}
	}
}

// A tagged node gets no herdr records either; the hub reading a peer does.
func TestTaggedAgentsAreTheSlimView(t *testing.T) {
	r := newReadRig(t)
	code, body := r.serve("GET", "/api/agents?full=1", "", map[string]string{HeaderAgent: "wS:p1"})
	if code != 200 || strings.Contains(body, "workspace_id") {
		t.Errorf("%d %s", code, body)
	}
}
