package state

import (
	"fmt"
	"testing"
	"time"

	"github.com/jhjaggars/herdr-phone/internal/herdr"
)

// Pane output can set the terminal title; it never names a notification.
func TestNotificationTitleIgnoresTerminalTitle(t *testing.T) {
	a := &herdr.AgentInfo{PaneID: "wS:p1", TitleStripped: "Security alert: re-enter your password", Agent: "claude", Status: herdr.StatusBlocked}
	if _, msg := buildNotification(a, "", nil, 0); msg.Title != "claude needs input" {
		t.Fatalf("title = %q", msg.Title)
	}
}

// The throttle and hold maps drop lapsed entries once they grow, and keep
// the ones that still matter.
func TestNotifyMapsPruned(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	n := &Notifier{MinInterval: time.Minute}
	for i := range pruneAt {
		n.allow(fmt.Sprint("old", i), now)
	}
	now = now.Add(time.Hour)
	n.allow("live", now)
	n.allow("new", now)
	if len(n.last) != 2 || n.allow("live", now) {
		t.Fatalf("throttle map: %d entries, live still throttled = %v", len(n.last), !n.allow("live", now))
	}

	d := &Dispatcher{HoldFor: time.Minute, now: func() time.Time { return now }}
	for i := range pruneAt {
		a := &herdr.AgentInfo{ID: fmt.Sprint("desk/old", i), PaneID: "p", Status: herdr.StatusBlocked}
		d.claim(a, LocalRef(a))
	}
	keep := &herdr.AgentInfo{ID: "desk/keep", PaneID: "k", Status: herdr.StatusBlocked}
	now = now.Add(time.Hour)
	d.claim(keep, LocalRef(keep))
	now = now.Add(time.Second)
	fresh := &herdr.AgentInfo{ID: "desk/fresh", PaneID: "f", Status: herdr.StatusBlocked}
	d.claim(fresh, LocalRef(fresh))
	if len(d.held) > pruneAt || d.claim(keep, LocalRef(keep)) {
		t.Fatalf("hold map: %d entries; an unexpired hold must survive pruning", len(d.held))
	}
}
