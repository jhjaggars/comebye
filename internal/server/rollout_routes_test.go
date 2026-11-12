package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/release"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// rolloutRouteRig is a hub with a tagged release and a fake fleet under the
// rollout engine, so the routes can be driven without peers.
func rolloutRouteRig(t *testing.T, names ...string) (*Server, *fakeFleet, *fakeTool, Todo, string) {
	t.Helper()
	s, tool, _ := relServer(t)
	if s.rollout == nil {
		t.Fatal("a ledger host has no rollout engine")
	}
	f := newFakeFleet("v0.5.9", names...)
	for _, h := range f.hosts {
		h.Version = "v0.5.8"
	}
	s.rollout = newRolloutEngine(filepath.Join(t.TempDir(), "rollout.json"), f, s.auditBy)
	s.rollout.now = f.clock
	s.rollout.onState = s.noteRollout
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := tagIt(t, s, it.ID)
	return s, f, tool, it, tok
}

func startBody(it Todo, hosts string) string {
	return `{"todo":"` + it.ID + `","version":"v0.5.9","commit":"` + relSHA + `","hosts":` + hosts + `}`
}

func TestRolloutStartRoute(t *testing.T) {
	s, f, tool, it, tok := rolloutRouteRig(t, "a", "b")
	post := func(path, body string) *httpRec { return rec(do(s, "POST", path, withCSRF(tok), body)) }

	// Refusals first: nothing is saved by any of them.
	cases := []struct {
		name, body, code string
		status           int
	}{
		{"no hosts", startBody(it, `[]`), "no_hosts", 400},
		{"wrong version", `{"todo":"` + it.ID + `","version":"v0.6.0","commit":"` + relSHA + `","hosts":["a"]}`, "confirm_mismatch", 409},
		{"wrong commit", `{"todo":"` + it.ID + `","version":"v0.5.9","commit":"` + strings.Repeat("b", 40) + `","hosts":["a"]}`, "confirm_mismatch", 409},
		{"unknown host", startBody(it, `["zz"]`), "bad_plan", 409},
		{"duplicate", startBody(it, `["a","a"]`), "bad_plan", 409},
	}
	for _, c := range cases {
		if w := post("/api/release/rollout", c.body); w.Code != c.status || !strings.Contains(w.Body, c.code) {
			t.Errorf("%s = %d %s", c.name, w.Code, w.Body)
		}
	}
	tool.mu.Lock()
	tool.build = release.Build{State: "in_progress"}
	tool.mu.Unlock()
	if w := post("/api/release/rollout", startBody(it, `["a","b"]`)); w.Code != 409 || !strings.Contains(w.Body, "build_not_green") {
		t.Fatalf("building = %d %s", w.Code, w.Body)
	}
	tool.mu.Lock()
	tool.build = release.Build{State: "success"}
	tool.mu.Unlock()
	f.set("b", func(h *relHost) { h.Enabled = false })
	if w := post("/api/release/rollout", startBody(it, `["a","b"]`)); w.Code != 409 || !strings.Contains(w.Body, "bad_plan") || !strings.Contains(w.Body, "-release-upgrade") {
		t.Fatalf("not enabled = %d %s", w.Code, w.Body)
	}
	f.set("b", func(h *relHost) { h.Enabled = true })
	if _, ok := s.rollout.Get(); ok {
		t.Fatal("a refused start saved a job")
	}

	// A user without the page's token cannot start one.
	if w := do(s, "POST", "/api/release/rollout", userHdr, startBody(it, `["a"]`)); w.Code != 403 {
		t.Fatalf("no csrf = %d", w.Code)
	}

	w := post("/api/release/rollout", startBody(it, `["b","a"]`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	var out struct{ Rollout Rollout }
	json.Unmarshal([]byte(w.Body), &out)
	j := out.Rollout
	if j.State != roRunning || j.Canary != "b" || j.Plan[0].Host != "b" || j.Plan[0].Wave != waveCanary || j.By != "me@example.com" || j.Todo != it.ID {
		t.Fatalf("job = %+v", j)
	}
	// A second start is refused while this one is live.
	if w := post("/api/release/rollout", startBody(it, `["a"]`)); w.Code != 409 || !strings.Contains(w.Body, "rollout_active") {
		t.Fatalf("second = %d %s", w.Code, w.Body)
	}
	// And a click on a host's Upgrade is refused meanwhile.
	if w := post("/api/hosts/desk/release/upgrade", `{"todo":"`+it.ID+`","version":"v0.5.9"}`); w.Code != 409 || !strings.Contains(w.Body, "rollout_active") {
		t.Fatalf("manual upgrade = %d %s", w.Code, w.Body)
	}

	// The view carries the job.
	v := do(s, "GET", "/api/releases", userHdr, "")
	var view struct {
		Job     *Rollout `json:"rollout_job"`
		Enabled bool     `json:"rollout_enabled"`
	}
	json.Unmarshal(v.Body.Bytes(), &view)
	if view.Job == nil || view.Job.ID != j.ID || !view.Enabled {
		t.Fatalf("view = %s", v.Body)
	}

	// Pause, resume, cancel: each names the rollout it means.
	if w := post("/api/release/rollout/pause", `{"id":"nope"}`); w.Code != 409 || !strings.Contains(w.Body, "confirm_mismatch") {
		t.Fatalf("wrong id = %d %s", w.Code, w.Body)
	}
	if w := post("/api/release/rollout/resume", `{"id":"`+j.ID+`"}`); w.Code != 409 || !strings.Contains(w.Body, "bad_state") {
		t.Fatalf("resume a running one = %d %s", w.Code, w.Body)
	}
	if w := post("/api/release/rollout/pause", `{"id":"`+j.ID+`"}`); w.Code != 200 {
		t.Fatalf("pause = %d %s", w.Code, w.Body)
	}
	if cur, _ := s.rollout.Get(); cur.State != roPaused {
		t.Fatalf("state %s", cur.State)
	}
	if w := post("/api/release/rollout/resume", `{"id":"`+j.ID+`"}`); w.Code != 200 {
		t.Fatalf("resume = %d %s", w.Code, w.Body)
	}
	if w := post("/api/release/rollout/cancel", `{"id":"`+j.ID+`"}`); w.Code != 200 {
		t.Fatalf("cancel = %d %s", w.Code, w.Body)
	}
	if cur, _ := s.rollout.Get(); cur.State != roCancelled {
		t.Fatalf("state %s", cur.State)
	}
	// Cancelled: the buttons of the hosts are back (Upgrade is no longer refused for the rollout).
	if w := post("/api/hosts/desk/release/upgrade", `{"todo":"`+it.ID+`","version":"v0.5.9"}`); strings.Contains(w.Body, "rollout_active") {
		t.Fatalf("still refused after cancel: %s", w.Body)
	}
	// The audit trail names who did what, under the rollout's id.
	var acts []string
	for _, e := range s.ledger.AuditLog(it.ID) {
		if strings.HasPrefix(e.Action, "rollout.") {
			if !strings.Contains(e.Detail, "id="+j.ID) {
				t.Errorf("entry lacks the id: %+v", e)
			}
			acts = append(acts, e.Action+"/"+e.By)
		}
	}
	if got, want := strings.Join(acts, " "), "rollout.start/me@example.com rollout.pause/me@example.com rollout.resume/me@example.com rollout.cancel/me@example.com"; got != want {
		t.Fatalf("audit = %s", got)
	}
}

type httpRec struct {
	Code int
	Body string
}

func rec(w *httptest.ResponseRecorder) *httpRec { return &httpRec{w.Code, w.Body.String()} }

func TestRolloutNeedsTheLedgerHost(t *testing.T) {
	s, _, _ := relServer(t)
	s.rollout = nil
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := tagIt(t, s, it.ID)
	if w := do(s, "POST", "/api/release/rollout", withCSRF(tok), startBody(it, `["a"]`)); w.Code != 409 || !strings.Contains(w.Body.String(), "rollout_disabled") {
		t.Fatalf("= %d %s", w.Code, w.Body)
	}
	if s.rolloutLive() {
		t.Fatal("live without an engine")
	}
}

func TestRolloutViewOffersAPlanBeforeOne(t *testing.T) {
	s, _, _, _, _ := rolloutRouteRig(t, "a")
	v := do(s, "GET", "/api/releases", userHdr, "")
	var view struct {
		Plan    []RolloutStep `json:"rollout_plan"`
		Err     string        `json:"rollout_plan_error"`
		Job     *Rollout      `json:"rollout_job"`
		Enabled bool          `json:"rollout_enabled"`
	}
	json.Unmarshal(v.Body.Bytes(), &view)
	// The view reads the server's own fleet (this one host): the plan is that host, alone (no canary).
	if !view.Enabled || len(view.Plan) != 1 || view.Plan[0].Wave != wavePeers || view.Job != nil || view.Err != "" {
		t.Fatalf("view = %s", v.Body)
	}
}

func TestRolloutNoteAndAuditFollowTheJob(t *testing.T) {
	s, f, _, it, tok := rolloutRouteRig(t, "a", "b")
	post := func(path, body string) *httpRec { return rec(do(s, "POST", path, withCSRF(tok), body)) }
	note := func() string { x, _ := s.todos.get(it.ID); return x.Release.RolloutNote }
	var out struct{ Rollout Rollout }
	json.Unmarshal([]byte(post("/api/release/rollout", startBody(it, `["a","b"]`)).Body), &out)
	id := out.Rollout.ID
	if note() != "" {
		t.Fatalf("note while running: %q", note())
	}
	ctx := context.Background()
	s.rollout.tick(ctx) // a upgrading
	f.finish("a", upgrade.RecRolledBack, "")
	s.rollout.tick(ctx)
	if got := note(); got != "v0.5.9 stopped at a: rolled_back: smoke: journal has errors" {
		t.Fatalf("note after a stop: %q", got)
	}
	v := do(s, "GET", "/api/releases", userHdr, "")
	var view struct {
		Needs bool   `json:"needs_you"`
		Text  string `json:"needs_text"`
	}
	json.Unmarshal(v.Body.Bytes(), &view)
	if !view.Needs || !strings.Contains(view.Text, "stopped at a") {
		t.Fatalf("view needs = %v %q", view.Needs, view.Text)
	}
	post("/api/release/rollout/resume", `{"id":"`+id+`"}`)
	if note() != "" {
		t.Fatalf("note after retry: %q", note())
	}
	post("/api/release/rollout/cancel", `{"id":"`+id+`"}`)
	if !strings.Contains(note(), "cancelled") {
		t.Fatalf("note after cancel: %q", note())
	}
	var acts []string
	for _, e := range s.ledger.AuditLog(it.ID) {
		if strings.HasPrefix(e.Action, "rollout.") {
			acts = append(acts, e.Action)
		}
	}
	want := "rollout.start rollout.step rollout.stop rollout.resume rollout.cancel"
	if strings.Join(acts, " ") != want {
		t.Fatalf("audit %v", acts)
	}
}
