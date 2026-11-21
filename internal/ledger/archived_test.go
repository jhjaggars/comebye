package ledger

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

// The round trip: archive, a restart in between, unarchive; history both ways.
func TestArchiveRoundTrip(t *testing.T) {
	s, path := openTemp(t)
	fin := time.Now().UTC().Add(-8 * 24 * time.Hour)
	e, _ := s.Add(Entry{Host: "desk", AgentID: "desk/w1:p1", AgentName: "w1", Branch: "feat/x", Status: Finished, FinishedAt: &fin, FinalReport: "done"})
	got, err := s.Archive(e.ID, Archival{By: "hub", Why: "7 days without progress", BuildFreed: 52 << 20, BuildDirs: []string{"target"}, Unpushed: 2})
	if err != nil || got.Status != Archived || got.Archive == nil || got.Archive.From != Finished {
		t.Fatalf("archive = %+v %v", got, err)
	}
	if !got.Active() {
		t.Fatal("an archived entry still has a session to track (resume finds it)")
	}
	if _, ok := s.ByAgent("desk/w1:p1"); !ok {
		t.Fatal("ByAgent does not find the archived entry")
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := again.Get(e.ID)
	if r.Status != Archived || r.Archive == nil || r.Archive.BuildFreed != 52<<20 || r.Archive.Unpushed != 2 || len(r.ArchiveHistory) != 1 {
		t.Fatalf("after restart = %+v", r)
	}
	if n := len(again.ArchivedEntries()); n != 1 {
		t.Fatalf("ArchivedEntries = %d", n)
	}
	if _, err := again.Archive(e.ID, Archival{}); !errors.Is(err, ErrNotArchiving) {
		t.Fatalf("archiving twice: %v", err)
	}
	back, err := again.Unarchive(e.ID, "user", "restored")
	if err != nil || back.Status != Finished || back.Archive != nil || len(back.ArchiveHistory) != 2 || back.ArchiveHistory[1].Event != "unarchived" {
		t.Fatalf("unarchive = %+v %v", back, err)
	}
	if _, err := again.Unarchive(e.ID, "user", ""); !errors.Is(err, ErrNotArchived) {
		t.Fatalf("unarchive twice: %v", err)
	}
}

// A marked entry's mark moves into the record (the review forgets it), its
// unsettled LEFT items become Left, and unarchiving does not restore it.
func TestArchiveMarked(t *testing.T) {
	s, _ := openTemp(t)
	e, _ := s.Add(Entry{Host: "desk", AgentID: "desk/w2:p1", Branch: "poc/x", Status: Landed})
	m := Mark{AgentID: "desk/w2:p1", Reason: "sweep", Ask: &Ask{State: AskUnsafe, Items: []AskItem{
		{Type: ItemCleanup, Text: "a git stash entry"},
		{Type: ItemBranch, Branch: "poc/x", Done: &ItemDone{How: "deleted"}},
	}}}
	if _, err := s.SetMark(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.Archive(e.ID, Archival{By: "hub"})
	if err != nil || got.Mark != nil || got.MarkedFrom != "" || got.Archive.Mark == nil || got.Archive.From != Landed {
		t.Fatalf("archive = %+v %v", got, err)
	}
	if len(got.Archive.Left) != 1 || got.Archive.Left[0].Text != "a git stash entry" {
		t.Fatalf("left = %+v", got.Archive.Left)
	}
	back, _ := s.Unarchive(e.ID, "user", "")
	if back.Status != Landed || back.Mark != nil {
		t.Fatalf("unarchive = %+v", back)
	}
	r, _ := s.Add(Entry{Host: "desk", AgentID: "desk/w3:p1", Branch: "b", Status: Running})
	if _, err := s.Archive(r.ID, Archival{}); !errors.Is(err, ErrNotArchiving) {
		t.Fatalf("a running entry was archived: %v", err)
	}
}

// It comes back by itself: its agent working again (a new round), a report,
// a progress note, a resume. Another agent in the reused pane does not count.
func TestArchiveComesBack(t *testing.T) {
	now := time.Now().UTC()
	mk := func(s *Store, id string) Entry {
		e, _ := s.Add(Entry{Host: "desk", AgentID: id, AgentName: "w", Branch: "feat/" + id, Status: Finished, FinishedAt: &now, FinalReport: "r", ReportSource: "explicit"})
		if _, err := s.Archive(e.ID, Archival{By: "hub"}); err != nil {
			t.Fatal(err)
		}
		return e
	}
	s, _ := openTemp(t)
	a := mk(s, "desk/w1:p1")
	s.Observe([]herdr.AgentInfo{{ID: "desk/w1:p1", Name: "other", Status: herdr.StatusWorking}}, now)
	if g, _ := s.Get(a.ID); g.Status != Archived {
		t.Fatalf("another agent in the pane unarchived it: %s", g.Status)
	}
	s.Observe([]herdr.AgentInfo{{ID: "desk/w1:p1", Name: "w", Status: herdr.StatusWorking}}, now)
	if g, _ := s.Get(a.ID); g.Status != Running || g.Archive != nil || g.FinalReport != "r" {
		t.Fatalf("working again = %+v", g)
	}

	b := mk(s, "desk/w2:p1")
	if _, ok, _ := s.Report("desk/w2:p1", "new report", now); !ok {
		t.Fatal("report refused")
	}
	if g, _ := s.Get(b.ID); g.Status != Finished || g.FinalReport != "new report" || g.Archive != nil {
		t.Fatalf("after report = %+v", g)
	}

	c := mk(s, "desk/w3:p1")
	if _, ok, _ := s.Note("desk/w3:p1", "on it", "", now); !ok {
		t.Fatal("note refused")
	}
	if g, _ := s.Get(c.ID); g.Status != Running {
		t.Fatalf("after note = %s", g.Status)
	}

	d := mk(s, "desk/w4:p1")
	if _, err := s.NotePark("desk/w4:p1", ParkEvent{ID: "pk1", Event: "resumed", At: time.Now().UTC().Add(time.Second)}, &Where{AgentID: "desk/w9:p1", PaneID: "w9:p1"}); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.Get(d.ID); g.Status != Finished || g.AgentID != "desk/w9:p1" {
		t.Fatalf("after resume = %+v", g)
	}
}

// Marking archived work (Propose removal) takes it out of the archive into the
// review; unmarking returns it to the status it was archived from.
func TestMarkArchived(t *testing.T) {
	s, _ := openTemp(t)
	now := time.Now().UTC()
	e, _ := s.Add(Entry{Host: "desk", AgentID: "desk/w1:p1", Branch: "poc/x", Status: Landed, FinishedAt: &now})
	s.Archive(e.ID, Archival{By: "hub"})
	got, err := s.SetMark(Mark{AgentID: "desk/w1:p1", Reason: "propose removal", By: "me"})
	if err != nil || got == nil || got.Status != ReadyToRemove || got.Archive != nil || got.MarkedFrom != Landed {
		t.Fatalf("mark = %+v %v", got, err)
	}
	if _, err := s.ClearMark("desk/w1:p1"); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.Get(e.ID); g.Status != Landed || g.Archive != nil {
		t.Fatalf("unmark = %s %+v", g.Status, g.Archive)
	}
}
