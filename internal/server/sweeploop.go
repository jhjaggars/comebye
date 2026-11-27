package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/buildout"
)

// The sweep loop. The hub runs the sweep (sweep.go) on a schedule, so finished
// sessions are marked ready to remove without anyone typing `comebye sweep`,
// and the ones with nothing to lose are cleaned up without anyone tapping
// anything. It lives in the hub because the hub already owns the ledger, the
// fleet and the clock, every install gets it, and there is no unit file to keep
// in step with the binary.
//
// What it does, and what it never does:
//
//   - It marks, as the sweep always did (the session is asked what is left),
//     and it settles what it can prove for sessions whose agent is gone
//     (leftovers.go): a stale remote branch every commit of which is in the
//     default branch, with no open PR, is deleted, and its name and tip SHA
//     are logged so it can be pushed back.
//   - With "Clean up automatically" on (the default) it then removes a marked
//     session when the hub itself proves, on the session's host, right before
//     removing, that there is nothing to lose (sweepclean.go): clean tree, every
//     commit in the default branch, nothing unpushed. The removal is the
//     ordinary one: the session, its worktree and its local branch go, and the
//     worktree is kept whenever anything is not provably in the base. Off, the
//     loop only marks.
//   - It never files a To do item. A session that needs a look stays in the
//     removals review, unremoved. What the loop did is on the board's "Hub did"
//     line and in the review.
//   - It is idempotent: a sweep skips what is already marked, a second run
//     removes and deletes nothing more, and a run that does nothing adds
//     nothing to the log.
//   - It is cheap: one sweep an hour (setting), the same checks `comebye
//     sweep` makes, four hosts' worth at a time.
//
// Its state (the last run, what it marked, removed and deleted) is
// sweeploop.json in the state directory, so a restart neither repeats a sweep
// nor forgets what it did. The setting (on or off, how often) is in
// settings.json, edited in the app's Peers screen.

const (
	// SourceSweep marked the To do item the sweep loop used to file; the loop
	// closes one it finds from an earlier version.
	SourceSweep = "hub-sweep"
	evSweep     = "sweep" // a sweep marked, removed or deleted something

	defaultSweepEvery = time.Hour
	minSweepEvery     = 15 * time.Minute
	maxSweepEvery     = 24 * time.Hour
	sweepLoopTick     = time.Minute
	// sweepLoopIdle is the idle time after which a worker still recorded as
	// running (and an agent started by hand) counts as finished: longer than
	// `comebye sweep`'s 30 minutes, because nobody is watching this one.
	sweepLoopIdle = 2 * time.Hour
	// sweepCleanGrace is how long a mark waits before the loop cleans it up: the
	// session is asked first, and the automatic removal gets its look.
	sweepCleanGrace = 20 * time.Minute
	// sweepCleanLook is how often the marked sessions are looked at again
	// between two sweeps.
	sweepCleanLook = 20 * time.Minute
	sweepKeepNames = 12
	// sweepKeepDone is how many removals and deletions the state keeps, and
	// sweepDoneWindow how long the review and the board show them.
	sweepKeepDone   = 100
	sweepDoneWindow = 7 * 24 * time.Hour
)

// sweepLoopSettings is the "sweep_loop" part of settings.json. A nil Enabled
// is the default (on); a nil EveryMinutes is an hour. Enabled is the one switch,
// "Clean up automatically": off, the loop only marks.
type sweepLoopSettings struct {
	Enabled      *bool `json:"enabled,omitempty"`
	EveryMinutes *int  `json:"every_minutes,omitempty"`
	// AutoDeleteBranches is the old second switch (batched confirm or not for a
	// stale remote branch). It is read so an old file still loads, never acted on
	// and dropped when the settings are written again.
	AutoDeleteBranches *bool `json:"auto_delete_branches,omitempty"`
	// ArchiveAfterDays is the archive clock (archived.go): a finished session
	// with no progress for this long is archived (default 7; delivered kinds
	// at most 3).
	ArchiveAfterDays *int `json:"archive_after_days,omitempty"`
}

func normalizeSweepLoop(in *sweepLoopSettings) *sweepLoopSettings {
	if in == nil {
		return nil
	}
	out := &sweepLoopSettings{}
	if in.Enabled != nil {
		v := *in.Enabled
		out.Enabled = &v
	}
	if in.EveryMinutes != nil {
		n := min(max(*in.EveryMinutes, int(minSweepEvery/time.Minute)), int(maxSweepEvery/time.Minute))
		out.EveryMinutes = &n
	}
	if in.ArchiveAfterDays != nil {
		n := min(max(*in.ArchiveAfterDays, 1), maxArchiveAfterDays)
		out.ArchiveAfterDays = &n
	}
	if out.Enabled == nil && out.EveryMinutes == nil && out.ArchiveAfterDays == nil {
		return nil
	}
	return out
}

// sweepLoopConf is the loop's setting in force. On is "Clean up automatically".
type sweepLoopConf struct {
	On     bool          `json:"enabled"`
	Every  time.Duration `json:"-"`
	Source string        `json:"source"` // "setting" or "default"
}

func (s *Server) sweepLoopConf() sweepLoopConf {
	c := sweepLoopConf{On: true, Every: defaultSweepEvery, Source: "default"}
	if v := s.settings.get().SweepLoop; v != nil {
		if v.Enabled != nil {
			c.On, c.Source = *v.Enabled, "setting"
		}
		if v.EveryMinutes != nil {
			c.Every, c.Source = time.Duration(*v.EveryMinutes)*time.Minute, "setting"
		}
	}
	return c
}

// sweepSession is one session a sweep marked, as the board and the item name it.
type sweepSession struct {
	Name   string `json:"name"`
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// Kinds of sweepDone.
const (
	doneSession = "session" // a session removed (its worktree and local branch with it)
	doneBranch  = "branch"  // a stale remote branch deleted
	// doneArchived (archived.go): a session archived, Freed its build output.
)

// sweepDone is one thing the loop did: a session it removed, or a remote
// branch it deleted. Tip is the SHA the branch was at, so it can be restored
// with Restore.
type sweepDone struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Name    string    `json:"name"` // the session's name
	Repo    string    `json:"repo,omitempty"`
	Host    string    `json:"host,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	Tip     string    `json:"tip,omitempty"`
	Restore string    `json:"restore,omitempty"`
	// Backup is the ref on Host that keeps the deleted branch's tip.
	Backup string `json:"backup,omitempty"`
	// Note: a session removed but its worktree kept, and why; for an
	// archived one, why it was archived.
	Note string `json:"note,omitempty"`
	// Freed is the build output an archive cleared, in bytes.
	Freed int64 `json:"freed,omitempty"`
}

// restoreBranchCmd is the command that puts a deleted remote branch back.
func restoreBranchCmd(tip, branch string) string {
	return "git push origin " + tip + ":refs/heads/" + branch
}

// sweepLoopState is sweeploop.json.
type sweepLoopState struct {
	LastRunAt     time.Time      `json:"last_run_at"`
	Runs          int            `json:"runs"`
	LastMarked    int            `json:"last_marked"`
	LastNames     []sweepSession `json:"last_names,omitempty"`
	LastNeedsLook int            `json:"last_needs_look"`
	// LastSettled: gone sessions whose leftovers the last run settled.
	LastSettled int    `json:"last_settled,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	// CleanLook is when the marked sessions were last looked at for cleaning.
	CleanLook time.Time `json:"clean_look"`
	// Done is what the loop removed and deleted, newest first, kept for
	// sweepDoneWindow; LastDoneAt is when it last did anything.
	Done       []sweepDone `json:"done,omitempty"`
	LastDoneAt time.Time   `json:"last_done_at"`
	// Holds are the hosts whose destroy timer the hub holds (archived.go).
	Holds []string `json:"holds,omitempty"`
}

type sweepLoop struct {
	mu      sync.Mutex
	path    string // "" keeps it in memory
	st      sweepLoopState
	running bool
	// tick, grace and look are sweepLoopTick, sweepCleanGrace and sweepCleanLook;
	// the dev server (-test-mode) shortens them so a browser run sees it all.
	tick, grace, look time.Duration
	// archiveAge and recheck replace the archive clocks and the daily proof
	// (0: the settings and archiveRecheck); -test-archive-after sets them.
	archiveAge, recheck time.Duration
}

func openSweepLoop(path string, testMode bool) *sweepLoop {
	l := &sweepLoop{path: path, tick: sweepLoopTick, grace: sweepCleanGrace, look: sweepCleanLook}
	if testMode {
		l.tick, l.grace, l.look = time.Second, 3*time.Second, 2*time.Second
	}
	if path == "" {
		return l
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("sweep loop: %v (starting with none)", err)
		}
		return l
	}
	if err := json.Unmarshal(b, &l.st); err != nil {
		log.Printf("sweep loop: %s: %v (starting with none)", path, err)
		l.st = sweepLoopState{}
	}
	return l
}

// saveLocked writes the state (best effort: a failed write only means a
// restart may sweep once more).
func (l *sweepLoop) saveLocked() {
	if l.path == "" {
		return
	}
	b, err := json.MarshalIndent(l.st, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(l.path), 0o700)
	}
	if err == nil {
		tmp := l.path + ".tmp"
		if err = os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
			err = os.Rename(tmp, l.path)
		}
	}
	if err != nil {
		log.Printf("sweep loop: save: %v", err)
	}
}

func (l *sweepLoop) get() sweepLoopState {
	if l == nil {
		return sweepLoopState{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.st
	st.LastNames = append([]sweepSession(nil), st.LastNames...)
	st.Done = append([]sweepDone(nil), st.Done...)
	st.Holds = append([]string(nil), st.Holds...)
	return st
}

// sweepLoopStep is the janitor's sweep stage (janitor.go): sweep when one is
// due, then, with clean-up on, look at the marked sessions (after a sweep, or
// every sweepCleanLook). It reports whether a sweep ran. One step at a time.
func (s *Server) sweepLoopStep(ctx context.Context, now time.Time) bool {
	l := s.sweeps
	if l == nil || s.ledger == nil {
		return false
	}
	conf := s.sweepLoopConf()
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return false
	}
	l.running = true
	due := l.st.LastRunAt.IsZero() || now.Sub(l.st.LastRunAt) >= conf.Every
	l.mu.Unlock()
	defer func() { l.mu.Lock(); l.running = false; l.mu.Unlock() }()
	if due {
		s.sweepOnce(ctx, now, conf.On)
	}
	s.sweepCloseOldItem()
	if conf.On {
		s.sweepCleanStep(ctx, now, due)
	}
	// Finished work with no progress for days is put away by the janitor's
	// next stage, archive (janitor.go), after each sweep.
	return due
}

// sweepOnce runs one sweep as the hub and records it. clean is the "Clean up
// automatically" setting: off, no remote branch is deleted either.
func (s *Server) sweepOnce(ctx context.Context, now time.Time, clean bool) sweepResult {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res := s.sweep(ctx, sweepBody{IdleAfter: sweepLoopIdle.String()}, "", AutoCreator)
	settled, deleted := s.leftovers(ctx, now, clean)
	var names []sweepSession
	for _, g := range [][]sweepItem{res.Ready, res.Gone} {
		for _, it := range g {
			if it.Marked {
				names = append(names, sweepSession{Name: orDefault(it.Name, it.ID), Repo: it.Repo, Branch: it.Branch})
			}
		}
	}
	l := s.sweeps
	l.mu.Lock()
	l.st.LastRunAt, l.st.LastError = now, ""
	l.st.Runs++
	l.st.LastMarked, l.st.LastNeedsLook = len(names), len(res.NeedsLook)
	l.st.LastSettled = settled
	l.st.LastNames = names[:min(len(names), sweepKeepNames)]
	l.saveLocked()
	l.mu.Unlock()
	if len(names) > 0 {
		list := make([]string, 0, len(names))
		for _, n := range names[:min(len(names), 6)] {
			list = append(list, safeText(n.Name))
		}
		more := ""
		if len(names) > len(list) {
			more = fmt.Sprintf(" and %d more", len(names)-len(list))
		}
		s.logEvent(evSweep, "", "", "", fmt.Sprintf("Sweep marked %d %s ready to remove: %s%s.", len(names), plural(len(names), "session", "sessions"), strings.Join(list, ", "), more))
	}
	s.recordDone(now, deleted)
	return res
}

// recordDone keeps what the loop did in its state, on the board's line and in
// the log: for a branch, its name and the SHA it was at, with the command that
// puts it back.
func (s *Server) recordDone(now time.Time, done []sweepDone) {
	if len(done) == 0 {
		return
	}
	l := s.sweeps
	l.mu.Lock()
	l.st.Done = append(append([]sweepDone(nil), done...), l.st.Done...)
	keep := l.st.Done[:0]
	for _, d := range l.st.Done {
		if len(keep) < sweepKeepDone && now.Sub(d.At) < sweepDoneWindow {
			keep = append(keep, d)
		}
	}
	l.st.Done, l.st.LastDoneAt = keep, now
	l.saveLocked()
	l.mu.Unlock()
	var sessions, branches []string
	var archived []sweepDone
	for _, d := range done {
		switch d.Kind {
		case doneSession:
			t := safeText(d.Name)
			if d.Note != "" {
				t += " (worktree kept: " + safeText(d.Note) + ")"
			}
			sessions = append(sessions, t)
		case doneArchived:
			archived = append(archived, d)
		case doneBranch:
			t := fmt.Sprintf("origin/%s was at %s", safeText(d.Branch), d.Tip)
			if d.Backup != "" {
				t += ", kept on " + safeText(d.Host) + " as " + safeText(d.Backup)
			}
			branches = append(branches, t)
		}
	}
	if n := len(sessions); n > 0 {
		s.logEvent(evSweep, "", "", "", fmt.Sprintf("Sweep removed %d %s with nothing to lose (clean, every commit in the default branch, nothing unpushed): %s.", n, plural(n, "session", "sessions"), strings.Join(sessions, ", ")))
	}
	if n := len(archived); n > 0 {
		s.logEvent(evSweep, "", "", "", "Sweep "+archivedText(archived, 6)+" (nothing else deleted: worktree source, branches and transcripts are kept; `comebye archived` lists them).")
	}
	if n := len(branches); n > 0 {
		s.logEvent(evSweep, "", "", "", fmt.Sprintf("Sweep deleted %d stale remote %s (every commit is in the default branch, or was rebased into it; no open PR): %s. Restore one on its host with git push origin <sha>:refs/heads/<name>.", n, plural(n, "branch", "branches"), strings.Join(branches, "; ")))
	}
}

// sweepDoneSince lists what the loop did within sweepDoneWindow, newest first.
func (s *Server) sweepDoneSince(now time.Time) []sweepDone {
	out := []sweepDone{}
	for _, d := range s.sweeps.get().Done {
		if now.Sub(d.At) < sweepDoneWindow {
			out = append(out, d)
		}
	}
	return out
}

// sweepBranchesDeleted lists the stale remote branches the loop deleted in the
// last day, newest first, each with the SHA it was at and the command that puts
// it back; the removals review shows them next to the sessions it removed.
func (s *Server) sweepBranchesDeleted(now time.Time) []sweepDone {
	out := []sweepDone{}
	for _, d := range s.sweepDoneSince(now) {
		if d.Kind == doneBranch && now.Sub(d.At) < AutoRemoveWindow {
			out = append(out, d)
		}
	}
	return out
}

// sweepBoardLine is the "Hub did" lane's line for the loop: when it last ran,
// what it marked, and what it removed and deleted since. ok is false when there
// is nothing to say (it has not run).
func (s *Server) sweepBoardLine() (title string, at time.Time, ok bool) {
	st := s.sweeps.get()
	if st.LastRunAt.IsZero() {
		return "", time.Time{}, false
	}
	at = st.LastRunAt
	var parts []string
	// What it did in the last day, and when it last did any.
	now := time.Now()
	var sessions, branches, archived []sweepDone
	for _, d := range st.Done {
		if now.Sub(d.At) >= 24*time.Hour {
			continue
		}
		switch d.Kind {
		case doneBranch:
			branches = append(branches, d)
		case doneArchived:
			archived = append(archived, d)
		default:
			sessions = append(sessions, d)
		}
	}
	if len(sessions) > 0 {
		names := make([]string, 0, 3)
		for _, d := range sessions[:min(len(sessions), 3)] {
			names = append(names, safeText(d.Name))
		}
		p := fmt.Sprintf("removed %d %s (%s", len(sessions), plural(len(sessions), "session", "sessions"), strings.Join(names, ", "))
		if len(sessions) > len(names) {
			p += fmt.Sprintf(", +%d", len(sessions)-len(names))
		}
		parts = append(parts, p+")")
	}
	if len(branches) > 0 {
		names := make([]string, 0, 3)
		for _, d := range branches[:min(len(branches), 3)] {
			names = append(names, "origin/"+safeText(d.Branch)+" @ "+short(d.Tip))
		}
		p := fmt.Sprintf("deleted %d stale remote %s (%s", len(branches), plural(len(branches), "branch", "branches"), strings.Join(names, ", "))
		if len(branches) > len(names) {
			p += fmt.Sprintf(", +%d", len(branches)-len(names))
		}
		parts = append(parts, p+")")
	}
	if len(archived) > 0 {
		parts = append(parts, archivedText(archived, 3))
	}
	if len(parts) > 0 && st.LastDoneAt.After(at) {
		at = st.LastDoneAt
	}
	if st.LastMarked > 0 {
		parts = append(parts, fmt.Sprintf("marked %d ready to remove", st.LastMarked))
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing to clean up")
	}
	if st.LastNeedsLook > 0 {
		parts = append(parts, fmt.Sprintf("%d need a look", st.LastNeedsLook))
	}
	title = "Sweep: " + strings.Join(parts, "; ")
	if !s.sweepLoopConf().On {
		title += " (automatic clean-up is off: it only marks)"
	}
	return title, at, true
}

// sweepLoopView is what the settings answer says about the loop.
type sweepLoopView struct {
	Enabled      bool       `json:"enabled"`
	EveryMinutes int        `json:"every_minutes"`
	Source       string     `json:"source"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	LastMarked   int        `json:"last_marked"`
	NeedsLook    int        `json:"last_needs_look"`
	NextRunAt    *time.Time `json:"next_run_at,omitempty"`
	LastDoneAt   *time.Time `json:"last_done_at,omitempty"`
	// Hub: this host has no ledger, so it cannot sweep.
	Hub bool `json:"hub"`
}

func (s *Server) sweepLoopView() sweepLoopView {
	c := s.sweepLoopConf()
	v := sweepLoopView{Enabled: c.On, EveryMinutes: int(c.Every / time.Minute), Source: c.Source, Hub: s.ledger != nil}
	st := s.sweeps.get()
	if !st.LastRunAt.IsZero() {
		t := st.LastRunAt
		v.LastRunAt, v.LastMarked, v.NeedsLook = &t, st.LastMarked, st.LastNeedsLook
		n := t.Add(c.Every)
		v.NextRunAt = &n
	}
	if !st.LastDoneAt.IsZero() {
		t := st.LastDoneAt
		v.LastDoneAt = &t
	}
	return v
}

// archivedText says what the loop archived: "archived 4 sessions (a, b, c, +1),
// freed 31 GB of build output".
func archivedText(ds []sweepDone, show int) string {
	names := make([]string, 0, show)
	var freed int64
	for i, d := range ds {
		if i < show {
			names = append(names, safeText(d.Name))
		}
		freed += d.Freed
	}
	p := fmt.Sprintf("archived %d %s (%s", len(ds), plural(len(ds), "session", "sessions"), strings.Join(names, ", "))
	if len(ds) > len(names) {
		p += fmt.Sprintf(", +%d", len(ds)-len(names))
	}
	p += ")"
	if freed > 0 {
		p += ", freed " + buildout.Human(freed) + " of build output"
	}
	return p
}
