package fleet

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/testruns"
)

func runsPeer(t *testing.T, opts Options) (*Fleet, *peer) {
	t.Helper()
	u, _ := url.Parse("https://kv-a.tailnet.ts.net")
	f := New(nil, "desk", []Peer{{Name: "kv-a", URLs: []*url.URL{u}}}, opts)
	p := f.byName["kv-a"]
	p.checked = true
	return f, p
}

// A test VM's busy slots reach the hub's host list, bounded; the local host's
// come from Options.TestRuns.
func TestTestRunsReachTheHub(t *testing.T) {
	now := time.Now()
	opts := fastOpts(nil)
	opts.TestRuns = func() []state.TestRun {
		return []state.TestRun{{ID: "local-run", Slot: 1, Slots: 1, Suite: "go", Started: now, Expires: now.Add(time.Hour)}}
	}
	f, p := runsPeer(t, opts)
	var many []state.TestRun
	for i := 0; i < testruns.MaxRuns+5; i++ {
		many = append(many, state.TestRun{ID: "r", Slot: 2, Slots: 4, Suite: "browser", Branch: strings.Repeat("b", testruns.MaxText+50), Agent: "laharl/w1:p1", Started: now, Expires: now.Add(20 * time.Minute)})
	}
	// The peer's own entry is the one marked Local; a hub-of-hubs' others are ignored.
	p.setAgents(nil, []state.HostStatus{{Name: "kv-a", Local: true, TestRuns: many}, {Name: "other", TestRuns: many}})
	hosts := f.Hosts()
	if len(hosts) != 2 || len(hosts[0].TestRuns) != 1 || hosts[0].TestRuns[0].ID != "local-run" {
		t.Fatalf("%+v", hosts)
	}
	got := hosts[1].TestRuns
	if len(got) != testruns.MaxRuns || len(got[0].Branch) != testruns.MaxText || got[0].Agent != "laharl/w1:p1" || got[0].Slot != 2 || got[0].Slots != 4 {
		t.Fatalf("%d runs: %+v", len(got), got[0])
	}
	// The run ends on the VM: the next event clears it.
	p.setAgents(nil, []state.HostStatus{{Name: "kv-a", Local: true}})
	if r := f.Hosts()[1].TestRuns; len(r) != 0 {
		t.Errorf("%+v", r)
	}
}

// The hub drops a peer's run once its record has expired, even when the peer
// says nothing more (it powered off mid-run, say).
func TestTestRunsExpireOnTheHub(t *testing.T) {
	f, p := runsPeer(t, fastOpts(nil))
	now := time.Now()
	p.setAgents(nil, []state.HostStatus{{Name: "kv-a", Local: true, TestRuns: []state.TestRun{
		{ID: "soon", Slot: 1, Slots: 2, Started: now, Expires: now.Add(150 * time.Millisecond)},
		{ID: "later", Slot: 2, Slots: 2, Started: now, Expires: now.Add(time.Hour)},
		{ID: "gone", Slot: 2, Slots: 2, Started: now, Expires: now.Add(-time.Second)},
	}}})
	if r := f.Hosts()[1].TestRuns; len(r) != 2 || r[0].ID != "soon" || r[1].ID != "later" {
		t.Fatalf("%+v", r)
	}
	time.Sleep(200 * time.Millisecond)
	if r := f.Hosts()[1].TestRuns; len(r) != 1 || r[0].ID != "later" {
		t.Errorf("after expiry: %+v", r)
	}
}

// Mixed versions: a peer too old to report runs shows none, and a record with
// fields this hub does not know still shows. (A hub too old to know the field
// drops it as encoding/json drops any unknown field.)
func TestTestRunsMixedVersions(t *testing.T) {
	f, p := runsPeer(t, fastOpts(nil))
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	old := `{"type":"agents","agents":[],"hosts":[{"name":"kv-a","local":true,"status":"ok","agents":0}]}`
	p.handle("agents", old)
	if r := f.Hosts()[1].TestRuns; r != nil {
		t.Fatalf("old peer: %+v", r)
	}
	newer := `{"type":"agents","agents":[],"hosts":[{"name":"kv-a","local":true,"status":"ok","agents":0,"test_runs":[` +
		`{"id":"r1","slot":2,"slots":4,"suite":"browser","started":"2026-11-24T10:00:00Z","expires":"` + exp + `","logs":"/x","rc":null}]}]}`
	p.handle("agents", newer)
	if r := f.Hosts()[1].TestRuns; len(r) != 1 || r[0].ID != "r1" || r[0].Slot != 2 {
		t.Fatalf("newer peer: %+v", r)
	}
	// What the hub sends on decodes in a hub that knows nothing of runs.
	b, _ := json.Marshal(f.Hosts())
	var older []struct {
		Name   string `json:"name"`
		Agents int    `json:"agents"`
	}
	if err := json.Unmarshal(b, &older); err != nil || len(older) != 2 || older[1].Name != "kv-a" {
		t.Errorf("an older hub: %v %+v", err, older)
	}
	// Runs of a shape this version cannot read never cost the hub the rest of the event: an unreadable run is
	// left out, and a field that is not a list is no runs.
	p.handle("agents", `{"type":"agents","agents":[],"hosts":[{"name":"kv-a","local":true,"status":"ok","agents":0,"test_runs":[`+
		`{"id":"bad","slot":"two"},{"id":"r2","slot":1,"slots":1,"started":"2026-11-24T10:00:00Z","expires":"`+exp+`"}]}]}`)
	if h := f.Hosts()[1]; len(h.TestRuns) != 1 || h.TestRuns[0].ID != "r2" {
		t.Fatalf("one unreadable run: %+v", h)
	}
	p.handle("agents", `{"type":"agents","agents":[{"pane_id":"w1:p1","status":"idle"}],"hosts":[{"name":"kv-a","local":true,"status":"ok","agents":1,"test_runs":"nonsense"}]}`)
	if h := f.Hosts()[1]; len(h.TestRuns) != 0 || h.Agents != 1 {
		t.Fatalf("not a list: %+v", h)
	}
}

// A test VM's finished runs reach the hub, bounded, and the hub keeps the
// last report while the VM is down (asleep), until the VM reports again.
func TestTestHistoryKeptWhileDown(t *testing.T) {
	now := time.Now()
	opts := fastOpts(nil)
	zero := 0
	opts.TestHistory = func() []state.TestRun {
		return []state.TestRun{{ID: "local-done", Slot: 1, Slots: 1, Started: now, Ended: now, RC: &zero}}
	}
	f, p := runsPeer(t, opts)
	var many []state.TestRun
	for i := range testruns.MaxHistory + 9 {
		rc := i % 3
		many = append(many, state.TestRun{ID: "r", Slot: 1, Slots: 2, Suite: "browser", Agent: strings.Repeat("a", testruns.MaxText+1),
			Started: now.Add(-time.Hour), Ended: now.Add(time.Duration(i) * time.Second), RC: &rc})
	}
	p.setAgents(nil, []state.HostStatus{{Name: "kv-a", Local: true, TestHistory: many}, {Name: "other", TestHistory: many}})
	hosts := f.Hosts()
	if len(hosts[0].TestHistory) != 1 || hosts[0].TestHistory[0].ID != "local-done" {
		t.Fatalf("local: %+v", hosts[0].TestHistory)
	}
	got := hosts[1].TestHistory
	if len(got) != testruns.MaxHistory || len(got[0].Agent) != testruns.MaxText || *got[0].RC != (testruns.MaxHistory+8)%3 {
		t.Fatalf("%d: %+v", len(got), got[0])
	}
	// The VM powers off: the hub marks it down and keeps its history (but not its agents).
	p.markDown(nil)
	h := f.Hosts()[1]
	if h.Status != StatusDown || len(h.TestHistory) != testruns.MaxHistory {
		t.Fatalf("down: %s, %d runs", h.Status, len(h.TestHistory))
	}
	// It wakes and reports again: its own report replaces the kept one.
	p.setAgents(nil, []state.HostStatus{{Name: "kv-a", Local: true, TestHistory: many[:2]}})
	if h := f.Hosts()[1]; len(h.TestHistory) != 2 {
		t.Fatalf("woken: %+v", h.TestHistory)
	}
}

// Mixed versions for the history: an old peer reports none; a newer one's
// unknown fields are ignored; a field this hub cannot read is no history, and
// never costs it the rest of the event; an older hub drops the field.
func TestTestHistoryMixedVersions(t *testing.T) {
	f, p := runsPeer(t, fastOpts(nil))
	p.handle("agents", `{"type":"agents","agents":[],"hosts":[{"name":"kv-a","local":true,"status":"ok","agents":0}]}`)
	if r := f.Hosts()[1].TestHistory; r != nil {
		t.Fatalf("old peer: %+v", r)
	}
	p.handle("agents", `{"type":"agents","agents":[],"hosts":[{"name":"kv-a","local":true,"status":"ok","agents":0,"test_history":[`+
		`{"id":"d1","slot":1,"slots":1,"suite":"go","started":"2026-11-24T10:00:00Z","ended":"2026-11-24T10:05:00Z","secs":300,"rc":1,"phases":{"sync":2},"log":true},`+
		`{"id":"bad","rc":"one"},{"id":"live","started":"2026-11-24T10:00:00Z","expires":"2099-01-01T00:00:00Z"}]}]}`)
	h := f.Hosts()[1]
	if len(h.TestHistory) != 1 || h.TestHistory[0].ID != "d1" || *h.TestHistory[0].RC != 1 || !h.TestHistory[0].Log || h.TestHistory[0].Secs != 300 {
		t.Fatalf("newer peer: %+v", h.TestHistory)
	}
	p.handle("agents", `{"type":"agents","agents":[{"pane_id":"w1:p1","status":"idle"}],"hosts":[{"name":"kv-a","local":true,"status":"ok","agents":1,"test_history":{"no":"list"}}]}`)
	if h := f.Hosts()[1]; len(h.TestHistory) != 0 || h.Agents != 1 {
		t.Fatalf("not a list: %+v", h)
	}
	// What a new hub sends still decodes in an old one (unknown fields dropped).
	p.handle("agents", `{"type":"agents","agents":[],"hosts":[{"name":"kv-a","local":true,"status":"ok","agents":0,"test_history":[`+
		`{"id":"d1","slot":1,"slots":1,"suite":"go","started":"2026-11-24T10:00:00Z","ended":"2026-11-24T10:05:00Z","secs":300,"rc":0}]}]}`)
	b, _ := json.Marshal(f.Hosts())
	var older []struct {
		Name     string            `json:"name"`
		TestRuns []json.RawMessage `json:"test_runs"`
	}
	if err := json.Unmarshal(b, &older); err != nil || len(older) != 2 || older[1].Name != "kv-a" || older[1].TestRuns != nil {
		t.Errorf("an older hub: %v %+v", err, older)
	}
	if !strings.Contains(string(b), `"test_history":[{"id":"d1"`) || strings.Contains(string(b), `"expires"`) {
		t.Errorf("sent: %s", b)
	}
}
