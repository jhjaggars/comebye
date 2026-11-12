package server

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/opencode"
)

// OpenCode keeps its sessions in a database, so each live OpenCode agent's
// session is mirrored as a file in Pi's format (internal/opencode); the
// chat view, the archive, recall and usage then read it like any other.

// opencodeMirrorEvery is how often live OpenCode sessions are re-exported
// in the background.
const opencodeMirrorEvery = time.Minute

var (
	ocMirrorOnce sync.Once
	ocMirror     *opencode.Mirror
)

// opencodeMirror is this host's mirror (the program is found like the
// agent kinds are: PATH, then the usual per-user directories).
func (s *Server) opencodeMirror() *opencode.Mirror {
	ocMirrorOnce.Do(func() {
		ocMirror = &opencode.Mirror{Dir: s.cfg.OpencodeDir, Bin: lookExec("opencode")}
		if ocMirror.Dir == "" {
			ocMirror.Dir = opencode.DefaultDir()
		}
	})
	return ocMirror
}

// opencodeSessionID is the OpenCode session herdr reports for a, "" when it
// reports none.
func opencodeSessionID(a herdr.AgentInfo) string {
	if a.Agent != opencode.Kind || a.AgentSession == nil || a.AgentSession.Kind != "id" || !opencode.ValidID(a.AgentSession.Value) {
		return ""
	}
	return a.AgentSession.Value
}

// syncOpencode refreshes the mirror of a's session (an OpenCode agent on
// this host), at once when force is set.
func (s *Server) syncOpencode(ctx context.Context, a herdr.AgentInfo, force bool) {
	id := opencodeSessionID(a)
	if id == "" || lookExec("opencode") == "" {
		return
	}
	m := s.opencodeMirror()
	if force {
		m.Forget(id)
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if _, err := m.Sync(ctx, id); err != nil {
		s.logOpencode(id, err)
	}
}

var ocLogged sync.Map

func (s *Server) logOpencode(id string, err error) {
	if _, dup := ocLogged.LoadOrStore(id+err.Error(), true); !dup {
		log.Printf("opencode mirror %s: %v", id, err)
	}
}

// RunOpencodeMirror keeps the mirror of every live OpenCode agent here up to
// date until ctx ends, so the archive and usage see sessions as they go.
func (s *Server) RunOpencodeMirror(ctx context.Context) {
	for {
		for _, a := range s.manager.Agents() {
			if ctx.Err() != nil {
				return
			}
			s.syncOpencode(ctx, a, false)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(opencodeMirrorEvery):
		}
	}
}
