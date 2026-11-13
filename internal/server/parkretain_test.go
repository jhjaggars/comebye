package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
)

func TestParkRetentionProposesOnceAndSkipsPinned(t *testing.T) {
	r, _ := newPQRig(t)
	r.s.cfg.ParkRetention = 14 * 24 * time.Hour
	now := time.Now()
	add := func(id, name string, age time.Duration, pinned bool) {
		if err := r.s.parkStore().Add(park.Record{ID: id, Name: name, Kind: "claude", Host: "desk", PaneID: "w5:p1", Worktree: "/wt/" + name, ParkedAt: now.Add(-age), Pinned: pinned}); err != nil {
			t.Fatal(err)
		}
	}
	add("pk_old0000001", "old", 15*24*time.Hour, false)
	add("pk_young00001", "young", 3*24*time.Hour, false)
	add("pk_pinned0001", "kept", 40*24*time.Hour, true)
	if n := r.s.parkRetentionStep(now); n != 1 {
		t.Fatalf("proposed %d, want 1", n)
	}
	marks := r.s.ledger.Marks()
	if len(marks) != 1 || marks[0].Name != "old" || marks[0].Worktree != "/wt/old" || marks[0].AgentID != "desk/pk_old0000001" ||
		!strings.Contains(marks[0].Reason, "15 days") || marks[0].Ask != nil {
		t.Fatalf("marks = %+v", marks)
	}
	// Once: the user unmarks it in the review and it is not proposed again.
	if n := r.s.parkRetentionStep(now.Add(time.Hour)); n != 0 {
		t.Fatalf("proposed again: %d", n)
	}
	r.s.ledger.ClearMark("desk/pk_old0000001")
	if n := r.s.parkRetentionStep(now.Add(2 * time.Hour)); n != 0 || len(r.s.ledger.Marks()) != 0 {
		t.Fatalf("re-proposed after the user kept it: %d", n)
	}
	// The young one comes due later.
	if n := r.s.parkRetentionStep(now.Add(12 * 24 * time.Hour)); n != 1 {
		t.Fatalf("young one not proposed when due: %d", n)
	}
	// Off: retention 0 never proposes.
	r.s.cfg.ParkRetention = 0
	add("pk_old0000002", "older", 99*24*time.Hour, false)
	if n := r.s.parkRetentionStep(now); n != 0 {
		t.Fatalf("proposed with retention off: %d", n)
	}
	// The record of what was proposed survives a restart of the hub.
	if b, err := os.ReadFile(filepath.Join(r.s.cfg.StateDir, "parked-proposed.json")); err != nil || !strings.Contains(string(b), "pk_old0000001") {
		t.Fatalf("proposed file: %v %s", err, b)
	}
}

func TestParkRetentionMarksTheWorkersEntry(t *testing.T) {
	r, _ := newPQRig(t)
	r.s.cfg.ParkRetention = 14 * 24 * time.Hour
	e, _ := r.s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "feat/x", Worktree: "/wt/w", AgentID: "desk/w5:p1", PaneID: "w5:p1", AgentName: "w", Status: ledger.Finished})
	r.s.parkStore().Add(park.Record{ID: "pk_old0000001", Name: "w", Kind: "claude", Host: "desk", PaneID: "w5:p1", Worktree: "/wt/w", ParkedAt: time.Now().Add(-20 * 24 * time.Hour)})
	if n := r.s.parkRetentionStep(time.Now()); n != 1 {
		t.Fatalf("proposed %d", n)
	}
	got, _ := r.s.ledger.Get(e.ID)
	if got.Status != ledger.ReadyToRemove || got.Mark == nil || got.Mark.By != "hub (park retention)" {
		t.Fatalf("entry = %+v", got)
	}
	// The review lists it with the entry's own repository and branch.
	var item *removal
	for _, it := range r.s.marked() {
		if it.EntryID == e.ID {
			item = it
		}
	}
	if item == nil || item.Branch != "feat/x" || item.Worktree != "/wt/w" {
		t.Fatalf("review item = %+v", item)
	}
}

func TestRemovingAParkedWorktreeDropsItsRecordAndScratch(t *testing.T) {
	s, _, wt, _ := parkSetup(t)
	_, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1","name":"w"}`)
	rec := r["record"].(map[string]any)
	if rec["worktree_bytes"].(float64) <= 0 || rec["session_bytes"].(float64) <= 0 {
		t.Fatalf("disk cost not measured: %v", rec)
	}
	// A parked claude-style scratchpad on disk goes with the record.
	scratchDir := filepath.Join(s.cfg.StateDir, "parked-scratch", rec["id"].(string))
	os.MkdirAll(scratchDir, 0o755)
	os.WriteFile(filepath.Join(scratchDir, "f"), []byte("x"), 0o644)
	s.parkStore().Update(rec["id"].(string), func(x *park.Record) { x.ScratchTo = scratchDir })
	// The user confirms the removal in the review: the worktree is clean and merged, so it goes.
	code, rr := removeReq(t, s, `{"agent":"pk_gone","name":"w","worktree":"`+wt+`","remove_worktree":true}`)
	if code != 200 || rr["worktree_removed"] != true {
		t.Fatalf("remove = %d %v", code, rr)
	}
	if len(s.parkStore().List(false)) != 0 {
		t.Fatalf("record kept: %+v", s.parkStore().List(false))
	}
	if _, err := os.Stat(scratchDir); err == nil {
		t.Fatal("the parked scratchpad is still on disk")
	}
}

func TestLedgerFollowsAResumedWorker(t *testing.T) {
	r, _ := newPQRig(t)
	e, _ := r.s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "feat/x", Worktree: "/wt/w", AgentID: "desk/w5:p1", PaneID: "w5:p1", AgentName: "w", Status: ledger.Running})
	now := time.Now()
	r.s.parkStore().Add(park.Record{ID: "pk_w000000001", Name: "w", Kind: "claude", Host: "desk", PaneID: "w5:p1", Worktree: "/wt/w", ParkedAt: now.Add(-time.Hour)})
	live := []herdr.AgentInfo{cbAgent("w1:p1", "coordinator", herdr.StatusIdle)}
	r.s.rebindParked(live)
	got, _ := r.s.ledger.Get(e.ID)
	if len(got.ParkHistory) != 1 || got.ParkHistory[0].Event != "parked" || got.AgentID != "desk/w5:p1" {
		t.Fatalf("after park: %+v", got.ParkHistory)
	}
	r.s.rebindParked(live) // idempotent
	if got, _ = r.s.ledger.Get(e.ID); len(got.ParkHistory) != 1 {
		t.Fatalf("history repeated: %+v", got.ParkHistory)
	}
	// It resumes in a new pane: the entry follows it, so its work is observed again.
	r.s.parkStore().Update("pk_w000000001", func(x *park.Record) { x.ResumedAt, x.ResumedPane = &now, "w8:p1" })
	ag := cbAgent("w8:p1", "w", herdr.StatusWorking)
	ag.WorkspaceID = "w8"
	r.s.rebindParked(append(live, ag))
	got, _ = r.s.ledger.Get(e.ID)
	if got.AgentID != "desk/w8:p1" || got.PaneID != "w8:p1" || got.WorkspaceID != "w8" || len(got.ParkHistory) != 2 || got.ParkHistory[1].Event != "resumed" {
		t.Fatalf("after resume: %+v %s %s", got.ParkHistory, got.AgentID, got.PaneID)
	}
	r.s.ledger.Observe(append(live, ag), now)
	if got, _ = r.s.ledger.Get(e.ID); !got.SawWorking {
		t.Fatal("the resumed worker's work is not observed")
	}
}
