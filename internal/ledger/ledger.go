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

// Status is a ledger entry's state:
//
//	running -> finished (the worker went idle after working; its final
//	report is captured) -> ready_to_remove (marked) -> removed
//
// failed is a handoff whose worker never started. A finished worker that
// gets more work goes back to running; unmarking returns to running or
// finished.
type Status string

const (
	Running       Status = "running"
	Finished      Status = "finished"
	ReadyToRemove Status = "ready_to_remove"
	Removed       Status = "removed"
	Failed        Status = "failed"
)

// MaxReport caps a captured final report (bytes).
const MaxReport = 20000

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
	PreviousBranches []string  `json:"previous_branches,omitempty"`
	Base             string    `json:"base,omitempty"`
	Kind             string    `json:"kind,omitempty"`
	AgentID          string    `json:"agent_id,omitempty"` // "<host>/<pane>"
	PaneID           string    `json:"pane_id,omitempty"`
	AgentName        string    `json:"agent_name,omitempty"`
	WorkspaceID      string    `json:"workspace_id,omitempty"`
	Worktree         string    `json:"worktree,omitempty"`
	Reused           bool      `json:"reused,omitempty"`
	Brief            string    `json:"brief"`
	From             string    `json:"from,omitempty"`
	Status           Status    `json:"status"`
	Error            string    `json:"error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	// SawWorking: the agent was seen working since it started (or since
	// it last finished), so going idle now means it finished.
	SawWorking  bool       `json:"saw_working,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	FinalReport string     `json:"final_report,omitempty"`
	ReportTries int        `json:"report_tries,omitempty"`
	// ReplyTo is who asked for the handoff, when it was an agent: the hub
	// tells it when the worker finishes. Callback tracks that message.
	ReplyTo   *Ref       `json:"reply_to,omitempty"`
	Callback  *Callback  `json:"callback,omitempty"`
	Mark      *Mark      `json:"mark,omitempty"`
	RemovedAt *time.Time `json:"removed_at,omitempty"`
	// Removal is the owning host's account of the removal (what was
	// closed, whether the worktree went and why not).
	Removal json.RawMessage `json:"removal,omitempty"`
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
	Version int      `json:"version"`
	Entries []*Entry `json:"entries"`
	Marks   []*Mark  `json:"marks"`
}

// Store is the ledger. Methods are safe for concurrent use and return
// copies.
type Store struct {
	mu   sync.Mutex
	path string
	data file
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
func active(e *Entry) bool {
	return e.Status == Running || e.Status == Finished || e.Status == ReadyToRemove
}

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
		if e := s.data.Entries[i]; e.AgentID == agentID && active(e) {
			return e
		}
	}
	return nil
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
	for i := range agents {
		byID[agents[i].ID] = &agents[i]
	}
	changed := false
	var capture []Entry
	for _, e := range s.data.Entries {
		if e.Status != Running && e.Status != Finished {
			continue
		}
		a := byID[e.AgentID]
		if a == nil {
			continue
		}
		switch a.Status {
		case herdr.StatusWorking:
			if !e.SawWorking {
				e.SawWorking, changed = true, true
			}
			if e.Status == Finished {
				e.Status, e.FinishedAt, e.FinalReport, e.ReportTries = Running, nil, "", 0
				e.Callback = nil
				changed = true
			}
		case herdr.StatusIdle, herdr.StatusDone:
			if e.Status == Running && e.SawWorking {
				t := now.UTC()
				e.Status, e.FinishedAt, e.SawWorking, e.ReportTries = Finished, &t, false, 0
				if e.ReplyTo != nil {
					e.Callback = &Callback{State: CallbackPending, At: t}
				}
				changed = true
				capture = append(capture, copyEntry(e))
			} else if e.Status == Finished && e.FinalReport == "" && e.ReportTries < maxReportTries {
				capture = append(capture, copyEntry(e))
			}
		}
	}
	if changed {
		_ = s.save()
	}
	return capture
}

// PendingCallbacks lists the finished entries whose callback is waiting.
func (s *Store) PendingCallbacks() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.data.Entries {
		if e.Status == Finished && e.Callback != nil && e.Callback.State == CallbackPending {
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
	return s.setCallback(id, at, func(c *Callback) {
		c.State = state
		if state == CallbackDelivered {
			t := now.UTC()
			c.To, c.DeliveredAt = to, &t
			c.Tries++
		}
	})
}

func (s *Store) setCallback(id string, at time.Time, fn func(*Callback)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID == id && e.Callback != nil && e.Callback.State == CallbackPending && e.Callback.At.Equal(at) {
			fn(e.Callback)
			return s.save()
		}
	}
	return nil
}

// ReportPending reports whether the entry's final report is still being
// looked for (so a message that points at it should wait).
func (e Entry) ReportPending() bool {
	return e.FinalReport == "" && e.ReportTries < maxReportTries
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
