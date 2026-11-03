package tsauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTailscaled serves a LocalAPI on a Unix socket.
func fakeTailscaled(t *testing.T, whois func(addr string) (int, string)) (sock string, calls *atomic.Int32) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "ts.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	calls = new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("/localapi/v0/whois", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		code, body := whois(r.URL.Query().Get("addr"))
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/localapi/v0/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Self":{"DNSName":"Hub.Tail.ts.net.","TailscaleIPs":["100.64.0.9","fd7a:115c:a1e0::9"]}}`)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, calls
}

func TestWhoIsParsesTagsAndCaches(t *testing.T) {
	sock, calls := fakeTailscaled(t, func(addr string) (int, string) {
		if addr != "100.64.0.7" {
			return 404, "no match for IP:port"
		}
		return 200, `{"Node":{"Name":"Worker-1.tail.ts.net.","Tags":["tag:worker"]},"UserProfile":{}}`
	})
	l := &LocalAPI{Socket: sock, TTL: time.Minute}
	id, err := l.WhoIs(context.Background(), "100.64.0.7:51234")
	if err != nil {
		t.Fatal(err)
	}
	if id.Node != "worker-1.tail.ts.net" || len(id.Tags) != 1 || !id.HasTag("TAG:worker") || id.HasTag("tag:db") {
		t.Fatalf("id = %+v", id)
	}
	// A second connection from the same node (another port) is cached.
	if _, err := l.WhoIs(context.Background(), "100.64.0.7:60000"); err != nil || calls.Load() != 1 {
		t.Fatalf("cache: err %v, %d calls", err, calls.Load())
	}
	// Unknown to tailscaled.
	if _, err := l.WhoIs(context.Background(), "100.64.0.8:1"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestWhoIsRefusesNonTailnetAddressesWithoutAsking(t *testing.T) {
	sock, calls := fakeTailscaled(t, func(string) (int, string) { return 200, `{"Node":{"Name":"x.ts.net.","Tags":["tag:worker"]}}` })
	l := &LocalAPI{Socket: sock}
	for _, a := range []string{"127.0.0.1:1", "[::1]:1", "192.168.1.5:1", "8.8.8.8:1", "garbage", ""} {
		if _, err := l.WhoIs(context.Background(), a); !errors.Is(err, ErrUnknown) {
			t.Errorf("%q: %v", a, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("asked tailscaled %d times for non-tailnet addresses", calls.Load())
	}
	// IPv4-mapped and IPv6 tailnet addresses are looked up.
	if _, err := l.WhoIs(context.Background(), "[::ffff:100.64.0.7]:1"); err != nil {
		t.Errorf("mapped v4: %v", err)
	}
	if _, err := l.WhoIs(context.Background(), "[fd7a:115c:a1e0::9]:1"); err != nil {
		t.Errorf("v6: %v", err)
	}
}

func TestWhoIsTailscaledDownIsNotUnknown(t *testing.T) {
	l := &LocalAPI{Socket: filepath.Join(t.TempDir(), "none.sock")}
	if _, err := l.WhoIs(context.Background(), "100.64.0.7:1"); err == nil || errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v", err)
	}
}

func TestSelf(t *testing.T) {
	sock, _ := fakeTailscaled(t, nil)
	name, ips, err := (&LocalAPI{Socket: sock}).Self(context.Background())
	if err != nil || name != "hub.tail.ts.net" || len(ips) != 2 || !ips[0].Is4() {
		t.Fatalf("%q %v %v", name, ips, err)
	}
}

func TestNormalizeTags(t *testing.T) {
	got, err := NormalizeTags(" Tag:Worker, tag:vm ,")
	if err != nil || len(got) != 2 || got[0] != "tag:worker" || got[1] != "tag:vm" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"worker", "tag:", "tag:a b"} {
		if _, err := NormalizeTags(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
