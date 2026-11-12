package cli

import (
	"testing"
	"time"
)

func TestStatusTextShowsQuiet(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	e := &Env{Now: func() time.Time { return now }}
	since := now.Add(-47 * time.Minute)
	if got := statusText(e, agentRow{Status: "working", QuietSince: &since}); got != "working (quiet 47m)" {
		t.Fatalf("got %q", got)
	}
	if got := statusText(e, agentRow{Status: "working"}); got != "working" {
		t.Fatalf("got %q", got)
	}
}
