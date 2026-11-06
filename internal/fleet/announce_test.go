package fleet

import (
	"errors"
	"strings"
	"testing"
)

func announceFleet(t *testing.T) *Fleet {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opt := AddrOptions{Domains: []string{".ts.net"}}
	f := New(nil, "desk", nil, Options{HubMode: true, Logf: func(string, ...any) {}})
	f.AttachStore(store, opt)
	return f
}

func TestAnnounce(t *testing.T) {
	f := announceFleet(t)
	info, created, err := f.Announce("vm1", "https://vm1.t.ts.net", "vm1.t.ts.net", "tag:worker")
	if err != nil || !created || info.AddedBy != "tag:worker@vm1.t.ts.net" || info.Tag != "tag:worker" || info.Source != SourceDynamic {
		t.Fatalf("%+v %v %v", info, created, err)
	}
	// Same node, same name: idempotent (also with the trailing dot and case
	// the node name might carry).
	if _, created, err := f.Announce("vm1", "", "VM1.t.ts.net.", "tag:worker"); err != nil || created {
		t.Fatalf("re-announce: %v %v", created, err)
	}
	// Same node, another name: refused. Another node, the name: refused.
	if _, _, err := f.Announce("vm9", "", "vm1.t.ts.net", "tag:worker"); !errors.Is(err, ErrNodeRegistered) {
		t.Fatalf("second name: %v", err)
	}
	if _, _, err := f.Announce("vm1", "", "vm2.t.ts.net", "tag:worker"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("taken name: %v", err)
	}
	// The URL must be the caller's node.
	var inv InvalidError
	if _, _, err := f.Announce("vm3", "https://vm4.t.ts.net", "vm3.t.ts.net", "tag:worker"); !errors.As(err, &inv) {
		t.Fatalf("other node's url: %v", err)
	}
	// The *.ts.net rule still applies (a node outside the tailnet domain).
	if _, _, err := f.Announce("odd", "", "odd.example.com", "tag:worker"); !errors.As(err, &inv) {
		t.Fatalf("non-tailnet: %v", err)
	}
	// A static peer's name is taken too; the local name is refused.
	if _, _, err := f.Announce("desk", "", "desk.t.ts.net", "tag:worker"); !errors.Is(err, ErrNameTaken) && !errors.Is(err, ErrLocalName) {
		t.Fatalf("local name: %v", err)
	}
	if name, tag, ok := f.FindByHost("VM1.t.ts.net."); !ok || name != "vm1" || tag != "tag:worker" {
		t.Fatalf("FindByHost: %q %q %v", name, tag, ok)
	}
	if _, _, ok := f.FindByHost("nobody.t.ts.net"); ok {
		t.Fatal("found a host nobody registered")
	}
}

func TestAnnounceNeedsAHubWithAStore(t *testing.T) {
	f := New(nil, "desk", nil, Options{HubMode: true, Logf: func(string, ...any) {}})
	if _, _, err := f.Announce("a", "", "a.t.ts.net", "tag:worker"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("%v", err)
	}
}

func TestUpstreamRefusesThePlainHTTPPeerListener(t *testing.T) {
	if _, err := NewUpstream("https://hub.t.ts.net", false, nil); err != nil {
		t.Errorf("Serve URL: %v", err)
	}
	// The removed peer listener's address says so, rather than "not https".
	for _, old := range []string{"http://hub.t.ts.net:8742", "http://100.64.0.9:8742"} {
		if _, err := NewUpstream(old, false, nil); err == nil || !strings.Contains(err.Error(), "Serve URL") {
			t.Errorf("%s: %v", old, err)
		}
	}
	for _, bad := range []string{"http://hub.t.ts.net", "http://hub.example.com:8742", "http://192.168.1.2:8742", "http://u:p@hub.t.ts.net:8742"} {
		if _, err := NewUpstream(bad, false, nil); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
