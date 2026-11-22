package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// "Let it clean up". When the only things a session says are left are
// cleanup items (work it can do itself once told to go ahead) or its own
// scratch, the review can send it that go-ahead: a message naming exactly
// the items it listed, asking it to do that and nothing else and to report.
// When it is idle again the hub asks "what is left to finish this work?"
// once more.
//
// The hub also sends it by itself (autoLetCleanup), once per answer, for
// the scratch part of any answer: the user's rule is that a session asking
// to delete its own scratch may always go ahead. Scratch is a cleanup item
// that names no PR, branch, push or issue, and a path or resource the
// session says is its own; anything it says to keep, or that is not its
// own, is never scratch. The rest of the answer stays as it is.

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
		if cleanable(v.AskItem) && v.State != itemResolved {
			out = append(out, cleanupText(v.AskItem))
		}
	}
	return out, ""
}

// scratchItems are the unresolved scratch items of a review, whatever else
// is left beside them: what the hub's own go-ahead covers.
func scratchItems(ar *askReview) []string {
	if ar == nil || ar.State != ledger.AskUnsafe {
		return nil
	}
	var out []string
	for _, v := range ar.Items {
		if isScratch(v.AskItem) && v.State != itemResolved {
			out = append(out, cleanupText(v.AskItem))
		}
	}
	return out
}

// cleanupText is an item as the go-ahead names it: the session's words,
// and for a path or resource what it is.
func cleanupText(it ledger.AskItem) string {
	switch it.Type {
	case ledger.ItemPath, ledger.ItemResource:
		if it.Was == "" || it.Was == it.Type {
			return strings.TrimSpace(itemLabel(it) + ": " + orDefault(it.Text, "(no detail given)"))
		}
	}
	return orDefault(it.Text, "(no detail given)")
}

var (
	// scratchOwnRe: the session says a path or resource is its own.
	scratchOwnRe = regexp.MustCompile(`(?i)\b(mine|my own|my|i (created|made|started|wrote|launched|ran|spawned|opened)|(created|started|made|opened) by me|scratch|throwaway|pre-edit|backup)\b`)
	// scratchKeepRe: words that make an item never scratch.
	scratchKeepRe = regexp.MustCompile(`(?i)\b(keep|kept|preserve|leave it|asked to|wants?|needs?|not mine|not my own|someone else|another (session|agent)|other (session|agent)s?|shared|credentials?|accounts?|secrets?|tokens?|keys?|password)\b|\bdo(n'?t| not) (delete|remove|stop|kill)\b`)
	// scratchWorkRe: cleanup that touches shared work. Only a person's
	// "Let it clean up" sends that.
	scratchWorkRe = regexp.MustCompile(`(?i)\b(prs?|pull requests?|branch(es)?|push|merge|issues?|commits?|tags?|releases?|remote|origin)\b|#\d`)
)

// cleanable: an item "Let it clean up" covers, cleanup or scratch.
func cleanable(item ledger.AskItem) bool { return isCleanup(item) || isScratch(item) }

// isScratch: an item that is the session's own throwaway: cleanup that names
// no shared work, or a path or resource it says it created; never one it
// says to keep or that is not its own.
func isScratch(item ledger.AskItem) bool {
	t := strings.ReplaceAll(item.Text, "`", " ")
	if scratchKeepRe.MatchString(t) {
		return false
	}
	switch item.Type {
	case ledger.ItemCleanup:
		return !scratchWorkRe.MatchString(t)
	case ledger.ItemPath, ledger.ItemResource:
		return item.Was == ledger.ItemCleanup || scratchOwnRe.MatchString(t)
	}
	return false
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
	items, why := cleanupItems(s.askReviewOf(ctx, it))
	if len(items) == 0 {
		return &errReask{"not_cleanup", why}
	}
	return s.sendCleanup(it, items, false)
}

// autoLetCleanup is the hub's own go-ahead for the scratch an answer still
// lists: once per answer, while the hub's re-asks last (the ask after the
// cleanup counts against reaskMax). It reports whether it sent one.
func (s *Server) autoLetCleanup(ctx context.Context, it *removal) bool {
	a := it.Mark.Ask
	if a == nil || a.State != ledger.AskUnsafe || a.Reasks >= s.reaskMax || a.ReaskedAt != nil || a.CleaningAt != nil ||
		(a.AutoCleanFor != nil && a.AutoCleanFor.Equal(a.At)) {
		return false
	}
	has := false
	for _, item := range a.Items {
		has = has || isScratch(retype(item, it.Branch))
	}
	if !has { // whether an item is scratch is in its words: no need to re-check the rest
		return false
	}
	items := scratchItems(s.askReviewOf(ctx, it))
	if len(items) == 0 {
		return false
	}
	if err := s.sendCleanup(it, items, true); err != nil {
		log.Printf("scratch go-ahead %s: %v", it.ID, err)
		return false
	}
	return true
}

// autoLetCleanupOf is autoLetCleanup for a marked agent by id.
func (s *Server) autoLetCleanupOf(ctx context.Context, id string) bool {
	for _, it := range s.marked() {
		if it.ID == id {
			return s.autoLetCleanup(ctx, it)
		}
	}
	return false
}

// sendCleanup records the go-ahead on the mark and sends it. auto: the
// hub's own, for the answer it was given for.
func (s *Server) sendCleanup(it *removal, items []string, auto bool) error {
	m := it.Mark
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
		if auto {
			if k.AutoCleanFor != nil && k.AutoCleanFor.Equal(k.At) {
				return false
			}
			at := k.At
			k.AutoCleanFor = &at
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
	s.setQuiet(m.AgentID, now.Add(s.cleanupWait+s.askWait+askQuietAfter))
	s.asking.Add(1)
	go func() {
		defer s.asking.Done()
		s.runCleanup(m, *a, items, auto)
	}()
	return nil
}

// runCleanup sends the go-ahead, waits for the session to be idle with a
// new reply, then asks what is left again.
func (s *Server) runCleanup(m ledger.Mark, a herdr.AgentInfo, items []string, auto bool) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cleanupWait+time.Minute)
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
	deadline := time.Now().Add(s.cleanupWait)
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
		if err := s.reask(cur, auto, "the cleanup you just did: is anything else left?"); err != nil {
			log.Printf("ask after cleanup %s: %v", m.AgentID, err)
		}
		return
	}
	if s.markCurrent(m) {
		finish("the session did not report its cleanup in time")
	}
}
