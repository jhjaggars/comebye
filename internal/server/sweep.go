package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// The sweep: find the ledger workers that look finished, check each on
// its own host, mark the ones with nothing to lose as ready to remove
// (the normal mark, so the session is asked and the user confirms in the
// removals review) and list the rest with why. It never removes anything.

// Sweep classes.
const (
	SweepReady     = "ready"      // marked (or would be, in a dry run)
	SweepNeedsLook = "needs_look" // not marked: a person or the coordinator decides
	// SweepDelivered: a research / plan / review / spike worker that has
	// reported; its branch has local work nobody meant to merge. Never
	// marked unless asked (mark_delivered): the user sees it first.
	SweepDelivered = "delivered"
	// SweepGone: nothing is left to lose or to check (an unknown host, a
	// vanished repository). Marked by a plain sweep; removing it clears
	// the record.
	SweepGone = "gone"
	SweepSkip = "skip" // not a candidate (already marked, a coordinator, a primary checkout…)
)

// defaultSweepIdle is how long a running worker's idle agent must have
// been idle to count as finished when it never reported.
const defaultSweepIdle = 30 * time.Minute

// sweepBody is the JSON body of a sweep.
type sweepBody struct {
	// DryRun lists without marking.
	DryRun bool `json:"dry_run"`
	// IdleAfter ("45m"): a worker still recorded as running whose agent
	// has been idle this long also counts. Default 30m; "0" turns it off.
	IdleAfter string `json:"idle_after"`
	// Notify tells a coordinator the outcome (never on a dry run).
	Notify bool `json:"notify"`
	// MarkDelivered also marks the "delivered" group for discarding (the
	// user still confirms each in the removals review). Off by default: it
	// holds unpushed commits, so it is the user's call.
	MarkDelivered bool `json:"mark_delivered"`
}

// sweepItem is one worker's outcome.
type sweepItem struct {
	ID      string `json:"id"` // the host-qualified agent id
	EntryID string `json:"entry_id,omitempty"`
	Host    string `json:"host"`
	Name    string `json:"name,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Class   string `json:"class"`
	// Reason is the mark's reason for ready items, else why it needs a
	// look or was skipped.
	Reason string `json:"reason"`
	Marked bool   `json:"marked,omitempty"`
	// AdHoc: the agent has no live handoff (started by hand, or after its
	// handoffs failed).
	AdHoc bool `json:"ad_hoc,omitempty"`
}

// sweepResult is the sweep's answer.
type sweepResult struct {
	DryRun    bool        `json:"dry_run"`
	Ready     []sweepItem `json:"ready"`
	NeedsLook []sweepItem `json:"needs_look"`
	// Delivered: work delivered in the report; the branch has local
	// commits. Gone: nothing left to lose or check.
	Delivered []sweepItem `json:"delivered"`
	Gone      []sweepItem `json:"gone"`
	Skipped   []sweepItem `json:"skipped"`
	// Notified is who got the summary, when asked for and delivered.
	Notified string `json:"notified,omitempty"`
	// NotifyError says why a requested notice was not delivered.
	NotifyError string `json:"notify_error,omitempty"`
}

// sweepCandidate is a worker the sweep looks at.
type sweepCandidate struct {
	e     ledger.Entry
	agent *herdr.AgentInfo // nil: gone
	adHoc bool             // no live handoff: the entry is made from the agent
}

// disposableBranchPrefixes are branch prefixes that, absent a declared
// purpose, suggest a branch not meant to merge. A hint only: it puts a
// worker in the "delivered" group for the user to see, never marks it.
var disposableBranchPrefixes = []string{"research/", "plan/", "review/", "spike/", "investigate/"}

// sweepDisposable says whether the worker's branch was never meant to
// merge: declared on the handoff, else guessed from the branch name.
func sweepDisposable(e ledger.Entry) bool {
	if ledger.DisposablePurpose(e.Purpose) {
		return true
	}
	if e.Purpose != "" {
		return false
	}
	for _, p := range disposableBranchPrefixes {
		if strings.HasPrefix(e.Branch, p) {
			return true
		}
	}
	return false
}

// dirtyFiles names the uncommitted files (porcelain status lines) for a
// reason: at most three.
func dirtyFiles(c *worktreeChecks) string {
	n := c.DirtyFiles
	var names []string
	if c.Detail != nil {
		for _, l := range c.Detail.Status {
			if len(l) > 3 {
				names = append(names, strings.TrimSpace(l[3:]))
			}
		}
	}
	text := fmt.Sprintf("%d %s", n, plural(n, "file", "files"))
	if len(names) == 0 {
		return text
	}
	shown := names[:min(3, len(names))]
	text += ": " + strings.Join(shown, ", ")
	if n > len(shown) {
		text += fmt.Sprintf(" and %d more", n-len(shown))
	}
	return text
}

// sweepClassify decides one worker from its live state and checks. It is
// pure: the single place the rules live.
func sweepClassify(e ledger.Entry, live *herdr.AgentInfo, c *worktreeChecks, checkErr string) (class, reason string) {
	if live != nil && (live.Status == herdr.StatusWorking || live.Status == herdr.StatusBlocked) {
		return SweepNeedsLook, "the agent is " + string(live.Status)
	}
	if checkErr != "" {
		return SweepNeedsLook, "checks failed: " + checkErr
	}
	if c == nil {
		return SweepNeedsLook, "no worktree to check"
	}
	switch {
	case !c.Exists:
		warn, msg := c.goneVerdict()
		if warn && !c.Gone && c.RepoMissing != "" {
			// The worktree and the repository it came from are both gone:
			// nothing is left to lose or to check.
			return SweepGone, msg
		}
		if warn {
			return SweepNeedsLook, msg
		}
		return SweepReady, "Sweep: " + sweepState(e) + "; " + msg
	case c.Primary:
		return SweepSkip, "it works in the repository's primary checkout"
	case c.InProgress != "":
		return SweepNeedsLook, "a " + c.InProgress + " is in progress"
	case c.Error != "":
		return SweepNeedsLook, "checks failed: " + c.Error
	case c.Unmerged < 0 && !c.Dirty:
		return SweepNeedsLook, "could not tell whether its branch is merged"
	case sweepDelivered(e, c):
		return SweepDelivered, deliveredReason(c)
	case c.Dirty:
		return SweepNeedsLook, "uncommitted changes (" + dirtyFiles(c) + ")"
	case c.Unmerged < 0:
		return SweepNeedsLook, "could not tell whether its branch is merged"
	case c.Unmerged > 0 && c.Equivalent != "":
		// A squash merge: the base has the branch's content.
		return SweepReady, sweepReason(e, c)
	case c.Unmerged > 0:
		unpushed := -1
		if c.Detail != nil {
			unpushed = c.Detail.Unpushed
		}
		switch {
		case unpushed > 0:
			return SweepNeedsLook, fmt.Sprintf("%d commits not in %s, %d of them on no remote", c.Unmerged, c.BaseUsed, unpushed)
		case unpushed == 0:
			return SweepNeedsLook, fmt.Sprintf("pushed but not merged: %d commits not in %s (an open PR?)", c.Unmerged, c.BaseUsed)
		}
		return SweepNeedsLook, fmt.Sprintf("%d commits not in %s", c.Unmerged, c.BaseUsed)
	}
	if c.Detail != nil && c.Detail.Unpushed > 0 {
		return SweepNeedsLook, fmt.Sprintf("%d commits on no remote", c.Detail.Unpushed)
	}
	return SweepReady, sweepReason(e, c)
}

// sweepDelivered: a research / plan / review / spike worker that
// reported and whose branch holds work the base lacks. Its output is the
// report; the branch is the user's to discard after seeing it.
func sweepDelivered(e ledger.Entry, c *worktreeChecks) bool {
	if !sweepDisposable(e) || strings.TrimSpace(e.FinalReport) == "" || c.Unmerged < 0 {
		return false
	}
	unpushed := 0
	if c.Detail != nil && c.Detail.Unpushed > 0 {
		unpushed = c.Detail.Unpushed
	}
	return c.Dirty || unpushed > 0 || (c.Unmerged > 0 && c.Equivalent == "")
}

func deliveredReason(c *worktreeChecks) string {
	base := orDefault(c.BaseUsed, "its base")
	var r string
	switch {
	case c.Detail != nil && c.Detail.Unpushed > 0:
		r = fmt.Sprintf("work delivered in the report; branch has %d local %s (on no remote)", c.Detail.Unpushed, plural(c.Detail.Unpushed, "commit", "commits"))
	case c.Unmerged > 0:
		r = fmt.Sprintf("work delivered in the report; branch is pushed, %d %s not in %s", c.Unmerged, plural(c.Unmerged, "commit", "commits"), base)
	default:
		r = "work delivered in the report"
	}
	if c.Dirty {
		r += "; uncommitted: " + dirtyFiles(c)
	}
	return r
}

func sweepState(e ledger.Entry) string {
	switch e.Status {
	case ledger.Finished:
		return "finished"
	case ledger.Landed:
		return "landed"
	}
	return "idle"
}

// sweepReason is the mark's reason for a ready worker, from its checks.
func sweepReason(e ledger.Entry, c *worktreeChecks) string {
	base := c.BaseUsed
	if base == "" {
		base = "its base"
	}
	if c.Unmerged > 0 && c.Equivalent != "" {
		return fmt.Sprintf("Sweep: %s; worktree clean; %s has %d commits with no equal in %s, but %s", sweepState(e), c.Branch, c.Unmerged, base, c.Equivalent)
	}
	r := fmt.Sprintf("Sweep: %s; worktree clean; every commit of %s is in %s", sweepState(e), c.Branch, base)
	if len(c.Landed) > 0 {
		r += " (" + landedText(base, c.Landed) + ")"
	}
	return r + "; nothing unpushed"
}

// sweepCandidates picks the workers to look at and the ones to skip with
// a reason. self is the caller's own agent id (never swept).
func (s *Server) sweepCandidates(agents []herdr.AgentInfo, idleAfter time.Duration, self string, now time.Time) (cands []sweepCandidate, skipped []sweepItem) {
	live := map[string]*herdr.AgentInfo{}
	for i := range agents {
		live[agents[i].ID] = &agents[i]
	}
	for _, e := range s.ledger.Entries() {
		item := sweepItem{ID: e.AgentID, EntryID: e.ID, Host: e.Host, Name: e.AgentName, Repo: e.RepoName, Branch: e.Branch, Class: SweepSkip}
		a := live[e.AgentID]
		name := e.AgentName
		if a != nil {
			name = a.Name
		}
		switch e.Status {
		case ledger.ReadyToRemove:
			item.Reason = "already marked ready to remove"
			skipped = append(skipped, item)
			continue
		case ledger.Finished, ledger.Landed:
		case ledger.Running:
			// Still recorded as running: only an agent that has sat idle
			// for a while counts.
			if !idleLongEnough(a, idleAfter, now) {
				continue
			}
		default:
			continue // waiting, removed, failed
		}
		switch {
		case e.AgentID == "":
			item.Reason = "no agent"
		case s.isCoordinator(e.AgentID, name):
			item.Reason = "a coordinator is never swept"
		case self != "" && e.AgentID == self:
			item.Reason = "the caller itself"
		case e.Worktree == "":
			item.Reason = "no worktree of its own"
		case !s.knownHost(e.Host):
			item.Class, item.Reason = SweepGone, "host "+e.Host+" is not in this hub's fleet any more: nothing can be checked there; removing clears the record"
		default:
			cands = append(cands, sweepCandidate{e: e, agent: a})
			continue
		}
		skipped = append(skipped, item)
	}
	// Agents with no live handoff (started by hand, or after their
	// handoffs failed) are not in the ledger as workers: an idle one in a
	// worktree of its own is a candidate too.
	marked := map[string]bool{}
	for _, m := range s.ledger.Marks() {
		marked[m.AgentID] = true
	}
	for i := range agents {
		a := &agents[i]
		if _, ok := s.ledger.ByAgent(a.ID); ok || !idleLongEnough(a, idleAfter, now) {
			continue
		}
		if a.Worktree == nil || !a.Worktree.IsLinked || a.Worktree.CheckoutPath == "" {
			continue // not in a worktree of its own: nothing to remove
		}
		host := a.Host
		if host == "" {
			host = s.fleet.LocalName()
		}
		e := ledger.Entry{Host: host, Repo: "", RepoName: a.Worktree.RepoName, Branch: a.Worktree.Branch, AgentID: a.ID, PaneID: a.PaneID,
			AgentName: a.Name, WorkspaceID: a.WorkspaceID, Worktree: a.Worktree.CheckoutPath, Status: ledger.Running}
		item := sweepItem{ID: a.ID, Host: host, Name: a.Name, Repo: e.RepoName, Branch: e.Branch, Class: SweepSkip, AdHoc: true}
		switch {
		case marked[a.ID]:
			item.Reason = "already marked ready to remove"
		case s.isCoordinator(a.ID, a.Name):
			item.Reason = "a coordinator is never swept"
		case self != "" && a.ID == self:
			item.Reason = "the caller itself"
		case !s.knownHost(host):
			item.Class, item.Reason = SweepGone, "host "+host+" is not in this hub's fleet any more"
		default:
			cands = append(cands, sweepCandidate{e: e, agent: a, adHoc: true})
			continue
		}
		skipped = append(skipped, item)
	}
	return cands, skipped
}

// idleLongEnough: the agent is idle (or done) and has been for idleAfter.
func idleLongEnough(a *herdr.AgentInfo, idleAfter time.Duration, now time.Time) bool {
	if idleAfter <= 0 || a == nil || (a.Status != herdr.StatusIdle && a.Status != herdr.StatusDone) {
		return false
	}
	since := a.StatusSince
	if since == nil {
		since = a.LastActive
	}
	return since != nil && now.Sub(*since) >= idleAfter
}

// sweep runs a sweep. mark records one ready worker's mark (nil: dry run).
func (s *Server) sweep(ctx context.Context, b sweepBody, self, by string) sweepResult {
	idle := defaultSweepIdle
	if v := strings.TrimSpace(b.IdleAfter); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			idle = d
		}
	}
	res := sweepResult{DryRun: b.DryRun, Ready: []sweepItem{}, NeedsLook: []sweepItem{}, Delivered: []sweepItem{}, Gone: []sweepItem{}, Skipped: []sweepItem{}}
	cands, skipped := s.sweepCandidates(s.fleet.Agents(), idle, self, time.Now())
	res.Skipped = append(res.Skipped, skipped...)
	for i := len(res.Skipped) - 1; i >= 0; i-- {
		switch res.Skipped[i].Class {
		case SweepNeedsLook:
			res.NeedsLook = append(res.NeedsLook, res.Skipped[i])
		case SweepGone:
			res.Gone = append(res.Gone, res.Skipped[i])
		default:
			continue
		}
		res.Skipped = append(res.Skipped[:i], res.Skipped[i+1:]...)
	}
	if !b.DryRun {
		for i := range res.Gone {
			g := &res.Gone[i]
			if g.ID == "" {
				continue
			}
			m := ledger.Mark{AgentID: g.ID, By: by, Reason: clip("Sweep: "+g.Reason, 500)}
			if _, _, err := s.applyMark(m, nil, true); err == nil {
				g.Marked = true
			}
		}
	}
	items := make([]sweepItem, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e := c.e
			// The host's checks fetch origin first, so a branch merged
			// upstream since the last fetch is seen as merged.
			chk, lerr := s.checksOn(ctx, e.Host, e.Worktree, e.Branch, e.Base, checkOpts{Detail: true}.forEntry(e))
			msg := ""
			if lerr != nil {
				msg = lerr.message
			}
			class, reason := sweepClassify(e, c.agent, chk, msg)
			items[i] = sweepItem{ID: e.AgentID, EntryID: e.ID, Host: e.Host, Name: e.AgentName, Repo: e.RepoName, Branch: e.Branch, Class: class, Reason: reason, AdHoc: c.adHoc}
		}()
	}
	wg.Wait()
	for i, it := range items {
		// Ready always marks; delivered and gone only when asked.
		wanted := it.Class == SweepReady || (it.Class == SweepDelivered && b.MarkDelivered) || it.Class == SweepGone
		if wanted && !b.DryRun {
			why := it.Reason
			if it.AdHoc && it.Class == SweepReady {
				why = strings.Replace(why, "Sweep: ", "Sweep: (no handoff) ", 1)
				it.Reason = why
			}
			if it.Class != SweepReady {
				why = "Sweep: " + why
			}
			m := ledger.Mark{AgentID: it.ID, By: by, Reason: clip(why, 500), Discard: it.Class == SweepDelivered}
			if _, _, err := s.applyMark(m, cands[i].agent, true); err == nil {
				it.Marked = true
			} else {
				it.Class, it.Reason = SweepNeedsLook, "could not mark it: "+err.Error()
			}
		}
		switch it.Class {
		case SweepReady:
			res.Ready = append(res.Ready, it)
		case SweepDelivered:
			res.Delivered = append(res.Delivered, it)
		case SweepGone:
			res.Gone = append(res.Gone, it)
		case SweepNeedsLook:
			res.NeedsLook = append(res.NeedsLook, it)
		default:
			res.Skipped = append(res.Skipped, it)
		}
	}
	for _, l := range []*[]sweepItem{&res.Ready, &res.NeedsLook, &res.Delivered, &res.Gone, &res.Skipped} {
		sort.SliceStable(*l, func(i, j int) bool { return (*l)[i].ID < (*l)[j].ID })
	}
	if b.Notify && !b.DryRun && (len(res.Ready)+len(res.NeedsLook)+len(res.Delivered)+len(res.Gone) > 0) {
		res.Notified, res.NotifyError = s.notifySweep(ctx, res)
	}
	return res
}

// sweepSummary is the text a coordinator gets.
func sweepSummary(res sweepResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "A sweep for finished agents marked %d ready to remove (the user confirms them in the app's removals review).", len(res.Ready))
	for _, it := range res.Ready {
		fmt.Fprintf(&sb, "\n- ready: %s %s: %s", it.ID, it.Branch, it.Reason)
	}
	if len(res.NeedsLook) > 0 {
		sb.WriteString("\nThese need a look (not marked); mark them with `comebye mark` if you judge them done:")
		for _, it := range res.NeedsLook {
			fmt.Fprintf(&sb, "\n- %s %s: %s", it.ID, it.Branch, it.Reason)
		}
	}
	for _, g := range []struct {
		title string
		items []sweepItem
	}{
		{"Work delivered in the report; the branch has local commits (the user can discard these after reading the report; `comebye sweep --mark-delivered` marks them)", res.Delivered},
		{"Gone: nothing left to lose or check (marked; removing clears the record)", res.Gone},
	} {
		if len(g.items) > 0 {
			sb.WriteString("\n" + g.title + ":")
			for _, it := range g.items {
				fmt.Fprintf(&sb, "\n- %s %s: %s", it.ID, it.Branch, it.Reason)
			}
		}
	}
	return sb.String()
}

// notifySweep tells an idle coordinator the outcome.
func (s *Server) notifySweep(ctx context.Context, res sweepResult) (to, errMsg string) {
	a, why := s.briefCoordinator()
	if a == nil {
		return "", "no idle coordinator to tell (" + why + ")"
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := s.promptHeld(ctx, *a, sweepSummary(res)); err != nil {
		return "", err.Error()
	}
	return a.ID, ""
}

// handleSweep: POST {dry_run?, idle_after?, notify?}. The user and agents
// may sweep, as both may mark; an agent is never swept by itself.
func (s *Server) handleSweep(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b sweepBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
	}
	self := ""
	if c := callerOf(r); c.agent() {
		self = c.agentRef()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	writeJSON(w, s.sweep(ctx, b, self, s.by(r, "")))
}
