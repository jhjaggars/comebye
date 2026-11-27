package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
)

// Automatic removal. A worker that is marked ready to remove (by its
// coordinator or by a sweep) and whose every check is clean is removed by the
// hub without the user's tap, so the removals review shows only the ones that
// need them. "Clean" is decided here and nowhere else (autoRemoveVerdict) and
// is rechecked at removal time:
//
//   - the work is in the base: every branch commit has an equivalent on
//     origin/<default> (git cherry, after a fetch that worked; a checkout's
//     own, possibly stale, main is not trusted), or the branch has no commits
//     (a research / Page-only handoff), or its pull request was merged, or
//     each commit git cherry cannot match was rebased onto origin/<default>
//     and every merge on the branch is clean (rebasedProof);
//   - nothing is unpushed, and the worktree has no uncommitted or untracked
//     files (ignored files do not count);
//   - the session answered SAFE (not "did not answer", UNSAFE or unchecked);
//   - no open to-do waits on the agent for an answer (a follow-up to-do is
//     the user's and stays in their list) and it has no queued prompts;
//   - it is not a coordinator, and is not working, blocked or running a
//     background task.
//
// Anything else stays in the review exactly as before. The removal itself is
// the ordinary one (removeSession), which keeps the worktree whenever anything
// is unmerged; when it removes the worktree, the local branch goes with it,
// since every commit is in the base. The remote branch is not touched. Each
// auto-removal is audited, reported to the coordinator in the digest, and
// listed in the review for a day with an Undo (handleUndoAutoRemove: the
// session is resumed from its park record, which recreates the worktree from
// the branch or from where the work landed, else a fresh worker is started).

const (
	// autoRemoveEvery is how often one marked worker is looked at again.
	autoRemoveEvery = 3 * time.Minute
	// AutoRemoveWindow is how long the review keeps showing an auto-removal.
	AutoRemoveWindow = 24 * time.Hour
	autoRemoveReason = "landed, clean, SAFE"
)

// autoFacts are what the verdict needs from outside the removal.
type autoFacts struct {
	// TodoWaiting: an open to-do waits on the agent for its answer (a
	// follow-up does not: it is the user's alone). Queued: prompts queued
	// for it. Background: tasks it runs in the background.
	TodoWaiting bool
	Queued      int
	Background  int
	// AnswerTTL, with Now, makes an old SAFE answer not clean: the session is
	// asked again instead (autoRemoveOne). Zero turns it off.
	AnswerTTL time.Duration
	Now       time.Time
}

// autoRemoveVerdict says whether a reviewed removal (review has run, with the
// origin check asked for) may be removed without the user's tap; when not,
// why not. It is the single place the rule lives.
func autoRemoveVerdict(it *removal, f autoFacts) (ok bool, why string) {
	m := it.Mark
	switch {
	case it.EntryID == "":
		return false, "not a handoff worker"
	case it.Coordinator:
		return false, "it is a coordinator"
	case !it.Live.Present:
		return false, "the session is not running: there is nothing to keep a record of for a resume"
	case it.Live.Present && (it.Live.Status == herdr.StatusWorking || it.Live.Status == herdr.StatusBlocked):
		return false, "the agent is " + string(it.Live.Status)
	case f.Background > 0:
		return false, "it has a background task running"
	case f.TodoWaiting:
		return false, "a to-do is waiting on it"
	case f.Queued > 0:
		return false, "it has queued prompts"
	case m.Discard:
		return false, "the mark proposes discarding the work"
	case m.Ask == nil || m.Ask.State != ledger.AskSafe:
		return false, "the session has not said nothing is left"
	case f.AnswerTTL > 0 && !m.Ask.At.IsZero() && f.Now.Sub(m.Ask.At) >= f.AnswerTTL && !hubSettled(m.Ask):
		return false, "the NOTHING LEFT answer is " + agoText(f.Now, m.Ask.At) + ": too old to trust"
	case m.Ask.ReaskedAt != nil:
		return false, "the session is being asked again"
	}
	return workProven(it)
}

// workProven is the part of the verdict that is about the work itself, from
// the host's checks and not from anything the session said: the worktree is
// clean and in a state to remove, every commit is in the base (or its pull
// request merged), and nothing is unpushed. Both the automatic removal of a
// session that said SAFE and the sweep loop's clean-up (which decides for a
// session whose agent is gone) use it.
func workProven(it *removal) (ok bool, why string) {
	c := it.Checks
	switch {
	case it.Worktree == "" || it.ChecksError != "" || c == nil:
		return false, "its worktree could not be checked"
	case !c.Exists && c.Gone:
		return goneProven(it)
	case !c.Exists || c.Primary || c.Error != "" || c.InProgress != "":
		return false, "its worktree is not in a state to remove"
	case c.Dirty || c.DirtyFiles > 0:
		return false, "the worktree has uncommitted or untracked files"
	case len(it.Warnings) > 0:
		return false, it.Warnings[0]
	}
	o := c.Origin
	switch {
	case o == nil:
		return false, "the host did not compare with origin"
	case !o.Records:
		return false, "the host cannot keep a record to resume the session from"
	case !o.Fetched || o.Ref == "":
		return false, "not compared with origin: " + o.Why
	case o.Unmerged < 0:
		return false, "could not tell whether its commits are in " + o.Ref
	case o.Unmerged > 0 && o.MergedPR == 0 && o.Rebased == "":
		return false, fmt.Sprintf("%d commits not in %s", o.Unmerged, o.Ref)
	}
	d := c.Detail
	switch {
	case d == nil || d.Unpushed < 0:
		return false, "could not tell whether anything is unpushed"
	case d.Unpushed > 0 && o.MergedPR == 0 && o.Rebased == "":
		return false, fmt.Sprintf("%d commits on no remote", d.Unpushed)
	}
	return true, ""
}

// goneProven is workProven for a worktree that is already gone: nothing is
// left on disk, so what matters is that nothing of the work exists only in
// what was removed. The rule is the review's (goneVerdict: every recorded
// commit, of the branch if it is still there or the ones the ledger kept,
// is in the base), held to origin's copy of the base, not the checkout's
// own main. Removing it closes the session and clears the record; no
// worktree or branch is touched.
func goneProven(it *removal) (ok bool, why string) {
	c := it.Checks
	if warn, msg := c.goneVerdict(); warn {
		return false, msg
	}
	switch {
	case len(it.Warnings) > 0:
		return false, it.Warnings[0]
	case len(c.Records) > 0 && c.GoneInOrigin == "":
		return false, "the worktree is gone and its commits are not all in origin's copy of " + orDefault(c.BaseUsed, "the base")
	}
	return true, ""
}

// workGone: the review found the worktree already gone.
func workGone(it *removal) bool {
	return it.Checks != nil && !it.Checks.Exists && it.Checks.Gone
}

// inBaseHow says how the work is in the base, for the audit entry.
func inBaseHow(c *worktreeChecks) string {
	o := c.Origin
	switch {
	case !c.Exists && c.Gone && len(c.Records) == 0:
		return "the worktree and branch were already removed"
	case !c.Exists && c.Gone:
		return "the worktree was already gone; every commit of " + strings.Join(c.Records, ", ") + " is in " + c.GoneInOrigin
	case o == nil:
		return ""
	case o.Unmerged == 0 && len(c.Landed) == 0:
		return "no commits beyond " + o.Ref
	case o.Unmerged == 0:
		return "every commit is in " + o.Ref
	case o.MergedPR > 0:
		return fmt.Sprintf("merged as PR #%d", o.MergedPR)
	case o.Rebased != "":
		return o.Rebased
	}
	return ""
}

// autoFactsOf gathers the outside facts about an agent id. Its background
// tasks (and scheduled wakeups) are read from the session now, not from the
// agent list's cache, which has nothing until its first read lands.
func (s *Server) autoFactsOf(ctx context.Context, it *removal) autoFacts {
	f := autoFacts{AnswerTTL: s.reaskCap, Now: time.Now()}
	if s.todos != nil {
		for _, t := range s.todos.list("") {
			if todoNeedsSession(t, it.ID) {
				f.TodoWaiting = true
			}
		}
	}
	if s.pq != nil {
		f.Queued = len(s.pq.list(it.ID, false))
	}
	for _, a := range s.fleet.Agents() {
		if a.ID == it.ID || (a.Host == it.Host && a.PaneID == it.PaneID && it.PaneID != "") {
			f.Background = len(s.backgroundOfAgent(a))
		}
	}
	if it.Live.Present {
		f.Background = max(f.Background, len(s.pendingTasks(ctx, it.Host, it.ID, it.PaneID)))
	}
	return f
}

// autoRemoveCandidate is the cheap filter before any git runs: a ledger
// worker marked with a SAFE answer.
func autoRemoveCandidate(it *removal) bool {
	m := it.Mark
	return it.EntryID != "" && !m.Discard && m.Ask != nil && m.Ask.State == ledger.AskSafe &&
		!it.Coordinator && !(it.Live.Present && (it.Live.Status == herdr.StatusWorking || it.Live.Status == herdr.StatusBlocked))
}

// autoRemoveStep looks at every marked worker once and removes the clean
// ones; it returns the ids removed. Each worker is looked at at most once per
// autoRemoveEvery.
func (s *Server) autoRemoveStep(ctx context.Context, now time.Time) []string {
	if !s.autoRemove || s.ledger == nil {
		return nil
	}
	var removed []string
	for _, it := range s.marked() {
		if autoRemoveGone(it) && !s.knownHost(it.Host) {
			if s.autoRemoveGoneOne(ctx, it) {
				removed = append(removed, it.ID)
			}
			continue
		}
		if !autoRemoveCandidate(it) || s.fleet.PeerDown(it.Host) != "" {
			continue
		}
		key := "autorm/" + it.ID
		s.coord.mu.Lock()
		last, seen := s.coord.calling[key]
		if (seen && now.Sub(last) < autoRemoveEvery) || s.coord.checking[key] {
			s.coord.mu.Unlock()
			continue
		}
		s.coord.calling[key] = now
		s.coord.checking[key] = true
		s.coord.mu.Unlock()
		done := s.autoRemoveOne(ctx, it)
		s.coord.mu.Lock()
		delete(s.coord.checking, key)
		s.coord.mu.Unlock()
		if done {
			removed = append(removed, it.ID)
		}
	}
	return removed
}

// autoRemoveGone: a ledger worker a sweep marked as "Gone" whose host is not in
// this hub's fleet any more. Removing it only clears the record (confirmOne
// touches no worktree, branch or agent), so there is nothing a confirmation
// protects. Anything else, a coordinator included, is left to the user.
func autoRemoveGone(it *removal) bool {
	return it.EntryID != "" && !it.Coordinator && !it.Mark.Discard && strings.HasPrefix(it.Mark.Reason, "Sweep: ")
}

// autoRemoveGoneOne clears the record of one such worker, audited and logged.
func (s *Server) autoRemoveGoneOne(ctx context.Context, it *removal) bool {
	if !s.markCurrent(it.Mark) {
		return false
	}
	it.recordPark = ""
	o := s.confirmOne(ctx, it, false, "")
	label := orDefault(it.Name, it.EntryID)
	if !o.OK {
		log.Printf("auto remove (gone) of %s: %s: %s", it.ID, o.Code, o.Message)
		return false
	}
	const why = "its host is no longer in the fleet: only the record was cleared"
	s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.auto_remove", Host: it.Host, OK: true,
		Detail: fmt.Sprintf("%s (%s, host %s): %s", label, it.EntryID, it.Host, why)})
	s.queueLine("autoremove", it.EntryID, "autorm/"+it.EntryID, "cleared the record of "+safeText(label)+" automatically: its host is no longer in the fleet.")
	return true
}

// autoRemoveOne reviews one marked worker and removes it when it is clean.
func (s *Server) autoRemoveOne(ctx context.Context, it *removal) bool {
	ctx, cancel := context.WithTimeout(ctx, removeTimeout+90*time.Second)
	defer cancel()
	it.opts.Origin = true
	s.review(ctx, it)
	f := s.autoFactsOf(ctx, it)
	if ok, _ := autoRemoveVerdict(it, f); !ok {
		if s.ttlReaskDue(it, f) {
			if err := s.reask(it.Mark, true, ""); err != nil {
				log.Printf("auto remove: re-ask %s: %v", it.ID, err)
			}
		}
		return false
	}
	// The mark may have been withdrawn or replaced while the checks ran.
	if !s.markCurrent(it.Mark) {
		return false
	}
	how := inBaseHow(it.Checks)
	it.recordPark = park.WhyAutoRemoved
	if workGone(it) {
		// A park record needs a worktree to resume in; there is none.
		it.recordPark = ""
	}
	o := s.confirmOne(ctx, it, false, "")
	if !o.OK {
		log.Printf("auto remove of %s: %s: %s", it.ID, o.Code, o.Message)
		s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.auto_remove", Host: it.Host, OK: false,
			Detail: fmt.Sprintf("%s (%s): refused: %s %s", it.Name, it.EntryID, o.Code, o.Message)})
		return false
	}
	rec := ledger.AutoRemoval{At: time.Now().UTC(), Reason: autoRemoveReason, Landed: how, Followups: followupsOf(it)}
	if o.Result != nil {
		rec.WorktreeRemoved, rec.Kept, rec.ParkID = o.Result.WorktreeRemoved, o.Result.Kept, o.Result.ParkID
	}
	if workGone(it) {
		rec.Kept = "" // there was no worktree left to keep
	}
	if _, _, err := s.ledger.Update(it.EntryID, func(e *ledger.Entry) { e.AutoRemoved = &rec }); err != nil {
		log.Printf("ledger: %v", err)
	}
	label := orDefault(it.Name, it.EntryID)
	detail := fmt.Sprintf("%s (%s, branch %s): %s; %s", label, it.EntryID, it.Branch, autoRemoveReason, how)
	if !rec.WorktreeRemoved && rec.Kept != "" {
		detail += "; the worktree was kept: " + rec.Kept
	}
	detail += followupsText(rec.Followups)
	s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.auto_remove", Host: it.Host, OK: true, Detail: detail})
	line := fmt.Sprintf("removed %s automatically: %s", safeText(label), autoRemoveReason)
	if !rec.WorktreeRemoved && rec.Kept != "" {
		line = fmt.Sprintf("closed %s automatically (%s) but kept its worktree: %s", safeText(label), autoRemoveReason, safeText(rec.Kept))
	}
	s.queueLine("autoremove", it.EntryID, "autorm/"+it.EntryID, line+"."+followupsNote(rec.Followups))
	return true
}

// ttlReaskDue: a SAFE answer that went old is asked again (it stays until the
// new one replaces it) only when the age of the answer is all that stands in
// the way: a fresh answer cannot prove work the host's checks do not (a gone
// worktree whose commits are not in origin, a host that cannot be reached),
// so asking would only repeat forever. It counts against reaskMax.
func (s *Server) ttlReaskDue(it *removal, f autoFacts) bool {
	a := it.Mark.Ask
	if a == nil || a.State != ledger.AskSafe || a.ReaskedAt != nil || f.AnswerTTL <= 0 || f.Now.Sub(a.At) < f.AnswerTTL {
		return false
	}
	if a.Reasks >= s.reaskMax {
		return false
	}
	fresh := f
	fresh.AnswerTTL = 0
	ok, _ := autoRemoveVerdict(it, fresh)
	return ok
}

// followupsOf lists what a removed session's answer left for the user: its
// follow-up items, and the to-dos that stay in the To do list. Neither keeps
// the session (recheck), so the removal records them.
func followupsOf(it *removal) []string {
	a := it.Mark.Ask
	if a == nil {
		return nil
	}
	var out []string
	for _, item := range a.Items {
		switch item.Type {
		case ledger.ItemFollowup:
			out = append(out, "follow-up: "+clip(strings.TrimSpace(item.Text), 300))
		case ledger.ItemTodo:
			if item.ID != "" {
				out = append(out, "to-do "+item.ID+" (in your To do list): "+clip(strings.TrimSpace(item.Text), 200))
			}
		}
	}
	return out
}

// followupsText is the follow-ups for an audit entry or a log line ("" for none).
func followupsText(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return " Left for you: " + strings.Join(f, "; ")
}

// followupsNote is the follow-ups for a coordinator's line: only how many, since
// their words are the session's (the audit entry and the record carry them).
func followupsNote(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return fmt.Sprintf(" %d %s it left for you %s kept on the removal record.", len(f), plural(len(f), "follow-up", "follow-ups"), plural(len(f), "is", "are"))
}

// autoRemovedView is one recent auto-removal as the review lists it.
type autoRemovedView struct {
	EntryID         string     `json:"entry_id"`
	Host            string     `json:"host"`
	Name            string     `json:"name,omitempty"`
	Repo            string     `json:"repo,omitempty"`
	Branch          string     `json:"branch,omitempty"`
	At              time.Time  `json:"at"`
	Reason          string     `json:"reason"`
	Landed          string     `json:"landed,omitempty"`
	WorktreeRemoved bool       `json:"worktree_removed"`
	Kept            string     `json:"kept,omitempty"`
	Followups       []string   `json:"followups,omitempty"`
	UndoneAt        *time.Time `json:"undone_at,omitempty"`
	UndoNote        string     `json:"undo_note,omitempty"`
	// CanUndo: the branch can be set up again (a repository and a branch
	// are recorded, the host is known, and it was not undone already).
	CanUndo bool `json:"can_undo"`
	// InRecent: the session is in Recently closed and can be resumed from there.
	InRecent bool `json:"in_recent,omitempty"`
}

// recentAutoRemovals lists the auto-removals of the last AutoRemoveWindow,
// newest first.
func (s *Server) recentAutoRemovals(now time.Time) []autoRemovedView {
	out := []autoRemovedView{}
	if s.ledger == nil {
		return out
	}
	for _, e := range s.ledger.Entries() {
		a := e.AutoRemoved
		if a == nil || e.Status != ledger.Removed || now.Sub(a.At) > AutoRemoveWindow {
			continue
		}
		out = append(out, autoRemovedView{
			EntryID: e.ID, Host: e.Host, Name: e.AgentName, Repo: e.RepoName, Branch: e.Branch, At: a.At,
			Reason: a.Reason, Landed: a.Landed, WorktreeRemoved: a.WorktreeRemoved, Kept: a.Kept, Followups: a.Followups,
			UndoneAt: a.UndoneAt, UndoNote: a.UndoNote, InRecent: a.ParkID != "",
			CanUndo: a.UndoneAt == nil && e.Repo != "" && e.Branch != "" && s.knownHost(e.Host),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// handleUndoAutoRemove starts a worker on the branch of an auto-removed one:
// POST {entry_id}. The removal kept the branch, so the worktree is created
// again (or reused when it was kept) and a fresh agent starts in it; the old
// conversation is not restored.
func (s *Server) handleUndoAutoRemove(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b struct {
		EntryID string `json:"entry_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil || b.EntryID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	e, ok := s.ledger.Get(b.EntryID)
	switch {
	case !ok || e.AutoRemoved == nil:
		relErr(w, http.StatusNotFound, "not_found", "that worker was not removed automatically")
		return
	case e.AutoRemoved.UndoneAt != nil:
		relErr(w, http.StatusConflict, "undone", "that removal was already undone")
		return
	case time.Since(e.AutoRemoved.At) > AutoRemoveWindow:
		relErr(w, http.StatusConflict, "too_old", "an automatic removal can be undone for a day")
		return
	case e.Repo == "" || e.Branch == "" || !s.knownHost(e.Host):
		relErr(w, http.StatusConflict, "not_feasible", "undo is not possible: the repository, the branch or the host is not known any more")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), removeTimeout)
	defer cancel()
	var note, worktree string
	var agentOut any
	if id := e.AutoRemoved.ParkID; id != "" {
		// The record the removal left: resume the session itself (the host
		// recreates the worktree from its branch, or from where the work landed).
		var res *resumeResult
		var lerr *launchError
		if e.Host == s.fleet.LocalName() {
			res, lerr = s.resumeAgent(ctx, resumeBody{ID: id})
		} else {
			res = &resumeResult{}
			if lerr = s.callHost(ctx, e.Host, "resume", nil, resumeBody{ID: id}, res); lerr != nil {
				res = nil
			}
		}
		if lerr == nil {
			note = "the session was resumed"
			if res.Recreated != "" {
				note += ": " + res.Recreated
			}
			worktree, agentOut = res.Record.Worktree, res.Agent
		} else if lerr.code != "session_gone" && lerr.code != "no_such_record" && lerr.code != "not_parked" {
			relErr(w, lerr.status, "not_feasible", "undo is not possible: "+lerr.message)
			return
		}
	}
	if note == "" {
		// No record, or its session is gone: start a fresh worker on the branch.
		body := workerBody{Repo: e.Repo, Branch: e.Branch, Base: e.Base, Kind: e.Kind, Name: e.AgentName, Model: e.Model, Thinking: e.Thinking}
		res, lerr := s.workerOn(ctx, e.Host, body)
		if lerr != nil && lerr.code == "agent_name_taken" {
			body.Name = ""
			res, lerr = s.workerOn(ctx, e.Host, body)
		}
		if lerr != nil {
			relErr(w, lerr.status, "not_feasible", "undo is not possible: "+lerr.message)
			return
		}
		note = "a new worker started on branch " + res.Branch + " in " + res.Worktree + "; the old conversation is not restored"
		worktree, agentOut = res.Worktree, res.Agent
	}
	now := time.Now().UTC()
	if _, _, err := s.ledger.Update(e.ID, func(x *ledger.Entry) {
		if x.AutoRemoved != nil {
			x.AutoRemoved.UndoneAt, x.AutoRemoved.UndoNote = &now, note
		}
	}); err != nil {
		log.Printf("ledger: %v", err)
	}
	s.audit(r, ledger.AuditEvent{Action: "worker.auto_remove_undo", Host: e.Host, OK: true, Detail: e.AgentName + " (" + e.ID + "): " + note})
	writeJSON(w, map[string]any{"ok": true, "note": note, "agent": agentOut, "worktree": worktree})
}
