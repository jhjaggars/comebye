package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/archive"
	"github.com/jhjaggars/comebye/internal/herdr"
)

// Removing a worker copies its session file to the archive first, including
// a last line with no newline; the removal still succeeds.
func TestRemoveFlushesTranscriptToArchive(t *testing.T) {
	s, fh, _, _ := removalSetup(t)
	d := t.TempDir()
	ident, rcpt, _ := archive.GenerateIdentity()
	sealer, _ := archive.NewSealer(rcpt)
	opener, _ := archive.NewOpener(ident)
	b, _ := archive.NewFSBackend(filepath.Join(d, "store"))
	s.cfg.Archive = &archive.Archiver{Host: "laharl", B: b, S: sealer}
	piRoot := filepath.Join(d, "pi")
	s.cfg.ArchiveSources = []archive.Source{{Kind: "pi", Root: piRoot}}

	sess := filepath.Join(piRoot, "--proj--", "2026-10-05T09-00-00-000Z_abc.jsonl")
	os.MkdirAll(filepath.Dir(sess), 0o755)
	content := `{"type":"session","id":"abc","cwd":"/x"}` + "\n" + `{"type":"message","partial`
	os.WriteFile(sess, []byte(content), 0o644)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents[0].AgentSession = &herdr.AgentSessionInfo{Kind: "path", Value: sess}
	})

	code, r := removeReq(t, s, `{"agent":"w9:p1","name":"w","remove_worktree":true}`)
	if code != 200 || r["closed"] != "workspace" || !strings.Contains(r["archived"].(string), "1 segments") {
		t.Fatalf("remove = %d %v", code, r)
	}
	var got bytes.Buffer
	if _, err := archive.StreamTo(&got, b, opener, "laharl", "pi", "--proj--/2026-10-05T09-00-00-000Z_abc.jsonl"); err != nil || got.String() != content {
		t.Fatalf("archived %q, %v", got.String(), err)
	}
}

// An archive that cannot be written does not stop the removal.
func TestRemoveGoesAheadWhenArchiveFails(t *testing.T) {
	s, fh, _, _ := removalSetup(t)
	d := t.TempDir()
	_, rcpt, _ := archive.GenerateIdentity()
	sealer, _ := archive.NewSealer(rcpt)
	b, _ := archive.NewFSBackend(filepath.Join(d, "store"))
	s.cfg.Archive = &archive.Archiver{Host: "laharl", B: b, S: sealer}
	s.cfg.ArchiveSources = []archive.Source{{Kind: "pi", Root: filepath.Join(d, "pi")}}
	sess := filepath.Join(d, "pi", "--p--", "s.jsonl")
	os.MkdirAll(filepath.Dir(sess), 0o755)
	os.WriteFile(sess, []byte("x\n"), 0o644)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents[0].AgentSession = &herdr.AgentSessionInfo{Kind: "path", Value: sess}
	})
	os.RemoveAll(filepath.Join(d, "store")) // the backend is gone
	os.WriteFile(filepath.Join(d, "store"), []byte("not a dir"), 0o644)
	code, r := removeReq(t, s, `{"agent":"w9:p1","name":"w","remove_worktree":true}`)
	if code != 200 || r["closed"] != "workspace" {
		t.Fatalf("remove = %d %v", code, r)
	}
	if a, _ := r["archived"].(string); !strings.Contains(a, "archive") {
		t.Fatalf("no archive note: %v", r)
	}
}
