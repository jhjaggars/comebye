package park

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResumeArgs(t *testing.T) {
	launch := []string{"--model", "opus", "--allowedTools", "Bash", "--", "first prompt"}
	for _, c := range []struct {
		kind, sk, sess string
		want           []string
	}{
		{"claude", "id", "c3eb8f54-1111", []string{"--resume", "c3eb8f54-1111", "--model", "opus", "--allowedTools", "Bash"}},
		{"opencode", "id", "ses_1b3b67110ffeaYfw74rSFIG19q", []string{"-s", "ses_1b3b67110ffeaYfw74rSFIG19q", "--model", "opus", "--allowedTools", "Bash"}},
		{"pi", "path", "/home/u/.pi/agent/sessions/x/s.jsonl", []string{"--session", "/home/u/.pi/agent/sessions/x/s.jsonl", "--model", "opus", "--allowedTools", "Bash"}},
	} {
		got, err := ResumeArgs(c.kind, c.sk, c.sess, launch)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, %v; want %v", c.kind, got, err, c.want)
		}
	}
	for _, bad := range [][3]string{
		{"claude", "id", "--dangerously-skip-permissions"}, // a session id is never an option
		{"claude", "path", "/x/y.jsonl"},
		{"opencode", "id", ""},
		{"pi", "path", "relative/s.jsonl"},
		{"pi", "id", "abc"},
		{"codex", "id", "abc"},
	} {
		if _, err := ResumeArgs(bad[0], bad[1], bad[2], nil); err == nil {
			t.Errorf("ResumeArgs(%v) accepted", bad)
		}
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.NoteLaunch("w", "claude", []string{"--model", "m", "--", "p"}, now)
	if l, ok := s.LaunchOf("w"); !ok || !reflect.DeepEqual(l.Args, []string{"--model", "m"}) {
		t.Fatalf("launch = %+v", l)
	}
	a := Record{ID: NewID(), Name: "a", ParkedAt: now.Add(-time.Hour)}
	b := Record{ID: NewID(), Name: "b", ParkedAt: now}
	if !strings.HasPrefix(a.ID, "pk_") || len(a.ID) != 13 {
		t.Fatalf("id %q", a.ID)
	}
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(b); err != nil {
		t.Fatal(err)
	}
	if l := s.List(true); len(l) != 2 || l[0].Name != "b" {
		t.Fatalf("list = %+v (newest first)", l)
	}
	old := now.Add(-40 * 24 * time.Hour)
	if _, err := s.Update(a.ID, func(r *Record) { r.ResumedAt = &old }); err != nil {
		t.Fatal(err)
	}
	if l := s.List(true); len(l) != 1 || l[0].Name != "b" {
		t.Fatalf("parked only = %+v", l)
	}
	if len(s.List(false)) != 2 {
		t.Fatal("history missing")
	}
	// A reopened store sees the same thing; history older than the window goes.
	s2, err := Open(dir)
	if err != nil || len(s2.List(false)) != 2 {
		t.Fatalf("reopen = %v %v", s2, err)
	}
	if l, ok := s2.LaunchOf("w"); !ok || l.Kind != "claude" {
		t.Fatal("launches not kept")
	}
	s2.Prune(30*24*time.Hour, now)
	if l := s2.List(false); len(l) != 1 || l[0].Name != "b" {
		t.Fatalf("after prune = %+v", l)
	}
	if err := s2.Delete("pk_nope"); err != ErrNotFound {
		t.Fatalf("delete = %v", err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "parked.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func TestOpenCorrupt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "parked.json"), []byte("{not json"), 0o600)
	if _, err := Open(dir); err == nil {
		t.Fatal("a corrupt file must be an error, not an empty store that overwrites it")
	}
	var r Record
	if err := json.Unmarshal([]byte(`{"id":"pk_1"}`), &r); err != nil || !r.Parked() {
		t.Fatal("a record without resumed_at is parked")
	}
}

func TestMoveTree(t *testing.T) {
	src := filepath.Join(t.TempDir(), "s")
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "f"), []byte("hello"), 0o640)
	os.Symlink("a/b/f", filepath.Join(src, "link"))
	dst := filepath.Join(t.TempDir(), "x", "d")
	n, err := MoveTree(src, dst)
	if err != nil || n != 5 {
		t.Fatalf("move = %d, %v", n, err)
	}
	if _, err := os.Stat(src); err == nil {
		t.Fatal("source remains")
	}
	if l, _ := os.Readlink(filepath.Join(dst, "link")); l != "a/b/f" {
		t.Fatalf("symlink = %q", l)
	}
	if _, err := MoveTree(dst, dst); err == nil {
		t.Fatal("moving onto an existing path must fail")
	}
	// The copy path (what a cross-device move takes).
	c := filepath.Join(t.TempDir(), "c")
	if n, err := copyTree(dst, c); err != nil || n != 5 {
		t.Fatalf("copy = %d, %v", n, err)
	}
	if fi, _ := os.Stat(filepath.Join(c, "a", "b", "f")); fi == nil || fi.Mode().Perm() != 0o640 {
		t.Fatal("mode not kept")
	}
}
