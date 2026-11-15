package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// "Let it clean up". When the only things a session says are left are
// cleanup items (work it can do itself once told to go ahead), the review
// can send it that go-ahead: a message naming exactly the cleanup it listed,
// asking it to do that and nothing else and to report. When it is idle again
// the hub asks "what is left to finish this work?" once more.

// cleanupWait bounds how long the hub waits for the session to finish its
// cleanup before it asks again anyway.
const cleanupWait = 15 * time.Minute

// cleanupPrompt is the go-ahead: the cleanup items, verbatim as the
// session listed them, and no licence for anything else.
func cleanupPrompt(items []string) string {
	var b strings.Builder
	b.WriteString("Go ahead with the cleanup you listed as left to finish. Do exactly this, nothing else:\n")
	for i, t := range items {
		fmt.Fprintf(&b, "%d. %s\n", i+1, t)
	}
	b.WriteString("Do not commit, push, merge or touch anything that is not listed. If a step cannot be done, or you find something else left, say so instead of improvising. " +
		"When you are done, report in a few lines what you did. You will then be asked once more what is left.")
	return b.String()
}

// cleanupItems are the unresolved cleanup items of a mark's review as the
// session worded them, or why there is nothing to send.
func cleanupItems(ar *askReview) ([]string, string) {
	if ar == nil || ar.State != ledger.AskUnsafe {
		return nil, "the session has not listed anything left"
	}
	if !ar.OnlyCleanup {
		return nil, "something besides cleanup is left"
	}
	var out []string
	for _, v := range ar.Items {
		if v.Type == ledger.ItemCleanup && v.State != itemResolved {
			out = append(out, orDefault(v.Text, "(no detail given)"))
		}
	}
	return out, ""
}

// handleLetCleanup is POST /api/marks/cleanup {agent_id}: tell a marked
// session whose only items left are cleanup to do exactly that and report.
func (s *Server) handleLetCleanup(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b reaskBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil || b.AgentID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	var it *removal
	for _, x := range s.marked() {
		if x.ID == b.AgentID {
			it = x
		}
	}
	if it == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "not_marked", "message": "that session is not marked ready to remove"})
		return
	}
	if err := s.letCleanup(r.Context(), it); err != nil {
		var re *errReask
		if errors.As(err, &re) {
			writeJSONStatus(w, http.StatusConflict, map[string]string{"code": re.code, "message": re.msg})
			return
		}
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "ledger_error", "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"cleaning": true})
}

func (s *Server) letCleanup(ctx context.Context, it *removal) error {
	m := it.Mark
	items, why := cleanupItems(s.askReviewOf(ctx, it))
	if len(items) == 0 {
		return &errReask{"not_cleanup", why}
	}
	a := s.liveAgent(m.AgentID)
	switch {
	case a == nil:
		return &errReask{"gone", "the session is gone"}
	case a.Status != herdr.StatusIdle && a.Status != herdr.StatusDone:
		return &errReask{"busy", "the session is " + string(a.Status) + ": tell it when it is idle"}
	case m.Ask.ReaskedAt != nil || m.Ask.CleaningAt != nil:
		return &errReask{"asking", "the session is already being asked or cleaning up"}
	}
	now := time.Now().UTC()
	changed, err := s.ledger.UpdateAsk(m.AgentID, m.At, func(k *ledger.Ask) bool {
		if k.ReaskedAt != nil || k.CleaningAt != nil {
			return false
		}
		k.CleaningAt, k.ReaskNote = &now, ""
		return true
	})
	if err != nil {
		return err
	}
	if !changed {
		return &errReask{"not_marked", "that session is no longer marked"}
	}
	s.setQuiet(m.AgentID, now.Add(cleanupWait+s.askWait+askQuietAfter))
	s.asking.Add(1)
	go func() {
		defer s.asking.Done()
		s.runCleanup(m, *a, items)
	}()
	return nil
}

// runCleanup sends the go-ahead, waits for the session to be idle with a
// new reply, then asks what is left again.
func (s *Server) runCleanup(m ledger.Mark, a herdr.AgentInfo, items []string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupWait+time.Minute)
	defer cancel()
	finish := func(note string) {
		_, err := s.ledger.UpdateAsk(m.AgentID, m.At, func(k *ledger.Ask) bool {
			k.CleaningAt, k.ReaskNote = nil, note
			return true
		})
		if err != nil {
			log.Printf("ledger: %v", err)
		}
	}
	before := s.latestOf(ctx, a.Host, a.PaneID)
	if err := s.promptHeld(ctx, a, cleanupPrompt(items)); err != nil {
		finish("could not tell it to clean up: " + err.Error())
		return
	}
	deadline := time.Now().Add(cleanupWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
		case <-time.After(s.askPoll):
		}
		if !s.markCurrent(m) {
			return
		}
		live := s.liveAgent(m.AgentID)
		if live == nil {
			finish("the session went away while cleaning up")
			return
		}
		if live.Status != herdr.StatusIdle && live.Status != herdr.StatusDone {
			continue
		}
		l := s.latestOf(ctx, a.Host, a.PaneID)
		if l == nil || l.Text == "" || (before != nil && l.Text == before.Text && l.Time == before.Time) {
			continue
		}
		finish("")
		cur := m
		if e, ok := s.ledger.ByAgent(m.AgentID); ok && e.Mark != nil && e.Mark.At.Equal(m.At) {
			cur = *e.Mark
		} else {
			for _, sm := range s.ledger.Marks() {
				if sm.AgentID == m.AgentID && sm.At.Equal(m.At) {
					cur = sm
				}
			}
		}
		if err := s.reask(cur, false, "the cleanup you just did: is anything else left?"); err != nil {
			log.Printf("ask after cleanup %s: %v", m.AgentID, err)
		}
		return
	}
	if s.markCurrent(m) {
		finish("the session did not report its cleanup in time")
	}
}
