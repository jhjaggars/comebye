package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// The sweep loop's leftovers. A finished session whose agent is gone cannot
// answer "Let it clean up" or "Re-ask", so a "LEFT" answer it gave before it went
// would sit in the removals review for ever. For such sessions the loop looks at
// the facts on the host instead of asking:
//
//   - every item of the answer that has since resolved (a PR merged, a to-do
//     closed, a branch gone from origin) is re-checked each sweep, and when all
//     have, the answer becomes NOTHING LEFT, with a note of what the loop did;
//   - an item that is "delete the remote copy of my own branch" is done when git
//     proves every commit of that branch is in origin's default branch and no
//     pull request from it is open (stalebranch.go). With "Clean up
//     automatically" on (the default) the loop deletes it itself, from the
//     session's host, with the host's own git credentials, and logs the branch
//     and the SHA it was at so it can be pushed back; off, the branch stays;
//   - an item that is only "delete my scratch directory" under the session's own
//     Claude scratch path (/tmp/claude-<uid>/<its worktree>) is done by the
//     removal itself, which deletes that directory (scratchItem);
//   - anything else (a process to stop, a PR to close, a box to tick, a backup
//     branch the hub cannot prove is in main) is left as it is. The agent that
//     could do it is gone, and doing it from here would write outside the
//     session's own branch and local copy: the session stays in the removals
//     review, unremoved and with no to-do, and the user decides.

// answerSafeBySweep records that every leftover of a gone session's answer has
// resolved: the answer becomes NOTHING LEFT, and says who changed it.
func (s *Server) answerSafeBySweep(it *removal, what string, now time.Time) {
	m := it.Mark
	text := what
	changed, err := s.ledger.UpdateAsk(m.AgentID, m.At, func(k *ledger.Ask) bool {
		if k.State != ledger.AskUnsafe {
			return false
		}
		k.State, k.Reason, k.Items = ledger.AskSafe, clip("re-checked by the sweep: "+text, maxAskReason), nil
		k.Loop = &ledger.LoopNote{At: now.UTC(), Text: clip(text, 300)}
		return true
	})
	if err != nil {
		log.Printf("ledger: %v", err)
		return
	}
	if changed {
		s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.leftovers_resolved", Host: it.Host, OK: true,
			Detail: fmt.Sprintf("%s (%s): %s", orDefault(it.Name, it.EntryID), it.EntryID, text)})
	}
}

// deleteStaleFor deletes the stale remote branches a gone session's answer
// still lists, on its host, when they are all it has left. by says who asked
// ("the sweep", "your confirm"). It reports the branches deleted; an error means
// one could not be (nothing else is done then).
func (s *Server) deleteStaleFor(ctx context.Context, it *removal, by string, now time.Time) ([]sweepDone, error) {
	m := it.Mark
	if sessionBusy(it) || m.Ask == nil || m.Ask.State != ledger.AskUnsafe || it.EntryID == "" || !s.knownHost(it.Host) {
		return nil, nil
	}
	ar := s.askReviewOf(ctx, it)
	if ar == nil || !ar.StaleOnly {
		return nil, nil
	}
	type did struct {
		i      int
		branch string
		how    string
		tip    string // the tip it was deleted at (empty: it was already gone)
	}
	var done []did
	for i, v := range ar.Items {
		if !v.Stale {
			continue
		}
		branch, ok := remoteBranchItem(v.Text, it.Branch)
		if !ok {
			continue
		}
		r, _ := s.staleProbe(ctx, it, branch, true)
		switch {
		case r != nil && r.Deleted:
			done = append(done, did{i, branch, fmt.Sprintf("deleted origin/%s (was %s; restore with git push origin %s:refs/heads/%s): %s", branch, r.Tip, r.Tip, branch, r.Proof), r.Tip})
		case r != nil && r.State == staleAbsent:
			done = append(done, did{i, branch, "origin no longer had " + branch, ""})
		default:
			why := "no answer"
			if r != nil {
				why = orDefault(r.Why, r.State)
			}
			return nil, fmt.Errorf("origin/%s was not deleted: %s", branch, why)
		}
	}
	if len(done) == 0 {
		return nil, nil
	}
	var out []sweepDone
	var hows []string
	for _, d := range done {
		hows = append(hows, d.how)
		if d.tip != "" {
			out = append(out, sweepDone{At: now.UTC(), Kind: doneBranch, Name: orDefault(it.Name, it.EntryID), Repo: it.Repo, Host: it.Host,
				Branch: d.branch, Tip: d.tip, Restore: restoreBranchCmd(d.tip, d.branch)})
		}
	}
	text := by + " " + strings.Join(hows, "; ")
	allDone := len(done) == countUnresolved(ar)
	if _, err := s.ledger.UpdateAsk(m.AgentID, m.At, func(k *ledger.Ask) bool {
		for _, d := range done {
			if d.i < len(k.Items) {
				k.Items[d.i].Done = &ledger.ItemDone{At: now.UTC(), How: d.how}
			}
		}
		k.Loop = &ledger.LoopNote{At: now.UTC(), Text: clip(text, 300)}
		if allDone && by == "the sweep" {
			k.State, k.Reason = ledger.AskSafe, clip("re-checked by the sweep: "+text, maxAskReason)
		}
		return true
	}); err != nil {
		log.Printf("ledger: %v", err)
	}
	s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.stale_branch_delete", Host: it.Host, OK: true,
		Detail: fmt.Sprintf("%s (%s): %s", orDefault(it.Name, it.EntryID), it.EntryID, text)})
	return out, nil
}

func countUnresolved(ar *askReview) int {
	n := 0
	for _, v := range ar.Items {
		if v.State != itemResolved {
			n++
		}
	}
	return n
}

// leftovers is one pass over the marked sessions whose agent is gone. It
// returns how many answers it settled and the branches it deleted. del is the
// "clean up automatically" setting: off, a stale branch is left alone.
func (s *Server) leftovers(ctx context.Context, now time.Time, del bool) (settled int, deleted []sweepDone) {
	auto := del
	for _, it := range s.marked() {
		a := it.Mark.Ask
		if sessionBusy(it) || it.Coordinator || it.Mark.Discard || it.EntryID == "" || a == nil || a.State != ledger.AskUnsafe ||
			a.ReaskedAt != nil || a.CleaningAt != nil || !s.knownHost(it.Host) {
			continue
		}
		ar := s.askReviewOf(ctx, it)
		switch {
		case ar == nil:
		case ar.Resolved:
			s.answerSafeBySweep(it, "every thing the session listed as left has resolved: "+strings.TrimPrefix(ar.said(), "the session listed what was left at "), now)
			settled++
		case auto && ar.StaleOnly:
			got, err := s.deleteStaleFor(ctx, it, "the sweep", now)
			if err != nil {
				log.Printf("sweep leftovers: %s: %v", it.ID, err)
				continue
			}
			if len(got) > 0 {
				settled++
				deleted = append(deleted, got...)
			}
		}
	}
	return settled, deleted
}

// staleDeletions are the stale remote branches a reviewed removal's
// confirmation would delete.
func staleDeletions(it *removal) []string {
	var out []string
	if ar := it.AskReview; ar != nil && ar.StaleOnly {
		for _, v := range ar.Items {
			if v.Stale {
				if b, ok := remoteBranchItem(v.Text, it.Branch); ok {
					out = append(out, "origin/"+b)
				}
			}
		}
	}
	return out
}

// sessionBusy: the agent is working or blocked, so nothing is done around it.
func sessionBusy(it *removal) bool {
	return it.Live.Present && (it.Live.Status == herdr.StatusWorking || it.Live.Status == herdr.StatusBlocked)
}
