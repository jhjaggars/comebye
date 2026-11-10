package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// A notice is never delivered twice. herdr can fail agent.prompt after the
// text and Enter were written (a timeout, a dropped socket); the hub used
// to count that as a failed attempt and send the notice again 15 s later.
// The fake herdr here behaves that way: it appends the text to the target's
// session transcript, as the harness would, and then returns an error.

// arrivingThenError scripts agent.prompt: the text lands in the transcript
// file of the (single) target, and the call fails. Later calls succeed.
func arrivingThenError(fh *fakeLaunchHerdr, path string) {
	onPrompt(fh, map[int]promptAnswer{
		1: {err: &herdr.Error{Code: "internal_error", Message: "socket closed after write"}},
		0: {status: herdr.StatusWorking},
	}, func(f *fakeLaunchHerdr) {
		if len(f.calls) == 0 {
			return
		}
		last := f.calls[len(f.calls)-1]
		if last.Method != "agent.prompt" {
			return
		}
		text, _ := last.Params["text"].(string)
		fp, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		defer fp.Close()
		fp.WriteString(`{"type":"user","message":{"content":"` + strings.ReplaceAll(text, `"`, `\"`) + `"}}` + "\n")
	})
}

func TestCallbackNotRepeatedWhenSendErrorsAfterArrival(t *testing.T) {
	for _, kind := range []string{"claude", "pi"} {
		t.Run(kind, func(t *testing.T) {
			s, fh, _ := coordServer(t)
			s.noticeArriveWait = 20 * time.Millisecond
			path := filepath.Join(t.TempDir(), "session.jsonl")
			os.WriteFile(path, []byte(`{"type":"user","message":{"content":"earlier"}}`+"\n"), 0o600)
			arrivingThenError(fh, path)
			coord := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
			coord.Agent = kind
			coord.AgentSession = &herdr.AgentSessionInfo{Kind: "path", Value: path}
			e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, coord)

			s.deliverCallbacks([]herdr.AgentInfo{coord})
			waitFor(t, func() bool { return promptCount(fh) >= 1 })
			// The attempt errored but the text arrived: it is delivered.
			waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })

			// Later fleet updates (past the retry spacing) send nothing more.
			s.coord.mu.Lock()
			s.coord.calling = map[string]time.Time{}
			s.coord.mu.Unlock()
			s.deliverCallbacks([]herdr.AgentInfo{coord})
			time.Sleep(100 * time.Millisecond)
			if n := promptCount(fh); n != 1 {
				t.Fatalf("prompts = %d, want 1", n)
			}
		})
	}
}

// An error where the text did not arrive is still retried.
func TestCallbackRetriedWhenSendErrorsWithoutArrival(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.noticeArriveWait = 20 * time.Millisecond
	path := filepath.Join(t.TempDir(), "session.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o600)
	onPrompt(fh, map[int]promptAnswer{
		1: {err: &herdr.Error{Code: "internal_error", Message: "nope"}},
		0: {status: herdr.StatusWorking},
	}, nil)
	coord := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	coord.AgentSession = &herdr.AgentSessionInfo{Kind: "path", Value: path}
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, coord)
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { return promptCount(fh) >= 1 })
	time.Sleep(100 * time.Millisecond)
	s.coord.mu.Lock()
	s.coord.calling = map[string]time.Time{}
	s.coord.mu.Unlock()
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
	if n := promptCount(fh); n != 2 {
		t.Fatalf("prompts = %d, want 2", n)
	}
}

var _ = context.Background
