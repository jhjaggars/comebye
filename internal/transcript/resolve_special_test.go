//go:build unix

package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/jhjaggars/herdr-phone/internal/herdr"
)

// A reported session path that is not a regular file is never handed to a
// reader (a FIFO would block it).
func TestReportedPathMustBeRegular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "s.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{ProjectsDir: t.TempDir()}
	session := func(p string) herdr.AgentInfo {
		return herdr.AgentInfo{PaneID: "p1", Agent: "pi", AgentSession: &herdr.AgentSessionInfo{Kind: "path", Value: p}}
	}
	for _, p := range []string{dir, fifo} {
		if got, err := r.SessionPath(session(p), nil); !errors.Is(err, ErrNoSession) {
			t.Errorf("%s: got %q, %v; want ErrNoSession", p, got, err)
		}
	}
	file := filepath.Join(dir, "ok.jsonl")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := r.SessionPath(session(file), nil); err != nil || got != file {
		t.Fatalf("regular file: got %q, %v", got, err)
	}
}
