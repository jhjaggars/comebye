package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

func capHandoff(t *testing.T, s *Server, branch string) (int, ledger.Entry, bool) {
	t.Helper()
	w := do(s, "POST", "/api/handoffs", nil, `{"host":"kube","repo":"/home/k/code/proj","branch":"`+branch+`","kind":"claude","brief":"Do it.","from":"coordinator"}`)
	var r struct {
		Entry  ledger.Entry `json:"entry"`
		Queued bool         `json:"queued"`
	}
	json.Unmarshal(w.Body.Bytes(), &r)
	s.releasing.Wait()
	return w.Code, r.Entry, r.Queued
}

func capPeer(t *testing.T, limit string) (*Server, *workerPeer) {
	s, peer := afterServer(t)
	peer.numbered = true
	peer.settings = `{"host":"kube","settings":{"rev":1},"effective":{"max_workers_per_repo":` + limit + `}}`
	return s, peer
}

func TestCapQueuesOverTheLimitAndStartsWhenOneFinishes(t *testing.T) {
	s, peer := capPeer(t, "2")
	var run []ledger.Entry
	for _, b := range []string{"a", "b"} {
		code, e, queued := capHandoff(t, s, b)
		if code != 200 || queued || e.Status != ledger.Running {
			t.Fatalf("%s: %d queued=%v %+v", b, code, queued, e)
		}
		run = append(run, e)
	}
	code, c, queued := capHandoff(t, s, "c")
	if code != 200 || !queued || c.Status != ledger.Waiting || c.WaitKind != "cap" || !strings.Contains(c.WaitNote, "2 of 2") || len(c.After) != 0 {
		t.Fatalf("c = %d queued=%v %+v", code, queued, c)
	}
	// A later one goes behind c, not ahead of it.
	if _, d, queued := capHandoff(t, s, "d"); !queued || d.Status != ledger.Waiting {
		t.Fatalf("d = %+v", d)
	}
	// The same branch again answers with the queued entry rather than a second one.
	if code, _, _ := capHandoff(t, s, "c"); code != 409 {
		t.Fatalf("c twice = %d", code)
	}
	if len(peer.saw("POST /api/worker")) != 2 {
		t.Fatalf("workers started = %d", len(peer.saw("POST /api/worker")))
	}
	s.observeLedger(nil)
	if g, _ := s.ledger.Get(c.ID); g.Status != ledger.Waiting {
		t.Fatalf("started while full: %+v", g)
	}

	// One worker reports: c (the oldest) takes the slot; d keeps waiting.
	if _, ok, _ := s.ledger.Report(run[0].AgentID, "Done.", time.Now()); !ok {
		t.Fatal("report")
	}
	s.observeLedger([]herdr.AgentInfo{{ID: run[1].AgentID, Host: "kube", PaneID: run[1].PaneID, Status: herdr.StatusWorking}})
	waitFor(t, func() bool { g, _ := s.ledger.Get(c.ID); return g.Status == ledger.Running })
	s.releasing.Wait()
	s.observeLedger(nil)
	time.Sleep(100 * time.Millisecond)
	var d ledger.Entry
	for _, e := range s.ledger.Entries() {
		if e.Branch == "d" {
			d = e
		}
	}
	if d.Status != ledger.Waiting || d.WaitKind != "cap" {
		t.Fatalf("d = %+v", d)
	}
	if g, _ := s.ledger.Get(c.ID); g.WaitKind != "" || g.WaitNote != "" {
		t.Fatalf("started entry keeps its wait note: %+v", g)
	}

	// release starts it regardless of the cap.
	if w := do(s, "POST", "/api/handoffs/"+d.ID+"/release", nil, ""); w.Code != 200 {
		t.Fatalf("release = %d %s", w.Code, w.Body)
	}
	if g, _ := s.ledger.Get(d.ID); g.Status != ledger.Running {
		t.Fatalf("released = %+v", g)
	}
}

func TestCapZeroAndUnknownPeerMeanNoCap(t *testing.T) {
	s, _ := capPeer(t, "0")
	for _, b := range []string{"a", "b", "c", "d", "e", "f"} {
		if _, e, queued := capHandoff(t, s, b); queued || e.Status != ledger.Running {
			t.Fatalf("%s queued under no cap: %+v", b, e)
		}
	}
	// A peer that cannot say (too old) is not capped.
	s2, peer := capPeer(t, "1")
	peer.settings = ""
	for _, b := range []string{"a", "b", "c"} {
		if _, e, queued := capHandoff(t, s2, b); queued || e.Status != ledger.Running {
			t.Fatalf("%s: %+v", b, e)
		}
	}
}

func TestCapIsPerRepository(t *testing.T) {
	s, _ := capPeer(t, "1")
	if _, e, q := capHandoff(t, s, "a"); q || e.Status != ledger.Running {
		t.Fatal("first")
	}
	w := do(s, "POST", "/api/handoffs", nil, `{"host":"kube","repo":"/home/k/code/other","branch":"a","kind":"claude","brief":"x","from":"c"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"queued":true`) {
		t.Fatalf("other repo = %d %s", w.Code, w.Body)
	}
}

func TestLocalCapSettingRoundTrip(t *testing.T) {
	s := New(Config{ReadLines: 100, StateDir: t.TempDir()}, nil, nil)
	get := func() settingsView {
		var v settingsView
		w := do(s, "GET", "/api/settings", map[string]string{"Tailscale-User-Login": "u@example.com"}, "")
		json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	if v := get(); v.Effective.MaxWorkersPerRepo != 4 || v.Effective.MaxWorkersPerRepoSource != "default" {
		t.Fatalf("default = %+v", v.Effective)
	}
	put := func(body string) int {
		return do(s, "PUT", "/api/settings", map[string]string{"Tailscale-User-Login": "u@example.com"}, body).Code
	}
	if c := put(`{"max_workers_per_repo":2}`); c != 200 {
		t.Fatalf("put = %d", c)
	}
	if v := get(); v.Effective.MaxWorkersPerRepo != 2 || v.Effective.MaxWorkersPerRepoSource != "setting" {
		t.Fatalf("set = %+v", v.Effective)
	}
	if c := put(`{"max_workers_per_repo":0}`); c != 200 {
		t.Fatalf("put 0 = %d", c)
	}
	if v := get(); v.Effective.MaxWorkersPerRepo != 0 || v.Effective.MaxWorkersPerRepoSource != "setting" {
		t.Fatalf("zero = %+v", v.Effective)
	}
	if c := put(`{"max_workers_per_repo":-1}`); c != 400 {
		t.Fatalf("negative = %d", c)
	}
	if c := put(`{}`); c != 200 {
		t.Fatalf("clear = %d", c)
	}
	if v := get(); v.Effective.MaxWorkersPerRepo != 4 {
		t.Fatalf("cleared = %+v", v.Effective)
	}
}
