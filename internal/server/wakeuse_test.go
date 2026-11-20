package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// useRig is a hub whose worker demo1 is stopped in the cloud and down for the
// hub until the fake provisioner's start has run; demo1's own answers are
// canned and counted. agentHere says whether its session w9:p1 exists (the
// power-off may have ended it).
type useRig struct {
	s    *Server
	wk   *fakeWorkerDir
	prov *provPeer
	hdr  map[string]string

	mu   sync.Mutex
	hits map[string]int
	gone bool // demo1 has no w9:p1 any more
}

func newUseRig(t *testing.T) *useRig {
	t.Helper()
	r := &useRig{hits: map[string]int{}}
	s, prov, wk, hdr, _ := stoppedHubW(t, r.serve)
	r.mu.Lock()
	r.s, r.wk, r.prov, r.hdr = s, wk, prov, hdr
	r.mu.Unlock()
	real := func(name string) string {
		for _, h := range s.fleet.Hosts() {
			if h.Name == name {
				return h.Status
			}
		}
		return ""
	}
	s.peerStatusHook = func(name string) string {
		if name == "demo1" && wk.runs("start demo1") == 0 {
			return "down"
		}
		return real(name)
	}
	var h hostsResp
	getJSON(t, s, "/api/provision/hosts?refresh=1", userHdr, &h)
	t.Cleanup(s.waitBackground)
	return r
}

func (r *useRig) hit(k string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[k]
}

func (r *useRig) starts() int { return r.startRuns() }

// startRuns is how often the fake tool was asked to start demo1 (the rig's own
// hook reads it while the rig is still being built).
func (r *useRig) startRuns() int {
	r.mu.Lock()
	wk := r.wk
	r.mu.Unlock()
	if wk == nil {
		return 0
	}
	return wk.runs("start demo1")
}

func (r *useRig) coordinator(t *testing.T) map[string]string {
	t.Helper()
	if err := r.s.recordRole("desk/w1:p1", "", RoleCoordinator, "test"); err != nil {
		t.Fatal(err)
	}
	return agentHdr("w1:p1")
}

const promptURL = "/api/hosts/demo1/agents/w9:p1/prompt"

func TestPromptToAStoppedWorkerWakesItOnceThenSucceeds(t *testing.T) {
	r := newUseRig(t)
	w := do(r.s, "POST", promptURL, userHdr, `{"text":"hello"}`)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"host_waking"`) || !strings.Contains(w.Body.String(), `"demo1"`) {
		t.Fatalf("prompt = %d %s", w.Code, w.Body)
	}
	if r.hit("POST /api/agents/w9:p1/prompt") != 0 {
		t.Fatal("the prompt went to a stopped host")
	}
	// the app repeats it while the wake runs: joined, not started twice
	do(r.s, "POST", promptURL, userHdr, `{"text":"hello"}`)
	waitWake(t, r.s, "demo1", wakeUp)
	if r.starts() != 1 {
		t.Fatalf("starts = %d, want 1: %v", r.starts(), r.wk.argv())
	}
	w = do(r.s, "POST", promptURL, userHdr, `{"text":"hello"}`)
	if w.Code != 200 || r.hit("POST /api/agents/w9:p1/prompt") != 1 {
		t.Fatalf("after the wake = %d %s (prompts %d)", w.Code, w.Body, r.hit("POST /api/agents/w9:p1/prompt"))
	}
	if r.starts() != 1 {
		t.Errorf("starts = %d after the prompt", r.starts())
	}
}

func TestLaunchOnAStoppedWorkerWakesItThenLaunches(t *testing.T) {
	r := newUseRig(t)
	w := do(r.s, "POST", "/api/hosts/demo1/launch", userHdr, `{"kind":"claude","dir":"/home/k/code/proj"}`)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"host_waking"`) {
		t.Fatalf("launch = %d %s", w.Code, w.Body)
	}
	waitWake(t, r.s, "demo1", wakeUp)
	w = do(r.s, "POST", "/api/hosts/demo1/launch", userHdr, `{"kind":"claude","dir":"/home/k/code/proj"}`)
	if w.Code != 200 || r.hit("POST /api/launch") != 1 || r.starts() != 1 {
		t.Fatalf("launch after = %d %s (launches %d, starts %d)", w.Code, w.Body, r.hit("POST /api/launch"), r.starts())
	}
}

func TestResumeOnAStoppedWorkerWakesItThenResumes(t *testing.T) {
	r := newUseRig(t)
	w := do(r.s, "POST", "/api/hosts/demo1/resume", userHdr, `{"id":"pk_1"}`)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"host_waking"`) {
		t.Fatalf("resume = %d %s", w.Code, w.Body)
	}
	waitWake(t, r.s, "demo1", wakeUp)
	w = do(r.s, "POST", "/api/hosts/demo1/resume", userHdr, `{"id":"pk_1"}`)
	if w.Code != 200 || r.hit("POST /api/resume") != 1 || r.starts() != 1 {
		t.Fatalf("resume after = %d %s", w.Code, w.Body)
	}
}

func TestHandoffToAStoppedWorkerWakesItAndWaitsInside(t *testing.T) {
	r := newUseRig(t)
	body := `{"host":"demo1","repo":"/home/k/code/proj","branch":"feat/x","kind":"claude","brief":"Fix X.","from":"coordinator"}`
	w := do(r.s, "POST", "/api/handoffs", userHdr, body)
	if w.Code != 200 || r.hit("POST /api/worker") != 1 || r.starts() != 1 {
		t.Fatalf("handoff = %d %s (workers %d, starts %d)", w.Code, w.Body, r.hit("POST /api/worker"), r.starts())
	}
	if v, _ := r.s.wake.get("demo1"); v.State != wakeUp || v.Why != "a handoff" {
		t.Errorf("wake: %+v", v)
	}
}

func TestOpeningASessionOnAStoppedWorkerDoesNotWakeIt(t *testing.T) {
	r := newUseRig(t)
	for _, p := range []string{"/transcript", "/output", "/activity"} {
		do(r.s, "GET", "/api/hosts/demo1/agents/w9:p1"+p, userHdr, "")
	}
	do(r.s, "GET", "/api/hosts/demo1/parked", userHdr, "")
	if r.starts() != 0 {
		t.Fatalf("opening ran the start tool: %v", r.wk.argv())
	}
	if r.hit("GET /api/agents/w9:p1/transcript") != 1 {
		t.Error("opening did not read through to the host")
	}
	if _, ok := r.s.wake.get("demo1"); ok {
		t.Error("opening created a wake")
	}
	// the Start button is what wakes it
	w := do(r.s, "POST", "/api/provision/hosts/demo1/start", r.hdr, `{}`)
	if w.Code != 202 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	waitWake(t, r.s, "demo1", wakeUp)
}

func TestAnAgentIsRefusedAndTheCoordinatorMayWake(t *testing.T) {
	r := newUseRig(t)
	// an agent's own prompt, queued prompt and handoff
	w := do(r.s, "POST", promptURL, agentHdr("w2:p1"), `{"text":"hi"}`)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "demo1 is stopped; ask the coordinator") || !strings.Contains(w.Body.String(), "agent_forbidden") {
		t.Fatalf("agent prompt = %d %s", w.Code, w.Body)
	}
	w = do(r.s, "POST", "/api/queue", agentHdr("w2:p1"), `{"agent":"demo1/w9:p1","text":"hi"}`)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "ask the coordinator") {
		t.Fatalf("agent queue = %d %s", w.Code, w.Body)
	}
	body := `{"host":"demo1","repo":"/home/k/code/proj","branch":"feat/x","kind":"claude","brief":"Fix X."}`
	w = do(r.s, "POST", "/api/handoffs", agentHdr("w2:p1"), body)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "ask the coordinator") {
		t.Fatalf("agent handoff = %d %s", w.Code, w.Body)
	}
	if r.starts() != 0 {
		t.Fatalf("an agent woke a worker: %v", r.wk.argv())
	}
	if n := strings.Count(do(r.s, "GET", "/api/ledger", userHdr, "").Body.String(), "feat/x"); n != 0 {
		t.Errorf("the refused handoff left an entry")
	}
	// the coordinator may
	coord := r.coordinator(t)
	w = do(r.s, "POST", promptURL, coord, `{"text":"hi"}`)
	if w.Code != 202 || !strings.Contains(w.Body.String(), "host_waking") {
		t.Fatalf("coordinator prompt = %d %s", w.Code, w.Body)
	}
	waitWake(t, r.s, "demo1", wakeUp)
	if v, _ := r.s.wake.get("demo1"); !strings.Contains(v.By, "coordinator") && !strings.Contains(v.By, "w1:p1") {
		t.Errorf("by = %q", v.By)
	}
	if r.starts() != 1 {
		t.Errorf("starts = %d", r.starts())
	}
}

func TestWakeOnUseObeysTheHourlyLimitAndTheBudgetCap(t *testing.T) {
	r := newUseRig(t)
	for i := 0; i < wakeHostPerHour; i++ {
		r.s.wake.noteAuto("demo1")
	}
	w := do(r.s, "POST", promptURL, userHdr, `{"text":"hi"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "wake_limit") || !strings.Contains(w.Body.String(), "tap Start") {
		t.Fatalf("rate limited = %d %s", w.Code, w.Body)
	}
	// the budget cap wins, rate limit or not
	r.prov.peer.phosts.MarkStopped("demo1", true)
	tick(r.s)
	w = do(r.s, "POST", promptURL, userHdr, `{"text":"hi"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "needs_confirm") || !strings.Contains(w.Body.String(), "budget cap") {
		t.Fatalf("cap = %d %s", w.Code, w.Body)
	}
	if r.starts() != 0 {
		t.Fatalf("a refused wake ran the tool: %v", r.wk.argv())
	}
	// the Start tap asks to confirm the cap, and starts once confirmed
	if w := do(r.s, "POST", "/api/provision/hosts/demo1/start", r.hdr, `{}`); w.Code != 409 || !strings.Contains(w.Body.String(), "needs_confirm") {
		t.Fatalf("tap without confirm = %d %s", w.Code, w.Body)
	}
	if w := do(r.s, "POST", "/api/provision/hosts/demo1/start", r.hdr, `{"confirm_cap":true}`); w.Code != 202 {
		t.Fatalf("tap with confirm = %d %s", w.Code, w.Body)
	}
	waitWake(t, r.s, "demo1", wakeUp)
}

func TestAQueuedPromptWakesTheHostAndIsDeliveredAfterItJoins(t *testing.T) {
	r := newUseRig(t)
	var delivered []string
	var dmu sync.Mutex
	r.s.pq.send = func(_ context.Context, a herdr.AgentInfo, text string) error {
		dmu.Lock()
		delivered = append(delivered, a.ID+": "+text)
		dmu.Unlock()
		return nil
	}
	w := do(r.s, "POST", "/api/queue", userHdr, `{"agent":"demo1/w9:p1","text":"when you are up"}`)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"waking"`) {
		t.Fatalf("queue = %d %s", w.Code, w.Body)
	}
	// a second one while it starts does not start it again
	do(r.s, "POST", "/api/queue", userHdr, `{"agent":"demo1/w9:p1","text":"and this"}`)
	waitWake(t, r.s, "demo1", wakeUp)
	waitUntil(t, "delivery", func() bool {
		r.s.deliverQueue(r.s.fleet.Agents())
		dmu.Lock()
		defer dmu.Unlock()
		return len(delivered) > 0
	})
	if r.starts() != 1 {
		t.Errorf("starts = %d", r.starts())
	}
	dmu.Lock()
	defer dmu.Unlock()
	if !strings.Contains(delivered[0], "demo1/w9:p1") || !strings.Contains(delivered[0], "when you are up") {
		t.Errorf("delivered %v", delivered)
	}
}

func TestASessionThatDidNotSurviveThePowerOffIsSaidSo(t *testing.T) {
	r := newUseRig(t)
	r.s.wake.sessionGrace = 100 * time.Millisecond
	w := do(r.s, "POST", "/api/queue", userHdr, `{"agent":"demo1/w9:p1","text":"hello?"}`)
	if w.Code != 202 {
		t.Fatalf("queue = %d %s", w.Code, w.Body)
	}
	var q struct {
		Queued QueuedPrompt `json:"queued"`
	}
	json.Unmarshal(w.Body.Bytes(), &q)
	// the power-off ended the session: demo1 comes back without it
	r.mu.Lock()
	r.gone = true
	r.mu.Unlock()
	waitWake(t, r.s, "demo1", wakeUp)
	waitUntil(t, "the queued prompt given up", func() bool {
		for _, it := range r.s.pq.list("", true) {
			if it.ID == q.Queued.ID {
				return it.State == QueueGone
			}
		}
		return false
	})
	// a prompt now says it plainly, not a bare 404
	waitUntil(t, "the session gone from the fleet", func() bool {
		for _, a := range r.s.fleet.Agents() {
			if a.ID == "demo1/w9:p1" {
				return false
			}
		}
		return true
	})
	w = do(r.s, "POST", promptURL, userHdr, `{"text":"hello?"}`)
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "session_gone") || !strings.Contains(w.Body.String(), "powered off") {
		t.Fatalf("prompt to the gone session = %d %s", w.Code, w.Body)
	}
}

const useAgent = `{"id":"demo1/w9:p1","host":"demo1","pane_id":"w9:p1","name":"builder","agent":"claude","agent_status":"idle"}`

// serve is demo1's own API, canned.
func (r *useRig) serve(worker *provPeer) {
	worker.setHook(func(w http.ResponseWriter, q *http.Request) bool {
		if strings.HasPrefix(q.URL.Path, "/api/provisioner/") {
			return false
		}
		body, _ := io.ReadAll(q.Body)
		r.mu.Lock()
		r.hits[q.Method+" "+q.URL.Path]++
		gone := r.gone
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case q.URL.Path == "/api/events":
			// a stopped worker's sessions are gone from the fleet; once it has been
			// started they are back, unless the power-off ended them
			w.Header().Set("Content-Type", "text/event-stream")
			last := ""
			for {
				r.mu.Lock()
				g := r.gone
				r.mu.Unlock()
				cur := "[]"
				if r.startRuns() > 0 && !g {
					cur = "[" + useAgent + "]"
				}
				if cur != last {
					fmt.Fprintf(w, "event: agents\ndata: {\"type\":\"agents\",\"agents\":%s}\n\n", cur)
					w.(http.Flusher).Flush()
					last = cur
				}
				select {
				case <-q.Context().Done():
					return true
				case <-time.After(15 * time.Millisecond):
				}
			}
		case strings.HasSuffix(q.URL.Path, "/prompt"):
			if gone {
				http.Error(w, "unknown agent", http.StatusNotFound)
				return true
			}
			fmt.Fprint(w, `{"agent":`+useAgent+`}`)
		case q.Method == http.MethodGet: // opening a session reads: the host's own answer
			fmt.Fprint(w, `{"messages":[],"total":0}`)
		case q.URL.Path == "/api/resume":
			fmt.Fprint(w, `{"agent":`+useAgent+`}`)
		case q.URL.Path == "/api/launch":
			fmt.Fprint(w, `{"agent":`+useAgent+`}`)
		case q.URL.Path == "/api/worker":
			var wb workerBody
			json.Unmarshal(body, &wb)
			fmt.Fprintf(w, `{"agent":%s,"workspace_id":"w9","worktree":"/home/k/.worktrees/proj/x","branch":%q,"base":"main","repo":"/home/k/code/proj","reused":false}`, useAgent, wb.Branch)
		default:
			return false
		}
		return true
	})
}
