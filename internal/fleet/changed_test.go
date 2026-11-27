package fleet

import (
	"testing"
	"time"
)

// A peer's own kinds pass through, a transcript under the hub's name for the
// peer; the hub's own kinds (and junk) from a peer are dropped.
func TestRelayKind(t *testing.T) {
	for in, want := range map[string]string{
		"parked":               "parked",
		"releases":             "releases",
		"job:pj_1":             "job:pj_1",
		"transcript:csb/w1:p1": "transcript:vm1/w1:p1",
		"transcript:w1:p1":     "transcript:vm1/w1:p1",
		"ledger":               "",
		"todos":                "",
		"pages":                "",
		"usage":                "",
		"bogus":                "",
		"job:":                 "",
		"transcript:a\"b":      "",
	} {
		if got := relayKind("vm1", in); got != want {
			t.Errorf("relayKind(vm1, %q) = %q, want %q", in, got, want)
		}
	}
}

// A "changed" event on a peer's stream goes out to the hub's browsers and to
// OnPeerChanged.
func TestPeerChangedRelayed(t *testing.T) {
	f := New(nil, "hub", nil, Options{})
	p := f.addLocked(Peer{Name: "vm1"})
	ch, cancel := f.Changes().Subscribe()
	defer cancel()
	told := make(chan string, 1)
	f.OnPeerChanged(func(host, kind string) { told <- host + " " + kind })
	p.handle("changed", `{"kind":"transcript:w1:p1"}`)
	select {
	case ev := <-ch:
		if ev.Type != "changed" || ev.Kind != "transcript:vm1/w1:p1" {
			t.Fatalf("relayed %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing relayed")
	}
	if got := <-told; got != "vm1 transcript:vm1/w1:p1" {
		t.Fatalf("OnPeerChanged got %q", got)
	}
}
