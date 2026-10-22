package fleet

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// A peer cannot make the hub track more than maxPeerAgents agents, use
// absurd pane ids, or pass statuses outside herdr's set.
func TestPeerSnapshotSanitized(t *testing.T) {
	var logs []string
	opts := fastOpts(nil)
	opts.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	u, _ := url.Parse("https://kube.tailnet.ts.net")
	f := New(nil, "desk", []Peer{{Name: "kube", URL: u}}, opts)
	p := f.byName["kube"]
	p.checked = true // no push probe

	agents := []herdr.AgentInfo{
		agent("wS:bad", "x", `idle" onmouseover="alert(1)`),
		agent(strings.Repeat("p", maxPaneID+1), "long", herdr.StatusIdle),
	}
	for i := range maxPeerAgents + 10 {
		agents = append(agents, agent(fmt.Sprintf("wS:p%d", i), "a", herdr.StatusWorking))
	}
	p.setAgents(agents, nil)
	p.setAgents(agents, nil)

	got := f.Agents()
	if len(got) != maxPeerAgents {
		t.Fatalf("kept %d agents, want %d", len(got), maxPeerAgents)
	}
	if got[0].PaneID != "wS:bad" || got[0].Status != herdr.StatusUnknown {
		t.Fatalf("bad status not sanitized: %+v", got[0])
	}
	for _, a := range got {
		if len(a.PaneID) > maxPaneID {
			t.Fatalf("overlong pane id kept")
		}
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "ignoring the rest") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("cap logged %d times, want once: %q", n, logs)
	}
}
