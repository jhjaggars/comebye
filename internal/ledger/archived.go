package ledger

import (
	"errors"
	"time"
)

// Archived is finished work put away (docs/lifecycle.md, "Archived"): the
// work is done and nobody is going to merge it or look at it soon. The entry
// leaves the board's lanes and the removals review; its worktree source,
// branches, transcript and park record are all kept, and only regenerable
// build output is cleared. Git still decides any removal: an archived entry
// whose work later lands goes down the normal path (mark, proof, removal).
//
// It comes back by itself: a report, a progress note, its agent working again
// or resumed, or a new commit on its branch return it to the board.

// Archival is the record an archived entry carries (Entry.Archive), present
// only while it is archived.
type Archival struct {
	At  time.Time `json:"at"`
	By  string    `json:"by"`
	Why string    `json:"why"`
	// From is the status it was archived from, which Unarchive restores (a
	// marked entry goes back to the status it was marked from: its mark is
	// not restored).
	From Status `json:"from"`
	// Mark is the removal mark it had, kept so the "what is left" answer
	// survives while the removals review forgets it; Left are the unsettled
	// items of that answer.
	Mark *Mark     `json:"mark,omitempty"`
	Left []AskItem `json:"left,omitempty"`
	// ParkID is the park record that closed its pane ("" when the agent was
	// already gone or parked before).
	ParkID string `json:"park_id,omitempty"`
	// BuildFreed is the regenerable build output cleared at archive time, in
	// bytes, and BuildDirs the directories (relative to the worktree).
	// BuildPending: the host was not reachable, so the clearing waits for it.
	BuildFreed   int64    `json:"build_freed,omitempty"`
	BuildDirs    []string `json:"build_dirs,omitempty"`
	BuildPending bool     `json:"build_pending,omitempty"`
	// WorktreeBytes is the disk the worktree still uses (measured after the
	// build output went; 0: not measured).
	WorktreeBytes int64 `json:"worktree_bytes,omitempty"`
	// Tip is the branch tip when it was archived (a new commit brings it
	// back); Unmerged and Unpushed are the commits not in the base and, of
	// them, those on no remote, as last checked (-1: not known). A host that
	// holds an archived worktree with Unpushed > 0 is not destroyed by its
	// timer.
	Tip      string `json:"tip,omitempty"`
	Unmerged int    `json:"unmerged"`
	Unpushed int    `json:"unpushed"`
	// CheckedAt is the last time the hub re-ran the proof on it.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

// ArchiveEvent is one archive or unarchive of an entry.
type ArchiveEvent struct {
	Event string    `json:"event"` // "archived" or "unarchived"
	At    time.Time `json:"at"`
	By    string    `json:"by,omitempty"`
	Why   string    `json:"why,omitempty"`
}

// maxArchiveHistory bounds Entry.ArchiveHistory.
const maxArchiveHistory = 20

// Errors of Archive and Unarchive.
var (
	ErrNoEntry      = errors.New("no such entry")
	ErrNotArchiving = errors.New("only a finished, landed or marked entry can be archived")
	ErrNotArchived  = errors.New("the entry is not archived")
)

// Archivable reports whether an entry in this status can be archived.
func Archivable(st Status) bool {
	return st == Finished || st == Landed || st == ReadyToRemove
}

func copyArchival(a *Archival) *Archival {
	c := *a
	if a.Mark != nil {
		c.Mark = copyMark(a.Mark)
	}
	c.Left = append([]AskItem(nil), a.Left...)
	c.BuildDirs = append([]string(nil), a.BuildDirs...)
	if a.CheckedAt != nil {
		t := *a.CheckedAt
		c.CheckedAt = &t
	}
	return &c
}

func noteArchive(e *Entry, ev ArchiveEvent) {
	e.ArchiveHistory = append(e.ArchiveHistory, ev)
	if len(e.ArchiveHistory) > maxArchiveHistory {
		e.ArchiveHistory = e.ArchiveHistory[len(e.ArchiveHistory)-maxArchiveHistory:]
	}
}

// Archive archives the entry with id. a.At, a.From and a.Mark are set here: a
// marked entry's mark moves into the record (its unsettled LEFT items into
// Left when a.Left is empty), so the removals review forgets it.
func (s *Store) Archive(id string, a Archival) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID != id {
			continue
		}
		if !Archivable(e.Status) {
			return copyEntry(e), ErrNotArchiving
		}
		if a.At.IsZero() {
			a.At = time.Now().UTC()
		}
		a.From = e.Status
		if e.Status == ReadyToRemove {
			a.From = e.MarkedFrom
			if a.From == "" || a.From == ReadyToRemove || a.From == Running || a.From == Landing {
				a.From = Finished
			}
		}
		if e.Mark != nil {
			a.Mark = copyMark(e.Mark)
			if len(a.Left) == 0 && e.Mark.Ask != nil && e.Mark.Ask.State == AskUnsafe {
				for _, it := range e.Mark.Ask.Items {
					if it.Done == nil {
						a.Left = append(a.Left, it)
					}
				}
			}
		}
		e.Archive = copyArchival(&a)
		e.Status, e.Mark, e.MarkedFrom = Archived, nil, ""
		e.Callback = nil
		noteArchive(e, ArchiveEvent{Event: "archived", At: a.At, By: a.By, Why: a.Why})
		return copyEntry(e), s.save()
	}
	return Entry{}, ErrNoEntry
}

// UpdateArchive changes an archived entry's record in place (fn gets the
// record itself). It reports false when the entry is not archived.
func (s *Store) UpdateArchive(id string, fn func(a *Archival)) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID == id {
			if e.Status != Archived || e.Archive == nil {
				return copyEntry(e), false, nil
			}
			fn(e.Archive)
			return copyEntry(e), true, s.save()
		}
	}
	return Entry{}, false, nil
}

// Unarchive returns an archived entry to the status it was archived from
// (its mark is not restored). why says what brought it back.
func (s *Store) Unarchive(id, by, why string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.data.Entries {
		if e.ID == id {
			if e.Status != Archived {
				return copyEntry(e), ErrNotArchived
			}
			unarchiveLocked(e, "", by, why, time.Now().UTC())
			return copyEntry(e), s.save()
		}
	}
	return Entry{}, ErrNoEntry
}

// unarchiveLocked takes e out of the archive into to (empty: the status it was
// archived from).
func unarchiveLocked(e *Entry, to Status, by, why string, now time.Time) {
	if to == "" {
		to = Finished
		if e.Archive != nil && (e.Archive.From == Landed || e.Archive.From == Finished) {
			to = e.Archive.From
		}
	}
	if to == Finished && e.FinishedAt == nil {
		t := now
		e.FinishedAt = &t
	}
	e.Status, e.Archive = to, nil
	noteArchive(e, ArchiveEvent{Event: "unarchived", At: now, By: by, Why: why})
}

// ArchivedEntries lists the archived entries, oldest first.
func (s *Store) ArchivedEntries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.data.Entries {
		if e.Status == Archived {
			out = append(out, copyEntry(e))
		}
	}
	return out
}
