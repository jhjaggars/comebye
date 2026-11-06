package archive

import "testing"

// Two VMs have been called w1. A session of the second VM must not be linked
// to a handoff that ran on the first, even though both are "w1" in the ledger.
func TestBestLinkSeparatesInstancesOfAReusedName(t *testing.T) {
	links := []Link{
		{ID: "h-old", Host: "w1", HostInstance: "i-old", Worktree: "/wt/a", CreatedAt: "2026-05-01T10:00:00Z"},
		{ID: "h-new", Host: "w1", HostInstance: "i-new", Worktree: "/wt/a", CreatedAt: "2026-05-02T10:00:00Z"},
		{ID: "h-legacy", Host: "w1", Worktree: "/wt/b", CreatedAt: "2026-05-02T10:00:00Z"},
	}
	start := "2026-05-02T10:05:00Z"
	scope := linkScope{host: "w1", instance: "i-new", worker: true}
	if l := bestLink(links, scope, "/wt/a/sub", start); l == nil || l.ID != "h-new" {
		t.Fatalf("link = %+v", l)
	}
	// Only the old VM's handoff has this worktree: no link, rather than a wrong one.
	only := links[:1]
	if l := bestLink(only, scope, "/wt/a", start); l != nil {
		t.Fatalf("linked across instances: %+v", l)
	}
	// An entry from before instances keeps the old rule (host and dates).
	if l := bestLink(links, scope, "/wt/b", start); l == nil || l.ID != "h-legacy" {
		t.Fatalf("legacy link = %+v", l)
	}
	// A scope without an instance (an ordinary host) is unchanged.
	if l := bestLink(links, linkScope{host: "w1"}, "/wt/a", "2026-05-01T10:05:00Z"); l == nil {
		t.Fatal("ordinary host lost its link")
	}
	// The signature changes with the instance, but not for an old link.
	a, b := Link{ID: "x"}, Link{ID: "x", HostInstance: "i-1"}
	if a.sig() == b.sig() {
		t.Fatal("sig ignores the instance")
	}
}
