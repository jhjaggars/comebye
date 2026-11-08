// Package ledger keeps the hub's record of handoffs: which worker agent was
// started on which branch, how far it got, and the sessions marked ready to
// remove. It is a small JSON file in the state directory, rewritten
// atomically on every change.
package ledger

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// Status is a ledger entry's state (docs/lifecycle.md):
//
//	running -> finished (the worker reported, or went idle after
//	working; its final report is captured) -> landing (the user OKed
//	landing; `comebye land`) -> landed (it reported again and the hub's
//	check found its work in the base) -> ready_to_remove (marked) ->
//	removed
//
// failed is a handoff whose worker never started. A landing whose check
// fails goes back to finished. A finished or landed worker that gets more
// work goes back to running (a new round); unmarking returns to running,
// finished or landed.
type Status string

const (
	Waiting       Status = "waiting" // queued behind earlier steps of its plan; no worker yet
	Running       Status = "running"
	Finished      Status = "finished"
	Landing       Status = "landing" // approved; the worker is landing its own work
	Landed        Status = "landed"  // the hub's check found the landed work in the base
	ReadyToRemove Status = "ready_to_remove"
	Removed       Status = "removed"
	Failed        Status = "failed"
	// Lost: the host the worker ran on was removed or taken over by another
	// machine (a reused worker name) before the work was reported. The branch
	// and brief remain; nothing follows the new host's panes.
	Lost Status = "lost"
)

// MaxReport caps a captured final report (bytes).
const MaxReport = 20000

// reportTurnGrace is how long after an explicit report the agent may still
// be working without that counting as new work: the turn that ran
// `comebye report` ends within moments. Past it, a worker seen working has
// been given more to do.
const reportTurnGrace = 2 * time.Minute

// maxReportTries bounds how often an empty final report is looked for.
const maxReportTries = 3

// Entry is one handoff.
type Entry struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Repo     string `json:"repo"` // the primary checkout on Host
	RepoName string `json:"repo_name"`
	Branch   string `json:"branch"`
	// PreviousBranches are earlier names of Branch (the worker renamed it).
	PreviousBranches []string `json:"previous_branches,omitempty"`
	Base             string   `json:"base,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	// Model and Thinking are what the handoff asked for (empty: the
	// agent's default).
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	AgentID  string `json:"agent_id,omitempty"` // "<host>/<pane>"
	// HostInstance is the incarnation of Host the worker started on (the
	// hub's peer registry). A host name can be reused by another machine; the
	// entry follows only agents of this instance. Empty: an entry from before
	// instances, or a host without one (the hub's own).
	HostInstance string    `json:"host_instance,omitempty"`
	PaneID       string    `json:"pane_id,omitempty"`
	AgentName    string    `json:"agent_name,omitempty"`
	WorkspaceID  string    `json:"workspace_id,omitempty"`
	Worktree     string    `json:"worktree,omitempty"`
	Reused       bool      `json:"reused,omitempty"`
	Brief        string    `json:"brief"`
	From         string    `json:"from,omitempty"`
	Status       Status    `json:"status"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// SawWorking: the agent was seen working since it started (or since
	// it last finished), so going idle now means it finished.
	SawWorking  bool       `json:"saw_working,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	FinalReport string     `json:"final_report,omitempty"`
	ReportTries int        `json:"report_tries,omitempty"`
	// ReportSource is "explicit" when the worker ran `comebye report`
	// (it declared itself done), else empty (the hub took its last reply
	// when it went idle). AwaitIdle: after an explicit report, the agent
	// still working is the turn that ends with the report, not new work.
	ReportSource string `json:"report_source,omitempty"`
	AwaitIdle    bool   `json:"await_idle,omitempty"`
	// After lists the entries (ids) a waiting handoff is queued behind:
	// it starts when all of them have reported. Kept after it starts.
	After []string `json:"after,omitempty"`
	// PlanID is the plan this handoff is a member of, if any.
	PlanID string `json:"plan_id,omitempty"`
	// ReplyTo is who asked for the handoff, when it was an agent: the hub
	// tells it when the worker finishes. Callback tracks that message.
	ReplyTo  *Ref      `json:"reply_to,omitempty"`
	Callback *Callback `json:"callback,omitempty"`
	// ReportSeq counts the explicit reports (`comebye report`) the worker
	// filed. Noticed says the requester has been told about the entry, and
	// NoticeSeq is the ReportSeq that notice covered: a worker that goes
	// idle again without filing a new report sends no new notice. Both
	// survive the entry going back to running.
	ReportSeq int   `json:"report_seq,omitempty"`
	Noticed   bool  `json:"noticed,omitempty"`
	NoticeSeq int   `json:"notice_seq,omitempty"`
	Mark      *Mark `json:"mark,omitempty"`
	// MarkedFrom is the status the entry had when it was marked, which
	// unmarking restores.
	MarkedFrom Status `json:"marked_from,omitempty"`
	// Approval is the user's OK to land, as relayed by `comebye land`
	// (kept after it landed, until the next landing replaces it).
	Approval *Approval `json:"approval,omitempty"`
	// LandCheck is the hub's check after the landing report.
	LandCheck *LandCheck `json:"land_check,omitempty"`
	RemovedAt *time.Time `json:"removed_at,omitempty"`
	// LostAt is when the entry became Lost (Error says why).
	LostAt *time.Time `json:"lost_at,omitempty"`
	// Removal is the owning host's account of the removal (what was
	// closed, whether the worktree went and why not).
	Removal json.RawMessage `json:"removal,omitempty"`
}

// Approval records who relayed the user's OK to land, when, and what
// they approved: the branch tip then, landed onto Base.
type Approval struct {
	By   string    `json:"by,omitempty"`
	Note string    `json:"note,omitempty"` // the user's words, as relayed
	At   time.Time `json:"at"`
	Base string    `json:"base,omitempty"`
	Tip  string    `json:"tip,omitempty"`
	// ReportedAt is when the worker had reported the work being landed
	// (the entry's FinishedAt then), restored if the landing is cancelled.
	ReportedAt *time.Time `json:"reported_at,omitempty"`
}

// LandCheck is the hub's verdict on a landing: OK when the worktree is
// clean and every approved commit is in the base.
type LandCheck struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Base    string    `json:"base,omitempty"` // the base checked against ("origin/main")
	Verdict string    `json:"verdict"`
	// Landed lists the base commits the work landed as, where found.
	Landed []string `json:"landed,omitempty"`
}

// Ref addresses an agent: its host-qualified id and the name it had then
// (a pane id can be reused by another agent after a restart).
type Ref struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name,omitempty"`
}

// Callback states.
const (
	CallbackPending   = "pending"   // queued, waiting for an idle target
	CallbackDelivered = "delivered" // the requester was prompted
	CallbackExpired   = "expired"   // never delivered within CallbackTTL
)

// CallbackTTL bounds how long a finished worker's callback is retried.
const CallbackTTL = 2 * time.Hour

// Callback is the hub's "your worker finished" message to ReplyTo.
type Callback struct {
	State string    `json:"state"`
	At    time.Time `json:"at"` // queued
	Tries int       `json:"tries,omitempty"`
	// Seq is the entry's ReportSeq when the notice was queued.
	Seq int `json:"seq,omitempty"`
	// To is who got it (the requester, or the coordinator standing in).
	To          string     `json:"to,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// Mark is a "ready to remove" mark, on a ledger entry or on any other
// agent (kept in the ledger's marks, by host-qualified agent id).
type Mark struct {
	AgentID     string `json:"agent_id"`
	Host        string `json:"host,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
	Name        string `json:"name,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Worktree    string `json:"worktree,omitempty"`
	CWD         string `json:"cwd,omitempty"`
	By          string `json:"by,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// Discard proposes throwing the work away: removing the worktree
	// even with uncommitted or unmerged work, and deleting the branch.
	// Only the user's confirmation, in the app, does it.
	Discard bool      `json:"discard,omitempty"`
	At      time.Time `json:"at"`
	// Ask is the marked session's own answer to "is it safe to remove
	// you?" (nil: not asked, e.g. marked with --no-ask).
	Ask *Ask `json:"ask,omitempty"`
}

// Ask states: the session is being asked; it answered SAFE, UNSAFE (with
// a reason) or something else (unclear, the reply in Reason); it did not
// answer in time; or it was not asked (why in Reason).
const (
	AskAsking   = "asking"
	AskSafe     = "safe"
	AskUnsafe   = "unsafe"
	AskUnclear  = "unclear"
	AskNoAnswer = "no_answer"
	AskNotAsked = "not_asked"
)

// Ask is a marked session's answer about its own removal.
type Ask struct {
	State   string     `json:"state"`
	Reason  string     `json:"reason,omitempty"`
	AskedAt *time.Time `json:"asked_at,omitempty"`
	At      time.Time  `json:"at"`
}

func copyMark(m *Mark) *Mark {
	c := *m
	if m.Ask != nil {
		a := *m.Ask
		c.Ask = &a
	}
	return &c
}

// file is the on-disk format.
type file struct {
	Version int           `json:"version"`
	Entries []*Entry      `json:"entries"`
	Marks   []*Mark       `json:"marks"`
	Plans   []*Plan       `json:"plans,omitempty"`
	Audit   []*AuditEvent `json:"audit,omitempty"`
}

// Store is the ledger. Methods are safe for concurrent use and return
// copies.
type Store struct {
	mu   sync.Mutex
	path string
	data file
	// inst is each live agent's instance as of the last Observe (agent id ->
	// instance), so a lookup by agent id can tell an old entry from a reused
	// name. Not persisted.
	inst map[string]string
}

// Open loads the ledger at path, or starts an empty one if the file does
// not exist yet. A file that does not parse is an error (it is never
// overwritten silently).
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: file{Version: 1}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.data.Version = 1
	return s, nil
}

// save writes the ledger atomically (temp file, fsync, rename), mode 0600.
// Callers hold mu.
func (s *Store) save() error {
	b, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ledger-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "h" + hex.EncodeToString(b)
}

func copyEntry(e *Entry) Entry {
	c := *e
	if e.ReplyTo != nil {
		r := *e.ReplyTo
		c.ReplyTo = &r
	}
	if e.Callback != nil {
		cb := *e.Callback
		c.Callback = &cb
	}
	if e.Mark != nil {
		c.Mark = copyMark(e.Mark)
	}
	if e.Approval != nil {
		a := *e.Approval
		c.Approval = &a
	}
	if e.LandCheck != nil {
		l := *e.LandCheck
		l.Landed = append([]string(nil), l.Landed...)
		c.LandCheck = &l
	}
	return c
}

// Entries returns every entry, oldest first.
func (s *Store) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.data.Entries))
	for i, e := range s.data.Entries {
		out[i] = copyEntry(e)
	}
	return out
}

// Get returns the entry with id.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID == id {
			return copyEntry(e), true
		}
	}
	return Entry{}, false
}

// Find returns the newest entry match accepts.
func (s *Store) Find(match func(*Entry) bool) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.data.Entries) - 1; i >= 0; i-- {
		if e := s.data.Entries[i]; match(e) {
			return copyEntry(e), true
		}
	}
	return Entry{}, false
}

// Add records a new entry (its ID and CreatedAt are set here).
func (s *Store) Add(e Entry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.ID = newID()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	s.data.Entries = append(s.data.Entries, &e)
	return copyEntry(&e), s.save()
}

// Update applies fn to the entry with id and saves. It reports false when
// there is no such entry.
func (s *Store) Update(id string, fn func(*Entry)) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID == id {
			fn(e)
			return copyEntry(e), true, s.save()
		}
	}
	return Entry{}, false, nil
}

// active reports whether an entry still has a session to track.
func active(e *Entry) bool { return e.Active() }

// Active reports whether the entry still has a session to track: it is
// running, finished, landing, landed or marked.
func (e Entry) Active() bool {
	switch e.Status {
	case Running, Finished, Landing, Landed, ReadyToRemove:
		return true
	}
	return false
}

// Reported reports whether the worker is stopped with its work reported:
// finished or landed.
func (e Entry) Reported() bool { return e.Status == Finished || e.Status == Landed }

// ByAgent returns the active entry for a host-qualified agent id.
func (s *Store) ByAgent(agentID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.byAgent(agentID); e != nil {
		return copyEntry(e), true
	}
	return Entry{}, false
}

func (s *Store) byAgent(agentID string) *Entry {
	for i := len(s.data.Entries) - 1; i >= 0; i-- {
		if e := s.data.Entries[i]; e.AgentID == agentID && active(e) && !s.otherInstance(e, agentID) {
			return e
		}
	}
	return nil
}

// otherInstance reports whether the live agent now named agentID belongs to
// another incarnation of the host than the entry's.
func (s *Store) otherInstance(e *Entry, agentID string) bool {
	live, ok := s.inst[agentID]
	return e.HostInstance != "" && ok && live != "" && live != e.HostInstance
}

// LoseInstance settles the unfinished entries (running or landing) of an
// incarnation of host that ended to Lost, with why. Entries already reported
// keep their status: their work was reported, and they stop following the
// host's panes anyway (their HostInstance no longer matches).
func (s *Store) LoseInstance(host, instance, why string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if instance == "" {
		return nil, nil
	}
	var out []Entry
	now := time.Now().UTC()
	for _, e := range s.data.Entries {
		if e.Host != host || e.HostInstance != instance || (e.Status != Running && e.Status != Landing) {
			continue
		}
		e.Status, e.Error, e.LostAt = Lost, why, &now
		e.Callback = nil
		out = append(out, copyEntry(e))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, s.save()
}

// Backfill gives the active entries that predate instances the instance their
// host has now (hosts maps host -> instance), assuming the machine there is
// the one that ran them, which is what they have always assumed. It returns
// how many it stamped.
func (s *Store) Backfill(hosts map[string]string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.data.Entries {
		if e.HostInstance != "" || !e.Active() {
			continue
		}
		if in := hosts[e.Host]; in != "" {
			e.HostInstance = in
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.save()
}

// Marks returns the marks on agents that are not ledger workers.
func (s *Store) Marks() []Mark {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Mark, len(s.data.Marks))
	for i, m := range s.data.Marks {
		out[i] = *copyMark(m)
	}
	return out
}

// SetMark marks an agent ready to remove: its ledger entry when it is a
// worker, else a standalone mark (replacing an earlier one). It returns
// the entry when there is one.
func (s *Store) SetMark(m Mark) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.At.IsZero() {
		m.At = time.Now().UTC()
	}
	m = *copyMark(&m)
	if e := s.byAgent(m.AgentID); e != nil {
		if e.Status != ReadyToRemove {
			e.MarkedFrom = e.Status
		}
		e.Mark = &m
		e.Status = ReadyToRemove
		c := copyEntry(e)
		return &c, s.save()
	}
	for i, old := range s.data.Marks {
		if old.AgentID == m.AgentID {
			s.data.Marks[i] = &m
			return nil, s.save()
		}
	}
	s.data.Marks = append(s.data.Marks, &m)
	return nil, s.save()
}

// SetAsk changes the answer on the agent's mark made at markedAt; a mark
// replaced or cleared since is left alone. It reports whether it changed.
func (s *Store) SetAsk(agentID string, markedAt time.Time, a Ask) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m *Mark
	if e := s.byAgent(agentID); e != nil && e.Status == ReadyToRemove && e.Mark != nil {
		m = e.Mark
	}
	for _, sm := range s.data.Marks {
		if sm.AgentID == agentID {
			m = sm
		}
	}
	if m == nil || !m.At.Equal(markedAt) {
		return false, nil
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	m.Ask = &a
	return true, s.save()
}

// ClearMark unmarks an agent: a worker goes back to finished (or running
// when it never finished). It reports whether there was a mark.
func (s *Store) ClearMark(agentID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	if e := s.byAgent(agentID); e != nil && e.Status == ReadyToRemove {
		e.Mark = nil
		e.Status = Running
		if e.FinishedAt != nil {
			e.Status = Finished
		}
		if e.MarkedFrom == Landed || (e.MarkedFrom == Landing && e.Approval != nil) {
			e.Status = e.MarkedFrom
		}
		e.MarkedFrom = ""
		found = true
	}
	kept := s.data.Marks[:0]
	for _, m := range s.data.Marks {
		if m.AgentID == agentID {
			found = true
			continue
		}
		kept = append(kept, m)
	}
	s.data.Marks = kept
	if !found {
		return false, nil
	}
	return true, s.save()
}

// Removed records a confirmed removal: the agent's entry becomes removed
// and its standalone mark (if any) is dropped.
func (s *Store) Removed(agentID string, removal json.RawMessage, now time.Time) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out *Entry
	if e := s.byAgent(agentID); e != nil {
		t := now.UTC()
		e.Status, e.RemovedAt, e.Removal = Removed, &t, removal
		c := copyEntry(e)
		out = &c
	}
	kept := s.data.Marks[:0]
	for _, m := range s.data.Marks {
		if m.AgentID != agentID {
			kept = append(kept, m)
		}
	}
	s.data.Marks = kept
	return out, s.save()
}

// Observe applies an agent list to the running workers: a worker seen
// working is noted, and one that goes idle or done after working is
// finished (and running again if it gets back to work). It returns the
// entries whose final report should be captured now: those that just
// finished and finished ones still without a report.
func (s *Store) Observe(agents []herdr.AgentInfo, now time.Time) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := make(map[string]*herdr.AgentInfo, len(agents))
	s.inst = make(map[string]string, len(agents))
	for i := range agents {
		byID[agents[i].ID] = &agents[i]
		s.inst[agents[i].ID] = agents[i].Instance
	}
	changed := false
	var capture []Entry
	for _, e := range s.data.Entries {
		if e.Status != Running && e.Status != Landing && !e.Reported() {
			continue
		}
		a := byID[e.AgentID]
		if a == nil {
			continue
		}
		if e.HostInstance != "" && a.Instance != "" && a.Instance != e.HostInstance {
			// The host name now belongs to another machine: this agent is not
			// the entry's. An unfinished entry is lost; a reported one stays.
			if e.Status == Running || e.Status == Landing {
				t := now.UTC()
				e.Status, e.LostAt, e.Callback = Lost, &t, nil
				e.Error = "its host " + e.Host + " was replaced by another machine (instance " + a.Instance + ")"
				changed = true
			}
			continue
		}
		if e.Status == Landing {
			// Landing is the worker's own work: it does not start a new
			// round, and going idle after it is the landing report.
			switch a.Status {
			case herdr.StatusWorking:
				if !e.SawWorking && e.FinishedAt == nil {
					e.SawWorking, changed = true, true
				}
			case herdr.StatusIdle, herdr.StatusDone:
				if e.AwaitIdle {
					e.AwaitIdle, e.SawWorking, changed = false, false, true
				}
				if e.SawWorking && e.FinishedAt == nil {
					t := now.UTC()
					e.FinishedAt, e.SawWorking, e.ReportTries, e.ReportSource = &t, false, 0, ""
					changed = true
					capture = append(capture, copyEntry(e))
				} else if e.FinishedAt != nil && e.FinalReport == "" && e.ReportTries < maxReportTries {
					capture = append(capture, copyEntry(e))
				}
			}
			continue
		}
		switch a.Status {
		case herdr.StatusWorking:
			if !e.SawWorking {
				e.SawWorking, changed = true, true
			}
			if e.Reported() && !(e.AwaitIdle && e.FinishedAt != nil && now.Sub(*e.FinishedAt) < reportTurnGrace) {
				// A new round. The worker's own report stays until it files
				// another: what it says next on going idle is not a report
				// (SetReport leaves an explicit one alone). A guessed report
				// is cleared, to be guessed again.
				if e.ReportSource != "explicit" {
					e.FinalReport, e.ReportTries = "", 0
				}
				e.Status, e.FinishedAt = Running, nil
				e.Callback = nil
				changed = true
			}
		case herdr.StatusIdle, herdr.StatusDone:
			if e.Reported() && e.AwaitIdle {
				e.AwaitIdle, e.SawWorking, changed = false, false, true
			}
			if e.Status == Running && e.SawWorking {
				t := now.UTC()
				e.Status, e.FinishedAt, e.SawWorking, e.ReportTries = Finished, &t, false, 0
				queueNotice(e, t)
				changed = true
				capture = append(capture, copyEntry(e))
			} else if e.Reported() && e.FinalReport == "" && e.ReportTries < maxReportTries {
				capture = append(capture, copyEntry(e))
			}
		}
	}
	if changed {
		_ = s.save()
	}
	return capture
}

// Queued lists the handoffs waiting for earlier steps, oldest first.
func (s *Store) Queued() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.data.Entries {
		if e.Status == Waiting {
			out = append(out, copyEntry(e))
		}
	}
	return out
}

// PendingCallbacks lists the finished entries whose callback is waiting.
func (s *Store) PendingCallbacks() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.data.Entries {
		if e.Reported() && e.Callback != nil && e.Callback.State == CallbackPending {
			out = append(out, copyEntry(e))
		}
	}
	return out
}

// CallbackTried counts a failed delivery attempt on a pending callback.
func (s *Store) CallbackTried(id string, at time.Time) error {
	return s.setCallback(id, at, func(c *Callback) { c.Tries++ })
}

// CallbackDone settles a pending callback as delivered (to) or expired.
// at is the callback's queue time: a worker that finished again since
// has a newer callback, which this leaves alone.
func (s *Store) CallbackDone(id string, at time.Time, state, to string, now time.Time) error {
	return s.setCallback(id, at, func(c *Callback) { settleCallback(c, state, to, now) })
}

func (s *Store) setCallback(id string, at time.Time, fn func(*Callback)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID == id && e.Callback != nil && e.Callback.State == CallbackPending && e.Callback.At.Equal(at) {
			fn(e.Callback)
			if e.Callback.State != CallbackPending {
				// Told (or given up on): remember what the notice covered.
				e.Noticed, e.NoticeSeq = true, e.Callback.Seq
			}
			return s.save()
		}
	}
	return nil
}

// Report records a worker's explicit report: the agent's active entry is
// finished now with the report (capped), without waiting to see it go idle.
// A second report while still finished replaces the text and queues no
// second callback. ok is false when the agent has no running or finished
// entry.
func (s *Store) Report(agentID, text string, now time.Time) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byAgent(agentID)
	if e == nil || (e.Status != Running && e.Status != Landing && e.Status != ReadyToRemove && !e.Reported()) {
		return Entry{}, false, nil
	}
	if e.Status == ReadyToRemove {
		// A marked worker that reports again is at work again: the mark
		// no longer holds, and this is a new round's report.
		e.Mark, e.MarkedFrom, e.Status = nil, "", Finished
	}
	t := now.UTC()
	if e.Status == Landing {
		// The landing report: the hub checks the work before it counts
		// as landed (SetLandCheck), and tells the requester then.
		e.FinishedAt = &t
		e.FinalReport, e.ReportSource, e.ReportTries = Truncate(text, MaxReport), "explicit", 0
		e.AwaitIdle, e.SawWorking = true, false
		return copyEntry(e), true, s.save()
	}
	if e.Status == Landed {
		if e.AwaitIdle {
			// Still the landing turn (a corrected landing report).
			e.FinalReport, e.ReportTries = Truncate(text, MaxReport), 0
			return copyEntry(e), true, s.save()
		}
		e.Status = Finished // a report after landing is a new round's
	}
	e.ReportSeq++
	e.Status, e.FinishedAt = Finished, &t
	queueNotice(e, t)
	e.FinalReport, e.ReportSource, e.ReportTries = Truncate(text, MaxReport), "explicit", 0
	e.AwaitIdle, e.SawWorking = true, false
	return copyEntry(e), true, s.save()
}

// queueNotice queues the requester's "your worker finished" notice when
// there is something new for it: the worker filed a report since the last
// notice, or it was never told about this entry at all (a worker that
// forgets to report still surfaces once). A notice already waiting is
// left alone (it points at the newest report). Going idle again without a
// new report queues nothing.
func queueNotice(e *Entry, at time.Time) {
	if e.ReplyTo == nil {
		return
	}
	if e.Callback != nil && e.Callback.State == CallbackPending {
		return
	}
	if e.Noticed && e.ReportSeq <= e.NoticeSeq {
		return
	}
	e.Callback = &Callback{State: CallbackPending, At: at, Seq: e.ReportSeq}
}

// ReportPending reports whether the entry's final report is still being
// looked for (so a message that points at it should wait).
func (e Entry) ReportPending() bool {
	return e.FinalReport == "" && e.ReportTries < maxReportTries
}

// StartLanding records the user's OK to land a finished worker's work
// (relayed by `comebye land`) and moves it to landing. A landing that has
// not reported yet may be started again (a retry); its approval is
// replaced. ok is false when the agent has no finished entry with a
// report, or a landing that already reported.
func (s *Store) StartLanding(agentID string, a Approval) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byAgent(agentID)
	switch {
	case e == nil:
		return Entry{}, false, nil
	case e.Status == Finished && e.FinalReport != "":
		a.ReportedAt = e.FinishedAt
	case e.Status == Landing && e.FinishedAt == nil && e.Approval != nil:
		a.ReportedAt = e.Approval.ReportedAt
	default:
		return copyEntry(e), false, nil
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	e.Status, e.Approval, e.FinishedAt = Landing, &a, nil
	e.SawWorking, e.AwaitIdle = false, false
	return copyEntry(e), true, s.save()
}

// CancelLanding undoes StartLanding when the land prompt could not be
// sent: a landing that has not started working goes back to finished.
func (s *Store) CancelLanding(agentID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byAgent(agentID)
	if e == nil || e.Status != Landing || e.FinishedAt != nil || e.SawWorking || e.Approval == nil || e.Approval.ReportedAt == nil {
		return false, nil
	}
	e.Status, e.FinishedAt = Finished, e.Approval.ReportedAt
	e.Approval = nil
	return true, s.save()
}

// LandingsToCheck lists the landings whose report is in and whose check
// is due.
func (s *Store) LandingsToCheck() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.data.Entries {
		if e.Status == Landing && e.FinishedAt != nil {
			out = append(out, copyEntry(e))
		}
	}
	return out
}

// SetLandCheck settles a landing that reported at finishedAt: landed when
// the check is OK, else back to finished with the verdict. The requester
// is told now, with the outcome. A landing that went back to work or was
// marked since is left alone.
func (s *Store) SetLandCheck(id string, finishedAt time.Time, c LandCheck) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID != id {
			continue
		}
		if e.Status != Landing || e.FinishedAt == nil || !e.FinishedAt.Equal(finishedAt) {
			return false, nil
		}
		if c.At.IsZero() {
			c.At = time.Now().UTC()
		}
		c.Landed = append([]string(nil), c.Landed...)
		e.LandCheck = &c
		e.Status = Finished
		if c.OK {
			e.Status = Landed
		}
		if e.ReplyTo != nil {
			e.Callback = &Callback{State: CallbackPending, At: c.At, Seq: e.ReportSeq}
		}
		return true, s.save()
	}
	return false, nil
}

// SetReport stores a captured final report for an entry that is still
// finished (since finishedAt), counting the attempt.
func (s *Store) SetReport(id string, finishedAt *time.Time, report string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID != id {
			continue
		}
		if e.ReportSource == "explicit" {
			return nil // the worker's own report is not replaced by a guess
		}
		if e.FinishedAt == nil || finishedAt == nil || !e.FinishedAt.Equal(*finishedAt) {
			return nil // it went back to work meanwhile
		}
		e.ReportTries++
		e.FinalReport = Truncate(report, MaxReport)
		return s.save()
	}
	return nil
}

// Truncate cuts s to at most n bytes on a rune boundary, marking the cut.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "\n\n[… truncated]"
}
