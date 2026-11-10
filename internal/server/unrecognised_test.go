package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func captureFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// observeWith runs one read through the capture the way currentDialogCaptured does.
func observeWith(u *unrecCapture, io paneIO, a herdr.AgentInfo) {
	rec := &recordingIO{paneIO: io}
	d, _ := currentDialog(context.Background(), rec, a, nil)
	u.observe(a, rec, d)
}

func TestUnrecognisedCapture(t *testing.T) {
	dir := t.TempDir()
	u := newUnrecCapture(dir, "h1")
	now := time.Date(2026, 11, 10, 22, 38, 52, 0, time.UTC)
	u.now = func() time.Time { return now }
	a := herdr.AgentInfo{PaneID: "w8:p1", Agent: "claude", Status: herdr.StatusBlocked}
	long := strings.Repeat("line\n", 200) + "the last line\n"
	io := &fakeIO{screens: []string{long}}

	observeWith(u, io, a)
	observeWith(u, io, a) // same occurrence: nothing more
	capDir := filepath.Join(dir, unrecDirName)
	files := captureFiles(t, capDir)
	if len(files) != 1 || !strings.HasSuffix(files[0], "-screen.txt") {
		t.Fatalf("files %v", files)
	}
	if u.savedScreen("w8:p1") != files[0] {
		t.Errorf("savedScreen %q", u.savedScreen("w8:p1"))
	}
	fi, _ := os.Stat(filepath.Join(capDir, files[0]))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(capDir, files[0]))
	body := string(b)
	if !strings.Contains(body, "agent: w8:p1") || !strings.Contains(body, "host: h1") || !strings.HasSuffix(body, "the last line\n") {
		t.Errorf("content %q", body)
	}
	if n := strings.Count(body, "line\n"); n > unrecMaxLines+1 {
		t.Errorf("%d lines kept", n)
	}

	// A recognised dialog ends the occurrence; the next unrecognised one is saved again.
	observeWith(u, &fakeIO{screens: []string{"Q?\n❯ 1. Yes\n  2. No\n"}}, a)
	if u.savedScreen("w8:p1") != "" {
		t.Error("occurrence not cleared by a recognised dialog")
	}
	now = now.Add(time.Minute)
	observeWith(u, io, a)
	if n := len(captureFiles(t, capDir)); n != 2 {
		t.Errorf("%d files after a second occurrence", n)
	}

	// A running agent is never captured.
	b2 := herdr.AgentInfo{PaneID: "w9:p1", Agent: "claude", Status: herdr.StatusWorking}
	observeWith(u, io, b2)
	if n := len(captureFiles(t, capDir)); n != 2 {
		t.Errorf("captured a working agent: %d files", n)
	}
}

func TestUnrecognisedReadErrorSeparate(t *testing.T) {
	dir := t.TempDir()
	u := newUnrecCapture(dir, "h1")
	a := herdr.AgentInfo{PaneID: "w8:p1", Agent: "claude", Status: herdr.StatusBlocked}
	observeWith(u, &errIO{}, a)
	observeWith(u, &errIO{}, a)
	files := captureFiles(t, filepath.Join(dir, unrecDirName))
	if len(files) != 1 || !strings.HasSuffix(files[0], "-readerror.txt") {
		t.Fatalf("files %v", files)
	}
	if u.savedScreen("w8:p1") != "" {
		t.Error("a read error counted as a saved screen")
	}
	b, _ := os.ReadFile(filepath.Join(dir, unrecDirName, files[0]))
	if !strings.Contains(string(b), "boom") {
		t.Errorf("%q", b)
	}
}

func TestUnrecognisedPrune(t *testing.T) {
	dir := t.TempDir()
	u := newUnrecCapture(dir, "h1")
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < unrecKeepFiles+5; i++ {
		now = now.Add(time.Second)
		u.now = func() time.Time { return now }
		u.save(herdr.AgentInfo{PaneID: "p"}, "screen", "x\n")
	}
	if n := len(captureFiles(t, filepath.Join(dir, unrecDirName))); n != unrecKeepFiles {
		t.Errorf("kept %d", n)
	}
}

func TestNilCaptureIsSafe(t *testing.T) {
	var u *unrecCapture
	if u.savedScreen("x") != "" || newUnrecCapture("", "h") != nil {
		t.Error("nil capture")
	}
}
