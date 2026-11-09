package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/digest"
	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// resetRig is a hub with a Claude coordinator (w1:p1, named coordinator, in
// /work/coord) whose session s1 holds `tokens` of context.
type resetRig struct {
	t    *testing.T
	s    *Server
	fh   *fakeLaunchHerdr
	proj string // the projects directory
}

const resetCwd = "/work/coord"

func (r *resetRig) transcript(session string, lines ...string) {
	dir := filepath.Join(r.proj, transcript.ClaudeProjectSlug(resetCwd))
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func usageLine(tokens int) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"model":"claude-opus","usage":{"input_tokens":3,"cache_creation_input_tokens":%d,"cache_read_input_tokens":0}}}`, tokens-3)
}

// newResetRig builds the rig. The fake herdr answers /clear like a real one:
// a new session id, the name dropped, and (landDigest) a digest marker in
// the new session's transcript.
func newResetRig(t *testing.T, tokens int, withHook, landDigest bool) *resetRig {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	s, fh, _ := coordServer(t)
	s.resetPoll, s.resetWait = 5*time.Millisecond, 300*time.Millisecond
	s.cfg.CoordinatorResetTokens = 150000
	r := &resetRig{t: t, s: s, fh: fh, proj: filepath.Join(cfg, "projects")}
	lines := []string{usageLine(tokens)}
	if withHook {
		lines = append(lines, `{"type":"attachment","text":"`+hookMarker+` the hook"}`)
	}
	r.transcript("s1", lines...)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents[0].CWD = resetCwd
		f.agents[0].AgentSession = &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Source: "herdr:claude", Value: "s1"}
		f.hook = func(method string, params map[string]any) (any, *herdr.Error, bool) {
			switch method {
			case "agent.prompt":
				if params["text"] == "/clear" {
					for i := range f.agents {
						if f.agents[i].PaneID == params["target"] {
							f.agents[i].AgentSession = &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Source: "herdr:claude", Value: "s2"}
							f.agents[i].Name = ""
						}
					}
					if landDigest {
						r.transcript("s2", `{"type":"attachment","text":"`+digest.Marker+`"}`)
					} else {
						r.transcript("s2", `{"type":"user"}`)
					}
					return map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "w1:p1"}}, nil, true
				}
			case "agent.rename":
				for i := range f.agents {
					if f.agents[i].PaneID == params["target"] {
						f.agents[i].Name, _ = params["name"].(string)
					}
				}
				return map[string]any{"type": "ok"}, nil, true
			}
			return nil, nil, false
		}
	})
	r.waitCache(func(a herdr.AgentInfo) bool { return a.AgentSession != nil })
	if err := s.recordRole("desk/w1:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *resetRig) clears() int {
	n := 0
	for _, p := range sentPrompts(r.fh) {
		if strings.HasSuffix(p, ": /clear") {
			n++
		}
	}
	return n
}

// waitCache refreshes the manager until the coordinator (w1:p1) in its
// cache satisfies ok.
func (r *resetRig) waitCache(ok func(herdr.AgentInfo) bool) {
	r.t.Helper()
	r.s.manager.Refresh()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, a := range r.s.manager.Agents() {
			if a.PaneID == "w1:p1" && ok(a) {
				return
			}
		}
	}
	r.t.Fatal("the manager's cache never showed the change")
}

func (r *resetRig) setStatus(st herdr.AgentStatus) {
	r.fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Status = st })
	r.waitCache(func(a herdr.AgentInfo) bool { return a.Status == st })
}

func TestResetLocalHappyPath(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	status, payload := r.s.resetLocal(context.Background(), "w1:p1", resetBody{MinTokens: 150000})
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, payload)
	}
	m := payload.(map[string]any)
	if m["session_before"] != "s1" || m["session_after"] != "s2" || m["renamed"] != true || m["digest_landed"] != true || m["tokens"] != 160000 {
		t.Fatalf("answer = %v", m)
	}
	if r.clears() != 1 {
		t.Fatalf("/clear sent %d times", r.clears())
	}
	// herdr dropped the name; the reset gave it back.
	if c := r.fh.call("agent.rename"); c == nil || c.Params["name"] != "coordinator" || c.Params["target"] != "w1:p1" {
		t.Fatalf("rename = %+v", c)
	}
}

func TestResetLocalRefusals(t *testing.T) {
	cases := []struct {
		name   string
		tokens int
		hook   bool
		setup  func(r *resetRig)
		body   resetBody
		status int
		code   string
	}{
		{"working", 160000, true, func(r *resetRig) { r.setStatus(herdr.StatusWorking) }, resetBody{}, 409, "agent_busy"},
		{"blocked", 160000, true, func(r *resetRig) { r.setStatus(herdr.StatusBlocked) }, resetBody{}, 409, "agent_busy"},
		{"below the threshold", 90000, true, nil, resetBody{MinTokens: 150000}, 409, "below_threshold"},
		{"no hook in the session", 160000, false, nil, resetBody{}, 409, "hook_not_installed"},
		{"a dialog is open", 160000, true, func(r *resetRig) { showScreen(r.fh, "w1:p1", claudePermScreen) }, resetBody{}, 409, "dialog_open"},
		{"not claude", 160000, true, func(r *resetRig) {
			r.fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Agent = "pi" })
			r.waitCache(func(a herdr.AgentInfo) bool { return a.Agent == "pi" })
		}, resetBody{}, 409, "reset_unsupported"},
	}
	for _, c := range cases {
		r := newResetRig(t, c.tokens, c.hook, true)
		if c.setup != nil {
			c.setup(r)
		}
		status, payload := r.s.resetLocal(context.Background(), "w1:p1", c.body)
		b, _ := json.Marshal(payload)
		if status != c.status || !strings.Contains(string(b), c.code) {
			t.Errorf("%s: %d %s, want %d %s", c.name, status, b, c.status, c.code)
		}
		if r.clears() != 0 {
			t.Errorf("%s: /clear was sent", c.name)
		}
	}
}

func TestResetLocalClearNotConfirmed(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	// The pane swallows /clear: the session never changes.
	r.fh.set(func(f *fakeLaunchHerdr) {
		inner := f.hook
		f.hook = func(method string, params map[string]any) (any, *herdr.Error, bool) {
			if method == "agent.prompt" && params["text"] == "/clear" {
				return map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": "w1:p1"}}, nil, true
			}
			return inner(method, params)
		}
	})
	status, payload := r.s.resetLocal(context.Background(), "w1:p1", resetBody{})
	b, _ := json.Marshal(payload)
	if status != http.StatusGatewayTimeout || !strings.Contains(string(b), "clear_not_confirmed") {
		t.Fatalf("%d %s", status, b)
	}
}

func TestResetLocalDigestMissing(t *testing.T) {
	r := newResetRig(t, 160000, true, false)
	status, payload := r.s.resetLocal(context.Background(), "w1:p1", resetBody{})
	m, _ := payload.(map[string]any)
	if status != 200 || m["digest_landed"] != false || m["session_after"] != "s2" {
		t.Fatalf("%d %v", status, payload)
	}
}

// ---- the hub's guards ----

func TestFreshStartWaitsForQueuedMessages(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	a := r.s.coordinatorAgents()
	if len(a) != 1 {
		t.Fatalf("coordinators = %v", a)
	}
	cases := map[string]func(){
		"a queued prompt": func() {
			if _, _, err := r.s.pq.add(QueuedPrompt{Agent: "desk/w1:p1", Name: "coordinator", Text: "hi", From: "x", At: time.Now(), State: QueuePending}); err != nil {
				t.Fatal(err)
			}
		},
		"an undelivered worker notice": func() {
			e, _ := r.s.ledger.Add(ledger.Entry{Host: "desk", Repo: "/r", RepoName: "r", Branch: "b", Kind: "pi", AgentID: "desk/w2:p1", Status: ledger.Running,
				ReplyTo: &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}})
			if _, _, err := r.s.ledger.Report("desk/w2:p1", "done", time.Now()); err != nil {
				t.Fatal(err)
			}
			_ = e
		},
	}
	for name, setup := range cases {
		setup()
		status, out := r.s.freshStart(context.Background(), a[0], "manual", "me", 0)
		if status != http.StatusConflict || out["code"] != "queued" {
			t.Errorf("%s: %d %v", name, status, out)
		}
		if r.clears() != 0 {
			t.Errorf("%s: /clear was sent", name)
		}
		// Clear the queue for the next case.
		for _, p := range r.s.pq.list("", false) {
			r.s.pq.settle(p.ID, QueueCancelled, false)
		}
		for _, e := range r.s.ledger.PendingCallbacks() {
			r.s.ledger.CallbackDone(e.ID, e.Callback.At, ledger.CallbackDelivered, "x", time.Now())
		}
	}
	status, out := r.s.freshStart(context.Background(), a[0], "manual", "me", 0)
	if status != 200 || r.clears() != 1 {
		t.Fatalf("with nothing queued: %d %v, clears %d", status, out, r.clears())
	}
	// Recorded: the store, the audit trail.
	f := r.s.resets.get()
	if f.Last == nil || f.Last.Trigger != "manual" || f.Last.SessionAfter != "s2" || !f.Last.DigestLanded || f.Last.By != "me" {
		t.Fatalf("last = %+v", f.Last)
	}
	var audited bool
	for _, e := range r.s.ledger.AuditLog("") {
		audited = audited || (e.Action == "coordinator.reset" && e.OK && strings.Contains(e.Detail, "coordinator"))
	}
	if !audited {
		t.Fatal("no coordinator.reset audit event")
	}
}

func TestFreshStartOneAtATime(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	a := r.s.coordinatorAgents()[0]
	if !r.s.resets.begin(a.ID) {
		t.Fatal("begin")
	}
	status, out := r.s.freshStart(context.Background(), a, "manual", "me", 0)
	if status != 409 || out["code"] != "reset_running" {
		t.Fatalf("%d %v", status, out)
	}
}

// ---- the briefing tick ----

func (r *resetRig) publishBrief(at time.Time) {
	if err := r.s.brief.set(Brief{Markdown: "# b", By: "coordinator", At: at}); err != nil {
		panic(err)
	}
}

func TestResetTickAtTheBriefing(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	now := time.Now()
	r.publishBrief(now.Add(-time.Minute))

	// The coordinator is still working on its briefing: due, not run.
	r.setStatus(herdr.StatusWorking)
	r.s.resetTick(context.Background(), now)
	f := r.s.resets.get()
	d, ok := f.Due["desk/w1:p1"]
	if !ok || d.Tokens != 160000 || !strings.Contains(d.Why, "working") {
		t.Fatalf("due = %+v (%v)", d, f.Due)
	}
	if r.clears() != 0 {
		t.Fatal("reset a working coordinator")
	}

	// Idle, but the hub holds a prompt for it: still waits.
	r.setStatus(herdr.StatusIdle)
	r.s.pq.add(QueuedPrompt{Agent: "desk/w1:p1", Name: "coordinator", Text: "hi", From: "x", At: now, State: QueuePending})
	r.s.resetTick(context.Background(), now.Add(time.Minute))
	if r.clears() != 0 {
		t.Fatal("reset with a prompt queued")
	}
	for _, p := range r.s.pq.list("", false) {
		r.s.pq.settle(p.ID, QueueCancelled, false)
	}

	// Idle and nothing queued: it runs, once.
	r.s.resetTick(context.Background(), now.Add(2*time.Minute))
	if r.clears() != 1 {
		t.Fatalf("clears = %d", r.clears())
	}
	f = r.s.resets.get()
	if len(f.Due) != 0 || f.Last == nil || f.Last.Trigger != "briefing" || f.Last.By != "the hub" {
		t.Fatalf("after: due=%v last=%+v", f.Due, f.Last)
	}
	r.s.resetTick(context.Background(), now.Add(3*time.Minute))
	if r.clears() != 1 {
		t.Fatal("reset twice for one briefing")
	}
}

func TestResetTickUnderThresholdOrOff(t *testing.T) {
	r := newResetRig(t, 90000, true, true)
	r.publishBrief(time.Now())
	r.s.resetTick(context.Background(), time.Now())
	if f := r.s.resets.get(); len(f.Due) != 0 || r.clears() != 0 {
		t.Fatalf("90k is under 150k: due=%v", f.Due)
	}
	// The same briefing is not looked at again; a newer one is.
	r.fh.set(func(f *fakeLaunchHerdr) {})
	r.transcript("s1", usageLine(170000), `{"type":"attachment","text":"`+hookMarker+`"}`)
	r.s.resetTick(context.Background(), time.Now())
	if len(r.s.resets.get().Due) != 0 {
		t.Fatal("looked twice at one briefing")
	}
	r.publishBrief(time.Now().Add(time.Second))
	r.s.cfg.CoordinatorResetTokens = 0
	r.s.resetTick(context.Background(), time.Now())
	if r.clears() != 0 || len(r.s.resets.get().Due) != 0 {
		t.Fatal("threshold 0 is off")
	}
}

func TestResetTickGivesUpWithoutTheHook(t *testing.T) {
	r := newResetRig(t, 160000, false, true)
	r.publishBrief(time.Now())
	r.s.resetTick(context.Background(), time.Now())
	if r.clears() != 0 {
		t.Fatal("cleared a session that has no hook")
	}
	if f := r.s.resets.get(); len(f.Due) != 0 {
		t.Fatalf("a refusal that waiting cannot fix stays due: %v", f.Due)
	}
}

func TestResetTickMinGapAndTTL(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	now := time.Now()
	r.s.resets.update(func(f *resetFile) {
		f.Last = &ResetRecord{At: now.Add(-5 * time.Minute), AgentID: "desk/w1:p1"}
	})
	r.publishBrief(now)
	r.s.resetTick(context.Background(), now)
	if r.clears() != 0 || len(r.s.resets.get().Due) != 1 {
		t.Fatalf("a reset 5 minutes ago holds the next one back (clears %d)", r.clears())
	}
	r.s.resetTick(context.Background(), now.Add(resetDueTTL+time.Minute))
	if len(r.s.resets.get().Due) != 0 {
		t.Fatal("a due mark expires")
	}
}

// ---- the routes and who may call them ----

func TestCoordinatorResetAuth(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	s := r.s
	// An agent (the coordinator itself included) may not clear anything,
	// nor call the host-side routes.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/coordinator/reset", `{}`},
		{"POST", "/api/agents/w1:p1/reset", `{}`},
		{"GET", "/api/agents/w1:p1/context", ""},
	} {
		w := do(s, c.method, c.path, agentHdr("w1:p1"), c.body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "agent_forbidden") {
			t.Errorf("agent %s %s = %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	// No credentials at all: refused.
	if w := do(s, "POST", "/api/coordinator/reset", map[string]string{}, `{}`); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous reset = %d", w.Code)
	}
	if r.clears() != 0 {
		t.Fatal("a refused call cleared the coordinator")
	}
	// An agent may read the state (the digest's "since"), without contexts.
	w := do(s, "GET", "/api/coordinator?refresh=1", agentHdr("w1:p1"), "")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"tokens"`) {
		t.Errorf("agent state = %d %s", w.Code, w.Body)
	}
	// The user reads contexts with refresh.
	w = do(s, "GET", "/api/coordinator?refresh=1", userHdr, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tokens":160000`) || !strings.Contains(w.Body.String(), `"hook_active":true`) {
		t.Errorf("user state = %d %s", w.Code, w.Body)
	}
	// The user clears it.
	w = do(s, "POST", "/api/coordinator/reset", userHdr, `{}`)
	if w.Code != 200 || r.clears() != 1 {
		t.Fatalf("user reset = %d %s, clears %d", w.Code, w.Body, r.clears())
	}
	if w = do(s, "POST", "/api/coordinator/reset", userHdr, `{"agent_id":"desk/nope"}`); w.Code != 404 {
		t.Errorf("unknown coordinator = %d", w.Code)
	}
}

// A non-coordinator is never cleared by the route.
func TestCoordinatorResetOnlyCoordinators(t *testing.T) {
	r := newResetRig(t, 160000, true, true)
	w := do(r.s, "POST", "/api/coordinator/reset", userHdr, `{"agent_id":"desk/w2:p1"}`)
	if w.Code != 404 || r.clears() != 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// A coordinator on a peer (the usual setup: the hub is another machine):
// the hub reads its context and sends the reset through the peer's host-side
// routes, and records the result itself.
func TestFreshStartOnAPeer(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	var resetBody string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: agents\ndata: "+`{"type":"agents","agents":[{"pane_id":"wC:p1","name":"coordinator","agent":"claude","agent_status":"idle"}]}`+"\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if strings.HasPrefix(r.URL.Path, "/api/agents/") {
			hits = append(hits, r.Method+" "+r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/reset") {
			resetBody = string(b)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/context"):
			io.WriteString(w, `{"tokens":171000,"session":"s1","status":"idle","hook_active":true}`)
		case strings.HasSuffix(r.URL.Path, "/reset"):
			io.WriteString(w, `{"agent":{"pane_id":"wC:p1","name":"coordinator"},"tokens":171000,"session_before":"s1","session_after":"s2","renamed":true,"digest_landed":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	pu, _ := url.Parse(peer.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fl.Run(ctx)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, StateDir: t.TempDir(), AgentToken: testToken, CoordinatorResetTokens: 150000}, nil, nil)
	for deadline := time.Now().Add(5 * time.Second); len(s.fleet.Agents()) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the peer's agent never appeared")
		}
	}
	if err := s.recordRole("kube/wC:p1", "coordinator", RoleCoordinator, "me"); err != nil {
		t.Fatal(err)
	}
	// At a briefing: the context is read over there, the reset is due and runs.
	s.brief.set(Brief{Markdown: "# b", By: "coordinator", At: time.Now()})
	s.resetTick(ctx, time.Now())
	mu.Lock()
	got := strings.Join(hits, ",")
	rb := resetBody
	mu.Unlock()
	if got != "GET /api/agents/wC:p1/context,POST /api/agents/wC:p1/reset" {
		t.Fatalf("the peer saw %s", got)
	}
	if !strings.Contains(rb, `"min_tokens":150000`) || !strings.Contains(rb, `"trigger":"briefing"`) {
		t.Fatalf("reset body = %s", rb)
	}
	f := s.resets.get()
	if f.Last == nil || f.Last.AgentID != "kube/wC:p1" || f.Last.Tokens != 171000 || f.Last.SessionAfter != "s2" || len(f.Due) != 0 {
		t.Fatalf("recorded %+v due %v", f.Last, f.Due)
	}
}
