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

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

func TestHandoffLocal(t *testing.T) {
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false}}
	fh.set(func(f *fakeLaunchHerdr) { f.hook = worktreeHook(t, &wts, nil) })
	stateDir := t.TempDir()
	s := New(Config{ReadLines: 100, HostName: "desk", LaunchRoots: []string{root}, WorktreeRoot: wtRoot, StateDir: stateDir}, client, nil)
	s.findExec = func(name string) bool { return name == "pi" }

	w := do(s, "POST", "/api/handoffs", map[string]string{"Tailscale-User-Login": "me@example.com"},
		`{"host":"desk","repo":"`+repo+`","branch":"feat/a","kind":"pi","brief":"Add the thing."}`)
	var r struct {
		Entry  ledger.Entry `json:"entry"`
		Worker workerResult `json:"worker"`
	}
	json.Unmarshal(w.Body.Bytes(), &r)
	wt := filepath.Join(wtRoot, "proj", "feat-a")
	if w.Code != 200 || r.Entry.Status != ledger.Running || r.Entry.AgentID != "desk/w9:p1" || r.Entry.Worktree != wt ||
		r.Entry.Base != "main" || r.Entry.From != "me@example.com" || r.Entry.RepoName != "proj" || r.Entry.Kind != "pi" {
		t.Fatalf("handoff = %d %s", w.Code, w.Body)
	}
	text := fh.call("agent.prompt").Params["text"].(string)
	for _, want := range []string{"Add the thing.", "branch `feat/a`", "started from `main`", wt, "commit your work", "Do not use `tmux`", "## Summary", "## Changes", "## Verification", "## Follow-ups", "handed this task by me@example.com"} {
		if !strings.Contains(text, want) {
			t.Errorf("brief lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "{{") {
		t.Errorf("unfilled placeholder:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ledger.json")); err != nil {
		t.Fatal(err)
	}

	// A template in the state directory replaces the built-in one; a
	// failed start is recorded as failed.
	os.WriteFile(filepath.Join(stateDir, "handoff-template.md"), []byte("Do {{brief}} on {{branch}} in {{worktree}}."), 0o600)
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	w = do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/b","kind":"pi","brief":"B"}`)
	if w.Code != 200 {
		t.Fatalf("second = %d %s", w.Code, w.Body)
	}
	if got := fh.call("agent.prompt").Params["text"]; got != "Do B on feat/b in "+filepath.Join(wtRoot, "proj", "feat-b")+"." {
		t.Fatalf("templated brief = %q", got)
	}
	w = do(s, "POST", "/api/handoffs", nil, `{"repo":"`+repo+`","branch":"feat/c","base":"nope","kind":"pi","brief":"C"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatalf("failed = %d %s", w.Code, w.Body)
	}
	for body, code := range map[string]int{
		`{"repo":"` + repo + `","branch":"x","brief":"  "}`:              http.StatusBadRequest,
		`{"repo":"` + repo + `","branch":"a..b","brief":"x"}`:            http.StatusBadRequest,
		`{"repo":"proj","branch":"x","brief":"x"}`:                       http.StatusBadRequest,
		`{"host":"evil","repo":"` + repo + `","branch":"x","brief":"x"}`: http.StatusNotFound,
	} {
		if w := do(s, "POST", "/api/handoffs", nil, body); w.Code != code {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}

	// The ledger lists all three, the failure with its error.
	w = do(s, "GET", "/api/ledger", nil, "")
	var l struct {
		Entries []entryView `json:"entries"`
	}
	json.Unmarshal(w.Body.Bytes(), &l)
	if w.Code != 200 || len(l.Entries) != 3 || l.Entries[2].Status != ledger.Failed || !strings.Contains(l.Entries[2].Error, "bad_base") {
		t.Fatalf("ledger = %d %s", w.Code, w.Body)
	}
}

func TestLedgerDisabled(t *testing.T) {
	s := New(Config{HostName: "desk"}, nil, nil)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/ledger", ""}, {"POST", "/api/handoffs", `{}`}, {"POST", "/api/marks", `{"agent_id":"x"}`},
		{"GET", "/api/removals", ""}, {"POST", "/api/removals/confirm", `{"ids":["x"]}`},
	} {
		if w := do(s, c.method, c.path, nil, c.body); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: %d", c.method, c.path, w.Code)
		}
	}
}

// workerPeer is a stand-in peer for the hub's worker calls.
type workerPeer struct {
	mu     sync.Mutex
	hits   []string
	worker []byte
	remove []byte
}

func (p *workerPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.hits = append(p.hits, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/events":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: agents\ndata: {\"type\":\"agents\",\"agents\":[]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case "/api/worker":
		p.mu.Lock()
		p.worker = body
		p.mu.Unlock()
		fmt.Fprint(w, `{"agent":{"id":"kube/wS:p9","host":"kube","pane_id":"wS:p9","name":"feat-x","agent":"claude","agent_status":"idle"},"workspace_id":"wS","worktree":"/home/k/.worktrees/proj/feat-x","branch":"feat/x","base":"main","repo":"/home/k/code/proj","reused":false}`)
	case "/api/checks":
		fmt.Fprintf(w, `{"path":%q,"exists":true,"primary":false,"branch":"feat/x","dirty":false,"dirty_files":0,"unmerged_commits":0,"base_used":"main"}`, r.URL.Query().Get("path"))
	case "/api/remove":
		p.mu.Lock()
		p.remove = body
		p.mu.Unlock()
		fmt.Fprint(w, `{"closed":"workspace","pane_id":"wS:p9","workspace_id":"wS","worktree":"/home/k/.worktrees/proj/feat-x","worktree_removed":true}`)
	case "/api/agents/wS:p9/transcript":
		fmt.Fprint(w, `{"messages":[{"id":"1","kind":"user","text":"brief"},{"id":"2","kind":"assistant","text":"## Summary\nDone it."}],"total":2}`)
	default:
		http.NotFound(w, r)
	}
}

func (p *workerPeer) saw(prefix string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, h := range p.hits {
		if strings.HasPrefix(h, prefix) {
			out = append(out, h)
		}
	}
	return out
}

func TestHandoffViaPeer(t *testing.T) {
	peer := &workerPeer{}
	ps := httptest.NewServer(peer)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	s := New(Config{ReadLines: 100, Fleet: fl, StateDir: t.TempDir()}, nil, nil)

	w := do(s, "POST", "/api/handoffs", nil, `{"host":"kube","repo":"/home/k/code/proj","branch":"feat/x","kind":"claude","brief":"Fix X.","from":"coordinator"}`)
	var r struct {
		Entry ledger.Entry `json:"entry"`
	}
	json.Unmarshal(w.Body.Bytes(), &r)
	if w.Code != 200 || r.Entry.Host != "kube" || r.Entry.AgentID != "kube/wS:p9" || r.Entry.From != "coordinator" || r.Entry.Worktree != "/home/k/.worktrees/proj/feat-x" {
		t.Fatalf("handoff = %d %s", w.Code, w.Body)
	}
	var wb workerBody
	json.Unmarshal(peer.worker, &wb)
	if wb.Repo != "/home/k/code/proj" || wb.Branch != "feat/x" || wb.Kind != "claude" ||
		!strings.Contains(wb.Prompt, "Fix X.") || !strings.Contains(wb.Prompt, "{{worktree}}") || !strings.Contains(wb.Prompt, "{{base}}") {
		t.Fatalf("worker body = %+v", wb)
	}

	// working -> idle: finished, with the last assistant message as report.
	s.observeLedger([]herdr.AgentInfo{{ID: "kube/wS:p9", Host: "kube", PaneID: "wS:p9", Status: herdr.StatusWorking}})
	s.observeLedger([]herdr.AgentInfo{{ID: "kube/wS:p9", Host: "kube", PaneID: "wS:p9", Status: herdr.StatusIdle}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		e, _ := s.ledger.Get(r.Entry.ID)
		if e.Status == ledger.Finished && e.FinalReport == "## Summary\nDone it." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not finished: %+v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The ledger's candidate hint comes from (cached, background) checks.
	for {
		w := do(s, "GET", "/api/ledger", nil, "")
		if strings.Contains(w.Body.String(), `"candidate":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no candidate hint: %s", w.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e, _ := s.ledger.Get(r.Entry.ID); e.Status != ledger.Finished {
		t.Fatal("comebye marked on its own")
	}

	// Mark, review (checks from the peer), confirm.
	if w := do(s, "POST", "/api/marks", nil, `{"agent_id":"kube/wS:p9","reason":"merged"}`); w.Code != 200 {
		t.Fatalf("mark: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/marks", nil, `{"agent_id":"kube/nobody"}`); w.Code != http.StatusNotFound {
		t.Fatalf("mark unknown: %d", w.Code)
	}
	w = do(s, "GET", "/api/removals", nil, "")
	var rv struct {
		Removals []removal `json:"removals"`
	}
	json.Unmarshal(w.Body.Bytes(), &rv)
	if w.Code != 200 || len(rv.Removals) != 1 || !rv.Removals[0].Safe || rv.Removals[0].Checks == nil || rv.Removals[0].Mark.Reason != "merged" ||
		rv.Removals[0].FinalReport == "" {
		t.Fatalf("removals = %d %s", w.Code, w.Body)
	}
	if got := peer.saw("GET /api/checks"); len(got) == 0 || !strings.Contains(got[len(got)-1], "branch=feat%2Fx") {
		t.Fatalf("checks calls = %v", got)
	}
	w = do(s, "POST", "/api/removals/confirm", nil, `{"ids":["kube/wS:p9","desk/zz"]}`)
	var cr struct {
		Results []removalOutcome `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &cr)
	if w.Code != 200 || len(cr.Results) != 2 || !cr.Results[0].OK || !cr.Results[0].Result.WorktreeRemoved || cr.Results[1].Code != "not_marked" {
		t.Fatalf("confirm = %d %s", w.Code, w.Body)
	}
	var rb removeBody
	json.Unmarshal(peer.remove, &rb)
	if rb.Agent != "wS:p9" || rb.Name != "feat-x" || !rb.RemoveWorktree || rb.AllowWorking || rb.Worktree != "/home/k/.worktrees/proj/feat-x" || rb.Branch != "feat/x" {
		t.Fatalf("remove body = %+v", rb)
	}
	if e, _ := s.ledger.Get(r.Entry.ID); e.Status != ledger.Removed || !strings.Contains(string(e.Removal), "worktree_removed") {
		t.Fatalf("entry after removal = %+v", e)
	}
}

func TestHostOpAllowlist(t *testing.T) {
	peer := &workerPeer{}
	ps := httptest.NewServer(peer)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	ctx := context.Background()
	// Only the op's own query parameters are sent.
	if code, _, err := fl.CallHost(ctx, "kube", "checks", url.Values{"path": {"/p"}, "branch": {"b"}, "evil": {"1"}}, nil); err != nil || code != 200 {
		t.Fatal(code, err)
	}
	if got := peer.saw("GET /api/checks"); len(got) != 1 || got[0] != "GET /api/checks?branch=b&path=%2Fp" {
		t.Fatalf("checks = %v", got)
	}
	// Per-agent ops, unknown ops and unknown hosts are refused; bodies
	// must be JSON within the op's cap.
	for _, op := range []string{"keys", "prompt", "events", "anything"} {
		if _, _, err := fl.CallHost(ctx, "kube", op, nil, []byte(`{}`)); err == nil {
			t.Errorf("op %s was called", op)
		}
	}
	if _, _, err := fl.CallHost(ctx, "evil", "checks", nil, nil); err == nil {
		t.Error("unknown host called")
	}
	if _, _, err := fl.CallHost(ctx, "kube", "remove", nil, []byte("not json")); err == nil {
		t.Error("non-JSON body sent")
	}
	if _, _, err := fl.CallHost(ctx, "kube", "remove", nil, []byte(`"`+strings.Repeat("x", 70<<10)+`"`)); err == nil {
		t.Error("oversized body sent")
	}
	// Browsers reach a peer's repos and checks through the hub, nothing
	// else of the worker API.
	s := New(Config{AllowLogins: []string{"me@example.com"}, Fleet: fl}, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(path string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Tailscale-User-Login", "me@example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("/api/hosts/kube/checks?path=/x&evil=1"); c != 200 {
		t.Fatalf("proxied checks: %d", c)
	}
	if got := peer.saw("GET /api/checks"); got[len(got)-1] != "GET /api/checks?path=%2Fx" {
		t.Fatalf("proxied checks query = %v", got)
	}
	for _, p := range []string{"/api/hosts/kube/worker", "/api/hosts/kube/remove"} {
		if c := get(p); c != http.StatusNotFound && c != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", p, c)
		}
	}
	for _, op := range []string{"worker", "remove"} {
		rec := httptest.NewRecorder()
		fl.ProxyHost(rec, httptest.NewRequest("POST", "/x", strings.NewReader(`{}`)), "kube", op)
		if rec.Code != http.StatusNotFound {
			t.Errorf("browser %s forwarded: %d", op, rec.Code)
		}
	}
	if n := len(peer.saw("POST")); n != 0 {
		t.Fatalf("peer got %d POSTs", n)
	}
}
