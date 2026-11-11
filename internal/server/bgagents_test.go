package server

import (
	"context"
	"github.com/jhjaggars/comebye/internal/ledger"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/transcript"
)

func TestBgLabelNeverShowsSecrets(t *testing.T) {
	for _, c := range []struct {
		task transcript.BackgroundTask
		want string
	}{
		{transcript.BackgroundTask{Desc: "race + uitest"}, "race + uitest"},
		{transcript.BackgroundTask{Desc: "gh run watch 123 --exit-status", Cmd: true}, "gh run"},
		{transcript.BackgroundTask{Desc: "API_TOKEN=abc123 go test -race ./...", Cmd: true}, "go test"},
		{transcript.BackgroundTask{Desc: "curl -H 'Authorization: Bearer xyz' https://x", Cmd: true}, "curl"},
		{transcript.BackgroundTask{Desc: "cd /tmp && sudo secretrunner", Cmd: true}, ""},
		{transcript.BackgroundTask{Desc: strings.Repeat("long ", 30)}, strings.Repeat("long ", 7) + "long…"},
	} {
		got := bgLabel(c.task)
		if got != c.want && !(strings.HasSuffix(c.want, "…") && strings.HasSuffix(got, "…") && len(got) <= 44) {
			t.Errorf("bgLabel(%+v) = %q, want %q", c.task, got, c.want)
		}
		if strings.Contains(got, "abc123") || strings.Contains(got, "xyz") {
			t.Errorf("secret in label %q", got)
		}
	}
}

func bgAgent(host, pane string, st herdr.AgentStatus, since time.Time) herdr.AgentInfo {
	return herdr.AgentInfo{Host: host, PaneID: pane, ID: host + "/" + pane, Status: st, StatusSince: &since}
}

func waitBg(t *testing.T, s *Server, a herdr.AgentInfo, want int) []bgBrief {
	t.Helper()
	var got []bgBrief
	for i := 0; i < 200; i++ {
		if got = s.backgroundOfAgent(a); len(got) == want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background = %+v, want %d", got, want)
	return nil
}

func TestAgentInfoCarriesBackgroundTasks(t *testing.T) {
	var reads atomic.Int32
	var tasks atomic.Pointer[[]transcript.BackgroundTask]
	set := func(ts ...transcript.BackgroundTask) { tasks.Store(&ts) }
	set(transcript.BackgroundTask{ID: "b1", Tool: "Bash", Desc: "race + uitest", Since: "2026-11-11T16:00:00Z"},
		transcript.BackgroundTask{ID: "m1", Tool: "Monitor", Desc: "gh run watch", Cmd: true, Since: "2026-11-11T16:01:00Z"})
	s := &Server{}
	s.latest = func(_ context.Context, host, pane string) *transcript.Latest {
		reads.Add(1)
		return &transcript.Latest{Background: *tasks.Load(), BackgroundKnown: true}
	}
	t0 := time.Now().Add(-time.Minute)
	for _, host := range []string{"", "peer"} { // two agents, each read once
		a := bgAgent(host, "w1:p1", herdr.StatusDone, t0)
		if got := s.backgroundOfAgent(a); len(got) != 0 {
			t.Fatalf("first answer waits for no read: %+v", got)
		}
		got := waitBg(t, s, a, 2)
		if got[0] != (bgBrief{Kind: "bash", Label: "race + uitest", Since: "2026-11-11T16:00:00Z"}) || got[1].Kind != "monitor" || got[1].Label != "gh run" {
			t.Errorf("%q briefs: %+v", host, got)
		}
		if out := s.slimAgents([]herdr.AgentInfo{a}); len(out[0].Background) != 2 {
			t.Errorf("slim agent lacks tasks: %+v", out[0])
		}
	}
	// Cached for the same idle stretch: asking again does not read again.
	n := reads.Load()
	a := bgAgent("peer", "w1:p1", herdr.StatusIdle, t0)
	a.StatusSince = &t0
	for i := 0; i < 5; i++ {
		s.backgroundOfAgent(a)
	}
	if reads.Load() != n {
		t.Errorf("re-read %d times within one stretch", reads.Load()-n)
	}
	// A working agent shows none and reads nothing; a new stretch re-reads
	// and the finished task is gone.
	if got := s.backgroundOfAgent(bgAgent("peer", "w1:p1", herdr.StatusWorking, t0)); got != nil {
		t.Errorf("working agent: %+v", got)
	}
	set()
	t1 := t0.Add(time.Minute)
	waitBg(t, s, bgAgent("peer", "w1:p1", herdr.StatusDone, t1), 0)
	// A session that cannot say (pi) reports nothing.
	pi := &Server{latest: func(context.Context, string, string) *transcript.Latest { return &transcript.Latest{} }}
	pa := bgAgent("", "w9:p1", herdr.StatusIdle, t0)
	pi.backgroundOfAgent(pa)
	time.Sleep(50 * time.Millisecond)
	if got := pi.backgroundOfAgent(pa); len(got) != 0 {
		t.Errorf("unknown session: %+v", got)
	}
}

// The hub cannot read a peer's session file: the peer reads it, sends the
// tasks with its full records, and the hub passes them on to browsers
// without reading anything itself.
func TestHubPassesAPeersBackgroundTasks(t *testing.T) {
	var reads atomic.Int32
	s := &Server{
		fleet: fleet.New(nil, "desk", nil, fleet.Options{}),
		latest: func(context.Context, string, string) *transcript.Latest {
			reads.Add(1)
			return &transcript.Latest{Background: []transcript.BackgroundTask{{Tool: "Bash", Desc: "race", Since: "2026-11-11T16:00:00Z"}}, BackgroundKnown: true}
		},
	}
	t0 := time.Now().Add(-time.Minute)
	peer := bgAgent("kube", "w2:p1", herdr.StatusDone, t0)
	peer.Background = []herdr.BackgroundTask{{Kind: "bash", Label: "race + uitest", Since: "2026-11-11T16:00:00Z"}}
	working := bgAgent("kube", "w3:p1", herdr.StatusWorking, t0)
	working.Background = peer.Background
	out := s.slimAgents([]herdr.AgentInfo{peer, working})
	if len(out[0].Background) != 1 || out[0].Background[0].Label != "race + uitest" {
		t.Errorf("the peer's tasks were dropped: %+v", out[0])
	}
	if len(out[1].Background) != 0 {
		t.Errorf("a working agent shows tasks: %+v", out[1])
	}
	// This instance's own agents are stamped on the full records a hub reads.
	local := bgAgent("desk", "w1:p1", herdr.StatusIdle, t0)
	s.withBackground([]herdr.AgentInfo{local})
	var full []herdr.AgentInfo
	for i := 0; i < 200 && (len(full) == 0 || len(full[0].Background) == 0); i++ {
		time.Sleep(10 * time.Millisecond)
		full = s.withBackground([]herdr.AgentInfo{local, peer})
	}
	if len(full[0].Background) != 1 || full[0].Background[0].Label != "race" || full[1].Background[0].Label != "race + uitest" {
		t.Fatalf("full records: %+v", full)
	}
	if reads.Load() != 1 {
		t.Errorf("the hub read %d sessions; only its own agent's, once", reads.Load())
	}
}

// The hub holds a peer worker's "finished" notice while the peer says the
// worker has a background task running, and sends it once the task ends,
// though the hub cannot read the peer's session file.
func TestPeerBackgroundHoldsFinishedNotice(t *testing.T) {
	s, _, _ := coordServer(t)
	var mu sync.Mutex
	var bg []herdr.BackgroundTask
	s.peerAgents = func() []herdr.AgentInfo {
		mu.Lock()
		defer mu.Unlock()
		a := bgAgent("kube", "w9:p1", herdr.StatusDone, time.Now())
		a.Background = bg
		return []herdr.AgentInfo{a}
	}
	s.latest = func(context.Context, string, string) *transcript.Latest {
		t.Error("the hub read a peer's session")
		return nil
	}
	e := ledger.Entry{Host: "kube", AgentID: "kube/w9:p1", PaneID: "w9:p1", Status: ledger.Finished, FinalReport: "Waiting for the race run to finish."}
	now := time.Now()
	e.FinishedAt = &now
	mu.Lock()
	bg = []herdr.BackgroundTask{{Kind: "bash", Label: "race + uitest", Since: "2026-11-11T16:00:00Z"}}
	mu.Unlock()
	if why := s.holdFinished(context.Background(), e, now); !strings.Contains(why, "background task still running (race + uitest)") {
		t.Fatalf("a peer worker with a running task: hold = %q", why)
	}
	mu.Lock()
	bg = nil
	mu.Unlock()
	e.FinalReport = "## Summary\nDone.\n\n## Verification\nok"
	if why := s.holdFinished(context.Background(), e, now); why != "" {
		t.Fatalf("task ended, notice still held: %q", why)
	}
}
