package fleet

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/state"
)

// A peer's damaged repositories reach the hub's host list, bounded; the local
// host's come from Options.RepoDamage.
func TestRepoDamageReachesTheHub(t *testing.T) {
	opts := fastOpts(nil)
	opts.RepoDamage = func() []state.RepoDamage {
		return []state.RepoDamage{{Repo: "hub-repo", Path: "/h/hub-repo", Reason: "r", Source: "fsck", Since: time.Now()}}
	}
	u, _ := url.Parse("https://kube.tailnet.ts.net")
	f := New(nil, "desk", []Peer{{Name: "kube", URLs: []*url.URL{u}}}, opts)
	p := f.byName["kube"]
	p.checked = true
	var many []state.RepoDamage
	for i := 0; i < maxPeerRepoDamage+5; i++ {
		many = append(many, state.RepoDamage{Repo: "OpenShell", Path: "/home/u/code/OpenShell", Reason: strings.Repeat("x", maxRepoDamageText+10), Source: "fetch", Worktrees: []string{"/home/u/.worktrees/OpenShell/a"}})
	}
	// The peer's own entry is the one marked Local; a hub-of-hubs' others are ignored.
	p.setAgents(nil, []state.HostStatus{{Name: "kube", Local: true, RepoDamage: many}, {Name: "other", RepoDamage: many}})
	hosts := f.Hosts()
	if len(hosts) != 2 || hosts[0].Name != "desk" || len(hosts[0].RepoDamage) != 1 || hosts[0].RepoDamage[0].Repo != "hub-repo" {
		t.Fatalf("%+v", hosts)
	}
	got := hosts[1].RepoDamage
	if len(got) != maxPeerRepoDamage || len(got[0].Reason) != maxRepoDamageText || got[0].Worktrees[0] != "/home/u/.worktrees/OpenShell/a" {
		t.Fatalf("%d %d", len(got), len(got[0].Reason))
	}
	// Repaired on the peer: the next event clears it.
	p.setAgents(nil, []state.HostStatus{{Name: "kube", Local: true}})
	if d := f.Hosts()[1].RepoDamage; len(d) != 0 {
		t.Errorf("%+v", d)
	}
}
