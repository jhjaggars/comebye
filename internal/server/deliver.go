package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Verified delivery of a first prompt: a handoff's brief, a launch's
// first prompt. herdr's agent.prompt reports success once the text and
// Enter are written to the pane, which does not prove the agent read
// them — a harness that is still starting up can take the input and then
// silently discard it (pi accepts keystrokes after its session_start
// event but before its input loop begins; a prompt landing in that
// window leaves an idle agent with no transcript and no error anywhere —
// see docs/investigations/brief-delivery-race.md). So a first prompt is
// sent with a wait for the agent to react, and an unverified send is
// retried — but only after checking that it did not arrive late, so a
// slow agent is never handed the same brief twice.

const (
	// markerLen is how much of a prompt's first line identifies it in the
	// agent's session transcript.
	markerLen = 48
	// markerRead caps how much of the transcript is read looking for the
	// marker: the prompt is at the start of a fresh session's file.
	markerRead = 1 << 20
)

// promptMarker is the start of text as it appears in the agent's session
// transcript once text was taken: the beginning of the first non-empty
// line, cut before any character that JSON string encoding would escape,
// so it can be searched for in the raw transcript file. "" when the text
// has no usable plain prefix.
func promptMarker(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		end := len(line)
		for i, r := range line {
			if r == '"' || r == '\\' || r < 0x20 {
				end = i
				break
			}
		}
		marker := strings.TrimSpace(line[:end])
		if len(marker) > markerLen {
			marker = marker[:markerLen]
		}
		if len(marker) >= 6 {
			return marker
		}
	}
	return ""
}

// deliverPrompt sends text to the named agent and verifies the agent took
// it, retrying a bounded number of times. It returns nil only once the
// agent was observed working (or blocked) on the prompt, or the prompt
// turned up in its session transcript; a late arrival counts as delivered
// and is never sent twice.
func (s *Server) deliverPrompt(ctx context.Context, name, text string) error {
	marker := promptMarker(text)
	var last error
	for try := 1; try <= s.briefTries; try++ {
		if try > 1 {
			// The earlier send may have arrived late; check before
			// resending so the prompt is not delivered twice.
			if ok, _ := s.briefArrived(ctx, name, marker); ok {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("not delivered: %w", ctx.Err())
			case <-time.After(s.briefBackoff):
			}
		}
		a, err := s.client.Prompt(ctx, name, text, herdr.PromptOptions{
			// The wait makes the answer say whether the agent took the
			// prompt: herdr's activity gate returns agent_prompt_stalled
			// when no working/blocked activity follows the submission.
			Wait:      true,
			Until:     []herdr.AgentStatus{herdr.StatusWorking, herdr.StatusBlocked},
			TimeoutMs: s.briefWait.Milliseconds(),
		})
		switch {
		case err == nil:
			switch a.Status {
			case herdr.StatusWorking, herdr.StatusBlocked, herdr.StatusDone:
				return nil // the agent took it
			}
			last = fmt.Errorf("the agent is still %s", a.Status)
		default:
			last = err
			var he *herdr.Error
			if !errors.As(err, &he) {
				break
			}
			switch he.Code {
			case "agent_blocked":
				// A dialog is open; sending again cannot help.
				return err
			case "agent_name_not_active", "agent_not_found":
				// The launch deactivated between the ready wait and the
				// send; wait for it to be promptable again before the
				// next try instead of failing.
				if werr := s.waitForNamedAgent(ctx, name); werr != nil {
					last = werr
				}
			}
		}
		// Not verified. agent_prompt_stalled and timeout mean the text
		// was written but the agent was not observed to take it — it may
		// still arrive (the harness finishing its startup): check before
		// deciding to send again.
		if ok, _ := s.briefArrived(ctx, name, marker); ok {
			return nil
		}
	}
	return fmt.Errorf("the agent did not take it after %d tries: %v", s.briefTries, last)
}

// briefArrived reports whether the agent has already taken its prompt: it
// is working or blocked, or its session transcript exists and contains
// the prompt's marker. herdr's answer to agent.prompt does not prove the
// agent read the text, and a stalled or timed-out wait does not prove it
// did not (the text may arrive late) — this is the check that tells the
// two apart before a retry sends the prompt again. An agent whose session
// cannot be resolved, or a transcript that cannot be read (not written
// yet), simply counts as not arrived.
func (s *Server) briefArrived(ctx context.Context, name, marker string) (bool, error) {
	a, err := s.client.GetAgent(ctx, name)
	if err != nil {
		return false, err
	}
	switch a.Status {
	case herdr.StatusWorking, herdr.StatusBlocked:
		return true, nil
	}
	if marker == "" {
		return false, nil
	}
	path, err := transcript.SessionPath(a, nil)
	if err != nil {
		return false, nil
	}
	b, err := readFileHead(path, markerRead)
	if err != nil {
		return false, nil
	}
	return bytes.Contains(b, []byte(marker)), nil
}

// readFileHead reads at most limit bytes from the start of path.
func readFileHead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}
