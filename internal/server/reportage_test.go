package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// The finished notice says how old the report it points at is.
func TestFinishedNoticeSaysReportAge(t *testing.T) {
	s, fh, _ := coordServer(t)
	idle := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, idle)
	s.deliverCallbacks([]herdr.AgentInfo{idle})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
	text, _ := fh.call("agent.prompt").Params["text"].(string)
	if !strings.Contains(text, " Report age: the one on file is report (its last reply, taken by the hub), written ") || !strings.Contains(text, ", just before this notice.") {
		t.Fatalf("notice = %q", text)
	}
}

// The age sentence names the report's number, when it was written and a
// newer reply. It is measured from when the notice was queued, so one
// notice's text (and the hash that finds a late earlier attempt) is the
// same on every retry; and it is not part of the notice's key.
func TestReportAgeNoteAndKey(t *testing.T) {
	queued := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	rep := queued.Add(-3 * time.Hour)
	pass := queued.Add(-10 * time.Minute)
	e := ledger.Entry{ID: "h123", Branch: "feat/x", RepoName: "proj", Host: "desk", Status: ledger.Finished,
		FinalReport: "## Summary\nx", ReportSource: "explicit", ReportSeq: 3, ReportAt: &rep,
		LastPassReport: "later", LastPassReportAt: &pass, Callback: &ledger.Callback{State: ledger.CallbackPending, At: queued}}
	got := reportAgeNote(e, queued.Add(5*time.Minute))
	want := " Report age: the one on file is report 3, written 2026-11-23 09:00 UTC, 3 h before this notice; the worker's last reply, written 2026-11-23 11:50 UTC, is newer and is not a report (`comebye read h123 --report` shows both)."
	if got != want {
		t.Fatalf("note =\n%q\nwant\n%q", got, want)
	}
	if later := reportAgeNote(e, queued.Add(3*time.Hour)); later != got {
		t.Fatalf("a retry changed the notice: %q", later)
	}
	if a, b := callbackText(e, false), callbackText(e, false); a != b {
		t.Fatal("notice text not stable")
	}
	older := e
	r2 := queued.Add(-30 * time.Hour)
	older.ReportAt, older.LastPassReport = &r2, ""
	if n := reportAgeNote(older, queued); n != " Report age: the one on file is report 3, written 2026-11-22 06:00 UTC, 30 h before this notice." {
		t.Fatalf("note = %q", n)
	}
	k1 := callbackNoticeKey("a", e.FinalReport, withoutReportAge(callbackText(e, false)))
	k2 := callbackNoticeKey("a", e.FinalReport, withoutReportAge(callbackText(older, false)))
	if k1 != k2 {
		t.Fatal("the report's age changed the notice key")
	}
	unknown := e
	unknown.ReportAt, unknown.LastPassReport = nil, ""
	if n := reportAgeNote(unknown, queued); n != " Report age: the one on file is report 3, from an unrecorded time (before report times were kept)." {
		t.Fatalf("note = %q", n)
	}
	if n := reportAgeNote(ledger.Entry{}, queued); n != "" {
		t.Fatalf("no report: %q", n)
	}
	// The API's age is as of now.
	if a := reportAge(e, queued.Add(time.Hour)); a != "report 3 from 4 h ago" {
		t.Fatalf("reportAge = %q", a)
	}
}

// A loop worker's pass end: the hub captures its last reply beside the
// report, and the ledger API shows both ages.
func TestPassReplyCapturedAndShown(t *testing.T) {
	s, _, _ := coordServer(t)
	s.latest = func(context.Context, string, string) *transcript.Latest {
		return &transcript.Latest{Text: "pass 28: nothing new"}
	}
	e, err := s.ledger.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "loop/steward", AgentID: "desk/w9:p1", PaneID: "w9:p1",
		Status: ledger.Running, Loop: "steward pass"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ledger.Report("desk/w9:p1", "## Summary\npass 27", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.observeLedger([]herdr.AgentInfo{cbAgent("w9:p1", "steward", herdr.StatusWorking)})
	s.observeLedger([]herdr.AgentInfo{cbAgent("w9:p1", "steward", herdr.StatusIdle)})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.LastPassReport != "" })
	g, _ := s.ledger.Get(e.ID)
	if g.Status != ledger.Running || g.FinalReport != "## Summary\npass 27" || g.LastPassReport != "pass 28: nothing new" {
		t.Fatalf("%+v", g)
	}
	w := do(s, "GET", "/api/ledger", userHdr, "")
	var out struct {
		Entries []struct {
			ID            string `json:"id"`
			ReportAge     string `json:"report_age"`
			NewerReplyAge string `json:"newer_reply_age"`
			ReportAt      string `json:"report_at"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, v := range out.Entries {
		if v.ID == e.ID {
			if v.ReportAge != "report from 2 h ago" || v.NewerReplyAge != "just now" || v.ReportAt == "" {
				t.Fatalf("view = %+v", v)
			}
			return
		}
	}
	t.Fatal("entry not in the ledger")
}
