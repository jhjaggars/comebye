package server

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jhjaggars/comebye/internal/archive"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// archiveFlushWait bounds how long a removal waits for the final flush.
// A flush that takes longer carries on in the background; the removal does
// not wait for it, and the next `comebye archive run` finishes any rest.
const archiveFlushWait = 20 * time.Second

// archiveFlush copies the agent's session files into the archive, including
// a last unterminated line. It reports what happened in a short phrase for
// the removal result; it never fails the removal.
func (s *Server) archiveFlush(agent *herdr.AgentInfo, live []herdr.AgentInfo) string {
	if s.cfg.Archive == nil || agent == nil {
		return ""
	}
	kind := ""
	switch agent.Agent {
	case "claude":
		kind = "claude"
	case "pi":
		kind = "pi"
	case "opencode":
		kind = "opencode"
		s.syncOpencode(context.Background(), *agent, true)
	default:
		return ""
	}
	path, err := transcript.SessionPath(*agent, live)
	if err != nil {
		return "not archived: " + err.Error()
	}
	var src *archive.Source
	for i := range s.cfg.ArchiveSources {
		if s.cfg.ArchiveSources[i].Kind == kind {
			src = &s.cfg.ArchiveSources[i]
		}
	}
	if src == nil {
		return ""
	}
	type outcome struct {
		res archive.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := s.cfg.Archive.Flush(*src, path)
		done <- outcome{r, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			log.Printf("archive: flush for %s: %v", agent.PaneID, o.err)
			return "archive failed: " + o.err.Error()
		}
		if len(o.res.Errors) > 0 {
			log.Printf("archive: flush for %s: %v", agent.PaneID, o.res.Errors)
			return "archive incomplete: " + o.res.Errors[0]
		}
		return fmt.Sprintf("%d segments, %d bytes", o.res.Segments, o.res.RawBytes)
	case <-time.After(archiveFlushWait):
		return "archive still running in the background"
	}
}
