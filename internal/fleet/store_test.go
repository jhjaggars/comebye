package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil || len(s.List()) != 0 {
		t.Fatalf("empty open: %v %v", s.List(), err)
	}
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	rec := Record{Name: "w1", URLs: []string{"https://w1.tail1.ts.net"}, AddedBy: "me@example.com", AddedAt: at, Tag: "tag:worker"}
	if err := s.Add(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(rec); err != ErrExists {
		t.Fatalf("duplicate add: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, StoreFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi, err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".peers-*")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
	// A restart reads it back, with the provenance fields intact.
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get("w1")
	if !ok || got.AddedBy != "me@example.com" || !got.AddedAt.Equal(at) || got.Tag != "tag:worker" || got.URLs[0] != "https://w1.tail1.ts.net" {
		t.Fatalf("reloaded = %+v", got)
	}
	if err := s2.Remove("w1"); err != nil {
		t.Fatal(err)
	}
	if err := s2.Remove("w1"); err != ErrNotFound {
		t.Fatalf("second remove: %v", err)
	}
	if s3, _ := OpenStore(dir); len(s3.List()) != 0 {
		t.Fatal("removal not persisted")
	}
}

func TestStoreRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, StoreFile), []byte("{nope"), 0o600)
	if _, err := OpenStore(dir); err == nil {
		t.Fatal("a corrupt peers.json must not load as empty")
	}
	os.WriteFile(filepath.Join(dir, StoreFile), []byte(`{"peers":[{"name":"Bad Name","urls":[]}]}`), 0o600)
	if _, err := OpenStore(dir); err == nil {
		t.Fatal("a bad name must be refused")
	}
}

func TestStoreWriteFailureLeavesNoRecord(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(dir)
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	if err := s.Add(Record{Name: "w1", URLs: []string{"https://w1.x.ts.net"}}); err == nil {
		t.Fatal("expected a write error")
	}
	if len(s.List()) != 0 {
		t.Fatal("a failed save must not keep the record")
	}
}

func TestLoadDynamicMerge(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	opt := AddrOptions{Domains: []string{".ts.net"}}
	for _, r := range []Record{
		{Name: "a", URLs: []string{"https://a.t.ts.net"}, AddedBy: "me"},
		{Name: "static1", URLs: []string{"https://s.t.ts.net"}},    // clashes with a static name
		{Name: "desk", URLs: []string{"https://d.t.ts.net"}},       // the local name
		{Name: "evil", URLs: []string{"https://evil.example.com"}}, // no longer valid
	} {
		if err := s.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	var logs []string
	static := []Peer{{Name: "static1"}}
	got := LoadDynamic(s, static, "desk", opt, func(f string, a ...any) { logs = append(logs, f) })
	if len(got) != 1 || got[0].Name != "a" || got[0].Source != SourceDynamic || got[0].AddedBy != "me" {
		t.Fatalf("merged = %+v", got)
	}
	if len(logs) != 3 {
		t.Fatalf("each skipped record is logged: %v", logs)
	}
}

func TestParseAddrsTailnetRestriction(t *testing.T) {
	opt := AddrOptions{Domains: []string{".ts.net", "corp.example.com"}}
	for _, ok := range [][]string{
		{"https://box.tail1.ts.net"},
		{"https://BOX.tail1.ts.net/"},
		{"https://box.tail1.ts.net", "https://100.101.102.103"},
		{"https://[fd7a:115c:a1e0::1]"},
		{"https://w.corp.example.com"},
	} {
		if _, err := ParseAddrs(ok, opt); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		nil,
		{"https://evil.example.com"},
		{"https://ts.net"},             // the bare suffix is not a node
		{"https://box.ts.net.evil.io"}, // suffix tricks
		{"https://notts.net"},
		{"https://100.63.0.1"}, // just below the tailnet range
		{"https://10.0.0.5"},
		{"https://127.0.0.1"},
		{"http://box.tail1.ts.net"}, // https only
		{"https://box.tail1.ts.net/api"},
		{"https://u:p@box.tail1.ts.net"},
		{"https://box.tail1.ts.net", "https://box.tail1.ts.net"},
		{"https://a.ts.net", "https://b.ts.net", "https://c.ts.net", "https://d.ts.net", "https://e.ts.net"},
	} {
		if _, err := ParseAddrs(bad, opt); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
	// Loopback http only in test mode.
	if _, err := ParseAddrs([]string{"http://127.0.0.1:9"}, opt); err == nil {
		t.Error("loopback accepted outside test mode")
	}
	opt.AllowHTTPLoopback = true
	if _, err := ParseAddrs([]string{"http://127.0.0.1:9"}, opt); err != nil {
		t.Errorf("test-mode loopback: %v", err)
	}
	if _, err := ParseAddrs([]string{"https://evil.example.com"}, opt); err == nil || !strings.Contains(err.Error(), "tailnet") {
		t.Errorf("error should name the tailnet rule: %v", err)
	}
}
