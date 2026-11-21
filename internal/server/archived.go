package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/buildout"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
)

// Archived: finished work put away (issue #86, docs/lifecycle.md "Archived").
//
// The sweep loop archives a finished session that has made no progress for N
// days (7; 3 for research, plan, review, spike, poc, eval and test work), and
// a marked one whose "what is left" answer is still unsettled or that was
// marked as delivered. A marked one whose answer still says LEFT after the
// hub's last automatic re-ask, with only cleanup, other or followup items
// unsettled, is archived at once, and those items go onto its record. Archiving parks the pane (when the agent is live and
// idle), clears the regenerable build output in the worktree on its host (the
// bytes freed are recorded) and sets the ledger status. Nothing else is
// deleted: the worktree source, both branches, the transcript and the park
// record stay. The entry leaves the board's lanes and the removals review.
//
// Git still decides any removal: once a day the loop re-runs the proof on the
// archived entries whose host is up. Work that has landed since is unarchived
// and marked, and goes down the normal path (mark, proof, removal); a new
// commit on the branch brings the entry back to the board. While an archived
// worktree has commits on no remote, the hub holds its host's destroy timer.
//
// The user or a coordinator archives at once (POST /api/archived, `comebye
// archived add`) and brings one back (POST /api/archived/restore, `comebye
// archived restore`); an agent never archives itself.

const (
	defaultArchiveAfterDays = 7
	// archiveDeliveredDays is the clock for delivered kinds (research, plan,
	// review, spike, poc, eval, test): their output is the report.
	archiveDeliveredDays = 3
	maxArchiveAfterDays  = 90
	// archiveRecheck is how often an archived entry's proof is run again.
	archiveRecheck = 24 * time.Hour
	// archiveHoldHours is how long one destroy hold lasts: the loop renews it
	// at every sweep while it still holds.
	archiveHoldHours = 48
	doneArchived     = "archived" // a sweepDone kind: a session archived
	evArchive        = "archive"
)

// archiveAges are the archive clocks in force: plain work, and delivered kinds.
func (s *Server) archiveAges() (plain, delivered time.Duration) {
	if l := s.sweeps; l != nil && l.archiveAge > 0 {
		return l.archiveAge, l.archiveAge
	}
	days := defaultArchiveAfterDays
	if v := s.settings.get().SweepLoop; v != nil && v.ArchiveAfterDays != nil {
		days = *v.ArchiveAfterDays
	}
	plain = time.Duration(days) * 24 * time.Hour
	return plain, min(plain, archiveDeliveredDays*24*time.Hour)
}

// archiveLastActivity is when the entry last moved: the report, the latest
// progress note, the mark and its answer, a landing, a resume or a restore.
func archiveLastActivity(e ledger.Entry) time.Time {
	t := e.CreatedAt
	bump := func(x *time.Time) {
		if x != nil && x.After(t) {
			t = *x
		}
	}
	bump(e.FinishedAt)
	if e.Progress != nil {
		bump(&e.Progress.At)
	}
	if m := e.Mark; m != nil {
		bump(&m.At)
		if m.Ask != nil {
			bump(&m.Ask.At)
			bump(m.Ask.AskedAt)
		}
	}
	if e.Approval != nil {
		bump(&e.Approval.At)
	}
	if e.LandCheck != nil {
		bump(&e.LandCheck.At)
	}
	for i := range e.ParkHistory {
		if e.ParkHistory[i].Event == "resumed" {
			bump(&e.ParkHistory[i].At)
		}
	}
	for i := range e.ArchiveHistory {
		bump(&e.ArchiveHistory[i].At)
	}
	return t
}

// markUnsettled: a mark the removal path will not settle by itself: the work
// was marked as delivered (discard), or its answer says something is left (or
// gave none) and no re-ask or clean-up is under way.
func markUnsettled(m *ledger.Mark) bool {
	if m == nil {
		return false
	}
	if m.Discard {
		return true
	}
	a := m.Ask
	if a == nil || a.ReaskedAt != nil || a.CleaningAt != nil {
		return false
	}
	switch a.State {
	case ledger.AskUnsafe, ledger.AskUnclear, ledger.AskNoAnswer:
		return true
	}
	return false
}

// archiveFacts is what archiveBlock needs to know beyond the entry.
type archiveFacts struct {
	live        *herdr.AgentInfo // nil: the agent is gone
	coordinator bool
	knownHost   bool
	background  bool // a background task runs
	queued      bool // a prompt is queued for it
	openTodo    bool // an open to-do waits on it
	pinned      bool // its park record is pinned
	planWaiting bool // its plan still has steps waiting
	// reasksOut: the hub's automatic re-asks of its mark are used up, the last
	// answer still says LEFT, and every item left is one only the user or
	// nobody can settle (cleanup, other, followup): it is archived at once,
	// without waiting out the clock. left are those items.
	reasksOut bool
	left      []ledger.AskItem
}

// reasksSpent: the mark's answer is LEFT and came back after the last of the
// hub's max automatic re-asks (none in flight, no clean-up under way).
func reasksSpent(m *ledger.Mark, max int) bool {
	if m == nil || max <= 0 || m.Discard {
		return false
	}
	a := m.Ask
	return a != nil && a.State == ledger.AskUnsafe && a.Reasks >= max && a.ReaskedAt == nil && a.CleaningAt == nil
}

// leftOnlyOwed reads a re-checked LEFT answer (its items re-typed): it returns
// the items still unsettled when every one is cleanup, other or followup, and
// false when one is still moving or checkable (an open pr, a to-do, a
// decision, a branch, a path, ...), when all have settled (the proof removes
// it), or when what is left is a stale branch the sweep deletes.
func leftOnlyOwed(ar *askReview) ([]ledger.AskItem, bool) {
	if ar == nil || ar.State != ledger.AskUnsafe || ar.Resolved || ar.StaleOnly {
		return nil, false
	}
	var left []ledger.AskItem
	for _, v := range ar.Items {
		if v.State == itemResolved {
			continue
		}
		switch v.Type {
		case ledger.ItemCleanup, ledger.ItemOther, ledger.ItemFollowup:
			left = append(left, v.AskItem)
		default:
			return nil, false
		}
	}
	return left, len(left) > 0
}

// reasksOutWhy is the record's reason for an archive when the re-asks ran out.
func reasksOutWhy(n int, left []ledger.AskItem) string {
	return fmt.Sprintf("re-asked %d %s and its answer still lists %d %s left that the hub cannot settle", n, plural(n, "time", "times"), len(left), plural(len(left), "thing", "things"))
}

// archiveBlock says why the loop does not archive e now ("" when it does).
// It is pure: the rules live here.
func archiveBlock(e ledger.Entry, f archiveFacts, now time.Time, plain, delivered time.Duration) string {
	switch {
	case e.Status == ledger.Finished:
	case e.Status == ledger.ReadyToRemove && markUnsettled(e.Mark):
	default:
		return "not finished, or marked with nothing unsettled"
	}
	age := plain
	if ledger.DisposablePurpose(ledger.EffectivePurpose(e)) {
		age = delivered
	}
	switch {
	case e.Worktree == "":
		return "no worktree of its own"
	case e.Loop != "":
		return "a standing --loop worker"
	case f.coordinator:
		return "a coordinator"
	case !f.knownHost:
		return "its host is not in the fleet"
	case f.live != nil && (f.live.Status == herdr.StatusWorking || f.live.Status == herdr.StatusBlocked):
		return "the agent is " + string(f.live.Status)
	case f.background:
		return "a background task is running"
	case f.queued:
		return "a prompt is queued for it"
	case f.openTodo:
		return "an open to-do waits on it"
	case f.pinned:
		return "its park record is pinned"
	case f.planWaiting:
		return "its plan still has steps waiting"
	case !f.reasksOut && now.Sub(archiveLastActivity(e)) < age:
		return "it moved within " + archiveAgeText(age)
	}
	return ""
}

func archiveAgeText(d time.Duration) string {
	if d >= 24*time.Hour {
		n := int(d / (24 * time.Hour))
		return fmt.Sprintf("%d %s", n, plural(n, "day", "days"))
	}
	return d.String()
}

// archiveWhy is the record's reason for an archive by the loop.
func archiveWhy(e ledger.Entry, plain, delivered time.Duration) string {
	if e.Status == ledger.ReadyToRemove {
		if e.Mark != nil && e.Mark.Discard {
			return "marked as delivered, and no progress for " + archiveAgeText(delivered)
		}
		return "what is left is still unsettled, and no progress for " + archiveAgeText(plain)
	}
	if k := ledger.EffectivePurpose(e); ledger.DisposablePurpose(k) {
		return k + " work, reported; no progress for " + archiveAgeText(delivered)
	}
	return "finished, not merged; no progress for " + archiveAgeText(plain)
}

// archiveFactsOf gathers the facts for every entry from one fleet snapshot.
func (s *Server) archiveFactsOf(ctx context.Context, e ledger.Entry, live map[string]*herdr.AgentInfo, openTodos map[string]bool, pinned map[string]bool) archiveFacts {
	f := archiveFacts{live: live[e.AgentID], knownHost: s.knownHost(e.Host), openTodo: openTodos[e.AgentID]}
	if f.live != nil && e.AgentName != "" && f.live.Name != e.AgentName {
		f.live = nil // another agent in the reused pane
	}
	name := e.AgentName
	if f.live != nil {
		name = f.live.Name
	}
	f.coordinator = s.isCoordinator(e.AgentID, name)
	if f.live != nil {
		if s.pq != nil && len(s.pq.list(e.AgentID, false)) > 0 {
			f.queued = true
		}
		if f.live.Status == herdr.StatusIdle || f.live.Status == herdr.StatusDone {
			tasks, known := s.backgroundOf(ctx, e, false)
			f.background = known && len(tasks) > 0
		}
	}
	for _, h := range e.ParkHistory {
		if pinned[h.ID] {
			f.pinned = true
		}
	}
	if pinned[e.Host+"|"+filepath.Clean(e.Worktree)] {
		f.pinned = true
	}
	if e.PlanID != "" {
		for _, m := range s.ledger.Members(e.PlanID) {
			if m.Status == ledger.Waiting {
				f.planWaiting = true
			}
		}
	}
	return f
}

// archiveStep is the sweep loop's archive step: it archives what is due, runs
// the daily proof on the archived entries and holds the destroy of a host with
// an archived worktree holding commits on no remote.
func (s *Server) archiveStep(ctx context.Context, now time.Time) []sweepDone {
	if s.ledger == nil {
		return nil
	}
	plain, delivered := s.archiveAges()
	agents := s.fleet.Agents()
	live := map[string]*herdr.AgentInfo{}
	for i := range agents {
		live[agents[i].ID] = &agents[i]
	}
	openTodos := map[string]bool{}
	if s.todos != nil {
		for _, t := range s.todos.list("") {
			if t.State == TodoOpen && t.Waiting != nil {
				openTodos[t.Waiting.ID] = true
			}
		}
	}
	pinned := map[string]bool{}
	for _, pr := range s.parkSnapshot() {
		if pr.Rec.Pinned {
			pinned[pr.Rec.ID] = true
			if pr.Rec.Worktree != "" {
				pinned[pr.Host+"|"+filepath.Clean(pr.Rec.Worktree)] = true
			}
		}
	}
	var done []sweepDone
	var marks map[string]*removal // entry id -> its mark, read once when needed
	for _, e := range s.ledger.Entries() {
		if ctx.Err() != nil {
			break
		}
		f := s.archiveFactsOf(ctx, e, live, openTodos, pinned)
		if e.Status == ledger.ReadyToRemove && reasksSpent(e.Mark, s.reaskMax) {
			if marks == nil {
				marks = map[string]*removal{}
				for _, it := range s.marked() {
					if it.EntryID != "" {
						marks[it.EntryID] = it
					}
				}
			}
			if it := marks[e.ID]; it != nil {
				f.left, f.reasksOut = leftOnlyOwed(s.askReviewOf(ctx, it))
			}
		}
		if why := archiveBlock(e, f, now, plain, delivered); why != "" {
			continue
		}
		why := archiveWhy(e, plain, delivered)
		if f.reasksOut {
			why = reasksOutWhy(e.Mark.Ask.Reasks, f.left)
		}
		got, err := s.archiveOneLeft(ctx, e, f.live, AutoCreator, why, true, f.left)
		if err != nil {
			log.Printf("archive: %s (%s): %v", orDefault(e.AgentName, e.ID), e.ID, err)
			continue
		}
		done = append(done, archivedDone(got, now))
	}
	s.archiveRecheckAll(ctx, now)
	s.holdDestroys(ctx, now)
	return done
}

func archivedDone(e ledger.Entry, now time.Time) sweepDone {
	d := sweepDone{At: now.UTC(), Kind: doneArchived, Name: orDefault(e.AgentName, e.ID), Repo: e.RepoName, Host: e.Host, Branch: e.Branch}
	if a := e.Archive; a != nil {
		d.Note, d.Freed = a.Why, a.BuildFreed
	}
	return d
}

// errArchiveRefused is an archive the entry's state refuses (not a failure).
var errArchiveRefused = errors.New("refused")

// archiveOne archives e: park its pane when the agent is live, clear the build
// output on its host, set the ledger status. auto is the loop's own archive: a
// worktree that is gone is left to the sweep.
func (s *Server) archiveOne(ctx context.Context, e ledger.Entry, live *herdr.AgentInfo, by, why string, auto bool) (ledger.Entry, error) {
	return s.archiveOneLeft(ctx, e, live, by, why, auto, nil)
}

// archiveOneLeft is archiveOne with the unsettled items to carry onto the
// record (nil: those of the stored answer, as written).
func (s *Server) archiveOneLeft(ctx context.Context, e ledger.Entry, live *herdr.AgentInfo, by, why string, auto bool, left []ledger.AskItem) (ledger.Entry, error) {
	if !ledger.Archivable(e.Status) {
		return e, fmt.Errorf("%w: it is %s; only finished, landed or marked work is archived", errArchiveRefused, e.Status)
	}
	a := ledger.Archival{By: by, Why: why, Unmerged: -1, Unpushed: -1, Left: left}
	reachable := false
	if e.Worktree != "" {
		chk, lerr := s.checksOn(ctx, e.Host, e.Worktree, e.Branch, e.Base, checkOpts{Detail: true}.forEntry(e))
		switch {
		case lerr != nil:
			// The host is down: the entry is archived all the same and the
			// clearing waits for the host.
		case chk.Primary:
			return e, fmt.Errorf("%w: it works in the repository's primary checkout (a home session)", errArchiveRefused)
		case !chk.Exists && auto:
			return e, fmt.Errorf("%w: its worktree is gone (the sweep handles it)", errArchiveRefused)
		default:
			reachable = chk.Exists
			a.Tip, a.Unmerged = chk.Tip, chk.Unmerged
			if chk.Detail != nil {
				a.Unpushed = chk.Detail.Unpushed
			}
			t := time.Now().UTC()
			a.CheckedAt = &t
		}
	}
	if live != nil {
		res, lerr := s.parkOn(ctx, e.Host, parkBody{Agent: live.PaneID, Name: live.Name, Dirty: true, Why: "archive"})
		if lerr != nil {
			return e, fmt.Errorf("parking its pane: %s", lerr.message)
		}
		a.ParkID = res.Record.ID
		a.BuildFreed += res.BuildFreed
		a.BuildDirs = append(a.BuildDirs, res.BuildDirs...)
	}
	if reachable {
		if res, lerr := s.buildoutClearOn(ctx, e.Host, e.Worktree); lerr != nil {
			a.BuildPending = true
		} else {
			a.BuildFreed += res.Freed
			a.BuildDirs = append(a.BuildDirs, res.Dirs...)
			a.WorktreeBytes = res.WorktreeBytes
		}
	} else if e.Worktree != "" {
		a.BuildPending = true
	}
	got, err := s.ledger.Archive(e.ID, a)
	if err != nil {
		return got, err
	}
	what := "archived " + orDefault(e.AgentName, e.ID) + " (" + why + ")"
	if a.BuildFreed > 0 {
		what += "; freed " + buildout.Human(a.BuildFreed) + " of build output"
	}
	if a.BuildPending {
		what += "; its build output is cleared when " + e.Host + " is up"
	}
	s.logEvent(evArchive, e.ID, orDefault(e.AgentName, e.ID), "", what+". Restore it with `comebye archived restore "+orDefault(e.AgentName, e.ID)+"`.")
	return got, nil
}

// parkOn parks an agent on its own host.
func (s *Server) parkOn(ctx context.Context, host string, b parkBody) (*parkResult, *launchError) {
	if s.isLocalHost(host) {
		return s.parkAgent(ctx, b, AutoCreator)
	}
	var res parkResult
	if e := s.callHost(ctx, host, "park", nil, b, &res); e != nil {
		return nil, e
	}
	return &res, nil
}

// ---- the daily proof ----

// archiveRecheckAll re-runs the proof on the archived entries that are due and
// whose host answers: work that landed since goes down the removal path, a new
// commit brings the entry back, build output left pending is cleared.
func (s *Server) archiveRecheckAll(ctx context.Context, now time.Time) {
	every := archiveRecheck
	if s.sweeps != nil && s.sweeps.recheck > 0 {
		every = s.sweeps.recheck
	}
	for _, e := range s.ledger.ArchivedEntries() {
		a := e.Archive
		if ctx.Err() != nil || a == nil || e.Worktree == "" || !s.knownHost(e.Host) {
			continue
		}
		if !a.BuildPending && a.CheckedAt != nil && now.Sub(*a.CheckedAt) < every {
			continue
		}
		s.archiveRecheck(ctx, e, now)
	}
}

func (s *Server) archiveRecheck(ctx context.Context, e ledger.Entry, now time.Time) {
	a := e.Archive
	chk, lerr := s.checksOn(ctx, e.Host, e.Worktree, e.Branch, e.Base, checkOpts{Detail: true}.forEntry(e))
	if lerr != nil || chk == nil || !chk.Exists || chk.Error != "" {
		return // the host is down (or the worktree went): next time
	}
	name := orDefault(e.AgentName, e.ID)
	if a.Tip != "" && chk.Tip != "" && chk.Tip != a.Tip {
		if _, err := s.ledger.Unarchive(e.ID, AutoCreator, "a new commit on its branch"); err == nil {
			s.logEvent(evArchive, e.ID, name, "", name+" came back from the archive: a new commit on "+e.Branch+".")
		}
		return
	}
	if class, reason := sweepClassify(e, nil, chk, ""); class == SweepReady {
		// The work has landed since: the normal path, mark then proof then
		// removal. Nothing is deleted here.
		if _, err := s.ledger.Unarchive(e.ID, AutoCreator, "its work landed"); err != nil {
			return
		}
		m := ledger.Mark{AgentID: e.AgentID, By: AutoCreator, Reason: clip(strings.Replace(reason, "Sweep: ", "Sweep: archived, and since landed; ", 1), 500)}
		if _, _, err := s.applyMark(m, nil, true); err != nil {
			log.Printf("archive: marking %s: %v", name, err)
		}
		s.logEvent(evArchive, e.ID, name, "", name+" left the archive: its work landed, so it is marked and goes down the removal path.")
		return
	}
	var freed int64
	var dirs []string
	pending := a.BuildPending
	if pending {
		if res, lerr := s.buildoutClearOn(ctx, e.Host, e.Worktree); lerr == nil {
			freed, dirs, pending = res.Freed, res.Dirs, false
		}
	}
	t := now.UTC()
	s.ledger.UpdateArchive(e.ID, func(x *ledger.Archival) {
		x.CheckedAt, x.Unmerged = &t, chk.Unmerged
		if chk.Detail != nil {
			x.Unpushed = chk.Detail.Unpushed
		}
		if x.Tip == "" {
			x.Tip = chk.Tip
		}
		if x.BuildPending && !pending {
			x.BuildPending, x.BuildFreed = false, x.BuildFreed+freed
			x.BuildDirs = append(x.BuildDirs, dirs...)
		}
	})
	if freed > 0 {
		s.logEvent(evArchive, e.ID, name, "", fmt.Sprintf("freed %s of build output in archived %s now that %s is up.", buildout.Human(freed), name, e.Host))
	}
}

// ---- holding a host's destroy ----

// archiveHolding lists, per host, the archived sessions there with commits on
// no remote (or not known): a destroy of that host would lose them.
func (s *Server) archiveHolding() map[string][]string {
	out := map[string][]string{}
	for _, e := range s.ledger.ArchivedEntries() {
		if a := e.Archive; a != nil && e.Worktree != "" && a.Unpushed != 0 {
			out[e.Host] = append(out[e.Host], orDefault(e.AgentName, e.ID))
		}
	}
	return out
}

func holdWhy(names []string) string {
	sort.Strings(names)
	return fmt.Sprintf("archived %s on this host %s commits on no remote: %s", plural(len(names), "session", "sessions"), plural(len(names), "has", "have"), strings.Join(names, ", "))
}

// holdDestroys asks each provisioner to hold the destroy timer of a host with
// archived unpushed work, renewing the hold at every sweep, and lifts the holds
// it asked for before that no longer apply.
func (s *Server) holdDestroys(ctx context.Context, now time.Time) {
	if s.sweeps == nil || s.prov == nil {
		return
	}
	want := s.archiveHolding()
	owners := map[string]string{} // host -> provisioner node
	s.prov.mu.Lock()
	for node, c := range s.prov.hosts {
		if c == nil || c.View == nil {
			continue
		}
		for _, h := range c.View.Hosts {
			if h.State != "destroyed" && !h.Adopted {
				owners[h.Name] = node
			}
		}
	}
	s.prov.mu.Unlock()
	l := s.sweeps
	l.mu.Lock()
	prev := append([]string(nil), l.st.Holds...)
	l.mu.Unlock()
	var held []string
	send := func(host, why string, hours int) bool {
		if s.phosts != nil {
			if _, ok := s.phosts.Get(host); ok {
				return s.phosts.Hold(host, why, time.Duration(hours)*time.Hour)
			}
		}
		node, ok := owners[host]
		if !ok {
			return false
		}
		code, body, err := s.callBody(ctx, node, "provision-host-hold", map[string]any{"name": host, "why": why, "hours": hours})
		if err != nil || code != http.StatusOK {
			log.Printf("archive: holding the destroy of %s on %s: %v %d %s", host, node, err, code, oneLine(string(body), 200))
			return false
		}
		return true
	}
	hosts := make([]string, 0, len(want))
	for h := range want {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		why := holdWhy(want[h])
		if !send(h, why, archiveHoldHours) {
			continue
		}
		held = append(held, h)
		if !containsStr(prev, h) {
			s.logEvent(evArchive, h, "", "", "held the destroy timer of "+h+": "+why+". It waits while they stay unpushed.")
		}
	}
	for _, h := range prev {
		if !containsStr(held, h) && len(want[h]) == 0 {
			send(h, "", 0)
		}
	}
	l.mu.Lock()
	l.st.Holds = held
	l.saveLocked()
	l.mu.Unlock()
}

func containsStr(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// ---- the host's build-output call ----

type buildoutClearBody struct {
	Worktree string `json:"worktree"`
}

type buildoutClearResult struct {
	Worktree      string   `json:"worktree"`
	Freed         int64    `json:"freed"`
	Dirs          []string `json:"dirs"`
	WorktreeBytes int64    `json:"worktree_bytes"`
}

// handleBuildoutClear is POST /api/buildout/clear {worktree}: clear one linked
// worktree's regenerable build output (buildout's rule: a real directory git
// ignores and tracks nothing in, with a CACHEDIR.TAG or on the known list). A
// worktree with a live agent in it is refused.
func (s *Server) handleBuildoutClear(w http.ResponseWriter, r *http.Request) {
	var b buildoutClearBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil || b.Worktree == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	res, lerr := s.buildoutClearLocal(ctx, b.Worktree)
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

func (s *Server) buildoutClearLocal(ctx context.Context, worktree string) (*buildoutClearResult, *launchError) {
	root := s.worktreeRoot()
	if !filepath.IsAbs(root) || !filepath.IsAbs(worktree) {
		return nil, errLaunchDisabled
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errLaunchDisabled
	}
	wt, err := filepath.EvalSymlinks(filepath.Clean(worktree))
	if err != nil {
		return nil, &launchError{status: http.StatusNotFound, code: "no_worktree", message: "no such worktree"}
	}
	if !within(wt, rootReal) || wt == rootReal {
		return nil, &launchError{status: http.StatusForbidden, code: "outside_root", message: "the worktree is not under this host's worktree root"}
	}
	if fi, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || !fi.Mode().IsRegular() {
		return nil, &launchError{status: http.StatusConflict, code: "not_linked", message: "not a linked git worktree"}
	}
	live, err := s.client.ListAgents(ctx)
	if err != nil {
		return nil, herdrFailure("list agents", err) // never clear without knowing who is live
	}
	for _, a := range live {
		if cwd, err := filepath.EvalSymlinks(a.CWD); err == nil && within(cwd, wt) {
			return nil, &launchError{status: http.StatusConflict, code: "agent_live", message: "an agent is running in this worktree (" + a.Name + "): its build output is left alone"}
		}
	}
	freed, cleared, err := buildout.Clear(ctx, wt)
	if err != nil && freed == 0 {
		return nil, &launchError{status: http.StatusInternalServerError, code: "clear_failed", message: err.Error()}
	}
	res := &buildoutClearResult{Worktree: wt, Freed: freed, Dirs: []string{}}
	for _, d := range cleared {
		res.Dirs = append(res.Dirs, d.Rel)
	}
	res.WorktreeBytes = park.TreeSize(wt, 3*time.Second)
	if freed > 0 {
		log.Printf("archive: cleared %s of build output in %s (%s)", buildout.Human(freed), wt, strings.Join(res.Dirs, ", "))
	}
	return res, nil
}

// buildoutClearOn clears a worktree's build output on its host.
func (s *Server) buildoutClearOn(ctx context.Context, host, worktree string) (*buildoutClearResult, *launchError) {
	if s.isLocalHost(host) {
		return s.buildoutClearLocal(ctx, worktree)
	}
	var res buildoutClearResult
	if e := s.callHost(ctx, host, "buildout-clear", nil, buildoutClearBody{Worktree: worktree}, &res); e != nil {
		return nil, e
	}
	return &res, nil
}

// ---- the API ----

// archivedRow is one archived session, as `comebye archived` and the app list it.
type archivedRow struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	AgentID  string          `json:"agent_id,omitempty"`
	Host     string          `json:"host"`
	Project  string          `json:"project,omitempty"`
	Branch   string          `json:"branch,omitempty"`
	Purpose  string          `json:"purpose,omitempty"`
	Worktree string          `json:"worktree,omitempty"`
	Report   bool            `json:"report,omitempty"`
	Archive  ledger.Archival `json:"archive"`
}

type archivedView struct {
	Archived []archivedRow `json:"archived"`
	Count    int           `json:"count"`
	// BuildFreed is the build output freed across them, WorktreeBytes the
	// disk their worktrees still use (as measured at archive time).
	BuildFreed    int64 `json:"build_freed"`
	WorktreeBytes int64 `json:"worktree_bytes"`
}

func (s *Server) archivedRows() archivedView {
	v := archivedView{Archived: []archivedRow{}}
	for _, e := range s.ledger.ArchivedEntries() {
		if e.Archive == nil {
			continue
		}
		v.Archived = append(v.Archived, archivedRow{ID: e.ID, Name: orDefault(e.AgentName, e.ID), AgentID: e.AgentID, Host: e.Host, Project: e.RepoName,
			Branch: e.Branch, Purpose: ledger.EffectivePurpose(e), Worktree: e.Worktree, Report: e.FinalReport != "", Archive: *e.Archive})
		v.BuildFreed += e.Archive.BuildFreed
		v.WorktreeBytes += e.Archive.WorktreeBytes
	}
	sort.SliceStable(v.Archived, func(i, j int) bool { return v.Archived[i].Archive.At.After(v.Archived[j].Archive.At) })
	v.Count = len(v.Archived)
	return v
}

// handleArchived is GET /api/archived: the archived sessions, newest first.
func (s *Server) handleArchived(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	writeJSON(w, s.archivedRows())
}

type archiveBody struct {
	// Ref is the session: a ledger entry id, an agent id or a worker's name.
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
	// Resume (restore only) also resumes the parked pane.
	Resume bool `json:"resume"`
}

// archiveTarget finds the newest entry ref names (id, agent id or name) that
// passes ok.
func (s *Server) archiveTarget(ref string, ok func(ledger.Entry) bool) (ledger.Entry, bool) {
	ref = strings.TrimSpace(ref)
	return s.ledger.Find(func(e *ledger.Entry) bool {
		return (e.ID == ref || e.AgentID == ref || (e.AgentName != "" && e.AgentName == ref)) && ok(*e)
	})
}

// archiveAllowed: the user or a coordinator; never an agent on itself.
func (s *Server) archiveAllowed(w http.ResponseWriter, r *http.Request, e ledger.Entry) bool {
	c := callerOf(r)
	if !c.agent() {
		return true
	}
	if e.AgentID != "" && e.AgentID == c.agentRef() {
		writeJSONStatus(w, http.StatusConflict, apiErr("self", "an agent cannot archive itself"))
		return false
	}
	if !s.callerIsCoordinator(c) {
		writeJSONStatus(w, http.StatusForbidden, apiErr("coordinator_only", "only the user or a coordinator can archive or restore a session"))
		return false
	}
	return true
}

// handleArchiveAdd is POST /api/archived {ref, reason}: archive a session at once.
func (s *Server) handleArchiveAdd(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b archiveBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b); err != nil || strings.TrimSpace(b.Ref) == "" {
		writeJSONStatus(w, http.StatusBadRequest, apiErr("bad_request", "which session? (ref: its entry id, agent id or name)"))
		return
	}
	e, ok := s.archiveTarget(b.Ref, func(e ledger.Entry) bool { return e.Active() })
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, apiErr("not_found", "no handoff matches "+plainText(b.Ref, 60)))
		return
	}
	if !s.archiveAllowed(w, r, e) {
		return
	}
	if c := callerOf(r); c.agent() && strings.TrimSpace(b.Reason) == "" {
		writeJSONStatus(w, http.StatusBadRequest, apiErr("reason_required", "say why it can be put away (--reason)"))
		return
	}
	if e.Status == ledger.Archived {
		writeJSONStatus(w, http.StatusConflict, apiErr("already_archived", "it is archived already"))
		return
	}
	var live *herdr.AgentInfo
	for _, a := range s.fleet.Agents() {
		if a.ID == e.AgentID && (e.AgentName == "" || a.Name == e.AgentName) {
			a := a
			live = &a
		}
	}
	if live != nil && (live.Status == herdr.StatusWorking || live.Status == herdr.StatusBlocked) {
		writeJSONStatus(w, http.StatusConflict, apiErr("agent_working", "the agent is "+string(live.Status)+": archive it when it is idle"))
		return
	}
	why := clip(strings.TrimSpace(b.Reason), 300)
	if why == "" {
		why = "archived by hand"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	got, err := s.archiveOne(ctx, e, live, s.by(r, ""), why, false)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errArchiveRefused) {
			code = http.StatusConflict
		}
		writeJSONStatus(w, code, apiErr("not_archived", err.Error()))
		return
	}
	writeJSON(w, map[string]any{"entry": got})
}

// handleArchiveRestore is POST /api/archived/restore {ref, resume}: bring an
// archived session back to the state it was archived from (its mark is not
// restored); resume also resumes its parked pane.
func (s *Server) handleArchiveRestore(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b archiveBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b); err != nil || strings.TrimSpace(b.Ref) == "" {
		writeJSONStatus(w, http.StatusBadRequest, apiErr("bad_request", "which session? (ref: its entry id, agent id or name)"))
		return
	}
	e, ok := s.archiveTarget(b.Ref, func(e ledger.Entry) bool { return e.Status == ledger.Archived })
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, apiErr("not_found", "no archived session matches "+plainText(b.Ref, 60)))
		return
	}
	if !s.archiveAllowed(w, r, e) {
		return
	}
	parkID := ""
	if e.Archive != nil {
		parkID = e.Archive.ParkID
	}
	got, err := s.ledger.Unarchive(e.ID, s.by(r, ""), "restored")
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, apiErr("ledger_error", err.Error()))
		return
	}
	name := orDefault(e.AgentName, e.ID)
	s.logEvent(evArchive, e.ID, name, "", name+" was restored from the archive by "+orDefault(s.by(r, ""), "the user")+".")
	out := map[string]any{"entry": got}
	if b.Resume {
		if parkID == "" {
			out["resume_error"] = "it has no park record to resume (its agent was gone when it was archived): hand the branch off again"
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			defer cancel()
			res, lerr := s.resumeOn(ctx, e.Host, resumeBody{ID: parkID})
			if lerr != nil {
				out["resume_error"] = lerr.message
			} else {
				out["resume"] = res
			}
		}
	}
	writeJSON(w, out)
}

// resumeOn resumes a parked agent on its host.
func (s *Server) resumeOn(ctx context.Context, host string, b resumeBody) (*resumeResult, *launchError) {
	if s.isLocalHost(host) {
		return s.resumeAgent(ctx, b)
	}
	var res resumeResult
	if e := s.callHost(ctx, host, "resume", nil, b, &res); e != nil {
		return nil, e
	}
	return &res, nil
}
