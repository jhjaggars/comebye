package server

import (
	"os"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

// The transcript's "changed" event (issue #143, slice 1).
//
// An open chat used to read the transcript every 4 s while the agent worked.
// Now a read of the transcript marks it watched for a while, and this host
// (the one with the session file) looks at the file's size and time each
// second while anything is watched. When the file grows it sends
// transcript:<agent id>; the hub relays a peer's under its own name for the
// peer, and the app reads the transcript again. Every read renews the watch,
// and the app's slow safety read keeps a quiet one alive, so a watch ends a
// few minutes after the last app stops looking. Nothing runs while nothing is
// watched.

const (
	transcriptWatchEvery = time.Second
	transcriptWatchFor   = 3 * time.Minute
)

type transcriptWatch struct {
	mu      sync.Mutex
	m       map[string]*watchedFile // by changed kind
	running bool
	every   time.Duration
	keep    time.Duration
}

type watchedFile struct {
	path  string
	until time.Time
	size  int64
	mod   time.Time
}

func statOf(path string) (int64, time.Time) {
	fi, err := os.Stat(path)
	if err != nil {
		return -1, time.Time{}
	}
	return fi.Size(), fi.ModTime()
}

// watchTranscript marks a's session file at path watched (renewing it).
func (s *Server) watchTranscript(a herdr.AgentInfo, path string) {
	if path == "" || a.PaneID == "" {
		return
	}
	kind := state.ChangedTranscriptPrefix + state.QualifiedID(s.fleet.LocalName(), a.PaneID)
	w := &s.twatch
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]*watchedFile{}
	}
	if w.every <= 0 {
		w.every = transcriptWatchEvery
	}
	if w.keep <= 0 {
		w.keep = transcriptWatchFor
	}
	until := time.Now().Add(w.keep)
	if f := w.m[kind]; f != nil && f.path == path {
		f.until = until
	} else {
		size, mod := statOf(path)
		w.m[kind] = &watchedFile{path: path, until: until, size: size, mod: mod}
	}
	if !w.running {
		w.running = true
		go s.runTranscriptWatch(w.every)
	}
}

// runTranscriptWatch looks at the watched files until none is left.
func (s *Server) runTranscriptWatch(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		if !s.transcriptWatchStep(time.Now()) {
			return
		}
	}
}

// transcriptWatchStep is one look; false once nothing is watched (the loop
// then ends).
func (s *Server) transcriptWatchStep(now time.Time) bool {
	w := &s.twatch
	w.mu.Lock()
	files := make(map[string]watchedFile, len(w.m))
	for k, f := range w.m {
		if now.After(f.until) {
			delete(w.m, k)
			continue
		}
		files[k] = *f
	}
	if len(w.m) == 0 {
		w.running = false
		w.mu.Unlock()
		return false
	}
	w.mu.Unlock()
	for kind, f := range files { // stat outside the lock
		size, mod := statOf(f.path)
		if size == f.size && mod.Equal(f.mod) {
			continue
		}
		w.mu.Lock()
		if cur := w.m[kind]; cur != nil && cur.path == f.path {
			cur.size, cur.mod = size, mod
		}
		w.mu.Unlock()
		s.Changed(kind)
	}
	return true
}
