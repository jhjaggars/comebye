package server

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// Asking a marked session whether it is safe to remove. A coordinator can
// see the git checks but not what only the session knows (an open PR it
// is watching, a loop it runs, work it was told to keep), so a mark of a
// live, idle session sends it askPrompt and records the first word of its
// reply on the mark: SAFE, or UNSAFE and why. No reply within askWait is
// "no answer", never safe. A working or blocked session is not asked.

const (
	askPoll = 3 * time.Second
	askWait = 2 * time.Minute
	// askQuietAfter keeps the answer from notifying the user when the
	// status that carries it arrives after the answer was read (a
	// "needs input" waits out its delay first).
	askQuietAfter = time.Minute
	maxAskReason  = 300
)

const askPrompt = "A coordinator proposes removing this session and its worktree. " +
	"Do not take any other action: no commits, pushes, cleanup or other commands. " +
	"Reply with one line that starts with SAFE, or with UNSAFE followed by the reason " +
	"(unmerged or unpushed work, an open PR you are watching, a running loop, a pending request or follow-up you owe the user)."

// startAsk is a new mark's first answer: asking (and the asking started),
// or not asked and why.
func (s *Server) startAsk(m ledger.Mark, a *herdr.AgentInfo) *ledger.Ask {
	now := time.Now().UTC()
	switch {
	case a == nil:
		return &ledger.Ask{State: ledger.AskNotAsked, Reason: "the session is gone", At: now}
	case a.Status == herdr.StatusWorking || a.Status == herdr.StatusBlocked:
		return &ledger.Ask{State: ledger.AskNotAsked, Reason: "the session was " + string(a.Status), At: now}
	}
	s.setQuiet(m.AgentID, now.Add(s.askWait+askQuietAfter))
	go s.runAsk(m, *a)
	return &ledger.Ask{State: ledger.AskAsking, AskedAt: &now, At: now}
}

// runAsk prompts the session and waits for its reply.
func (s *Server) runAsk(m ledger.Mark, a herdr.AgentInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), s.askWait+time.Minute)
	defer cancel()
	asked := time.Now().UTC()
	set := func(state, reason string) {
		if _, err := s.ledger.SetAsk(m.AgentID, m.At, ledger.Ask{State: state, Reason: reason, AskedAt: &asked}); err != nil {
			log.Printf("ledger: %v", err)
		}
		s.setQuiet(m.AgentID, time.Now().Add(askQuietAfter))
	}
	before := s.latestOf(ctx, a.Host, a.PaneID)
	if err := s.promptAgent(ctx, a, askPrompt); err != nil {
		set(ledger.AskNotAsked, "could not prompt it: "+err.Error())
		return
	}
	deadline := time.Now().Add(s.askWait)
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
			set(ledger.AskNoAnswer, "the session went away")
			return
		}
		if live.Status != herdr.StatusIdle && live.Status != herdr.StatusDone {
			continue
		}
		l := s.latestOf(ctx, a.Host, a.PaneID)
		if l == nil || l.Text == "" || (before != nil && l.Text == before.Text && l.Time == before.Time) {
			continue
		}
		set(parseAnswer(l.Text))
		return
	}
	if s.markCurrent(m) {
		set(ledger.AskNoAnswer, "")
	}
}

// parseAnswer reads a reply's first word: SAFE, or UNSAFE and the rest
// as the reason; anything else is unclear, with the reply as its reason.
func parseAnswer(text string) (state, reason string) {
	t := strings.TrimSpace(text)
	word, rest, _ := strings.Cut(t, " ")
	if i := strings.IndexAny(word, "\n"); i >= 0 {
		word, rest = word[:i], word[i+1:]+" "+rest
	}
	word = strings.TrimFunc(word, func(r rune) bool { return !unicode.IsLetter(r) })
	rest = strings.TrimLeftFunc(rest, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(":-–—,.", r) })
	switch strings.ToUpper(word) {
	case "SAFE":
		return ledger.AskSafe, clip(rest, maxAskReason)
	case "UNSAFE":
		return ledger.AskUnsafe, clip(rest, maxAskReason)
	}
	return ledger.AskUnclear, clip(t, maxAskReason)
}

// clip collapses whitespace and cuts s to n bytes on a rune boundary.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// markCurrent reports whether m is still the agent's mark.
func (s *Server) markCurrent(m ledger.Mark) bool {
	if e, ok := s.ledger.ByAgent(m.AgentID); ok && e.Status == ledger.ReadyToRemove && e.Mark != nil {
		return e.Mark.At.Equal(m.At)
	}
	for _, sm := range s.ledger.Marks() {
		if sm.AgentID == m.AgentID {
			return sm.At.Equal(m.At)
		}
	}
	return false
}

func (s *Server) liveAgent(id string) *herdr.AgentInfo {
	for _, a := range s.fleet.Agents() {
		if a.ID == id {
			return &a
		}
	}
	return nil
}

// setQuiet keeps an agent's notifications off until until.
func (s *Server) setQuiet(id string, until time.Time) {
	s.coord.mu.Lock()
	defer s.coord.mu.Unlock()
	now := time.Now()
	for k, t := range s.coord.quiet {
		if now.After(t) {
			delete(s.coord.quiet, k)
		}
	}
	s.coord.quiet[id] = until
}

// quietAgent is the dispatcher's Quiet: a session being asked, or just
// asked.
func (s *Server) quietAgent(key string) bool {
	s.coord.mu.Lock()
	defer s.coord.mu.Unlock()
	t, ok := s.coord.quiet[key]
	return ok && time.Now().Before(t)
}

// endStaleAsks records "no answer" for asks a restart interrupted.
func (s *Server) endStaleAsks() {
	var marks []ledger.Mark
	for _, e := range s.ledger.Entries() {
		if e.Status == ledger.ReadyToRemove && e.Mark != nil {
			marks = append(marks, *e.Mark)
		}
	}
	marks = append(marks, s.ledger.Marks()...)
	for _, m := range marks {
		if m.Ask != nil && m.Ask.State == ledger.AskAsking {
			if _, err := s.ledger.SetAsk(m.AgentID, m.At, ledger.Ask{State: ledger.AskNoAnswer, Reason: "Comebye restarted while asking", AskedAt: m.Ask.AskedAt}); err != nil {
				log.Printf("ledger: %v", err)
			}
		}
	}
}
