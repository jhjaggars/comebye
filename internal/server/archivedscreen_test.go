package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The calls the app's Archived screen (#/archived, issue #86 slice 6) makes:
// the list needs the reader role (the user or an agent, never an anonymous
// caller), Unarchive and Resume need the user or a coordinator, and Propose
// removal is a mark, which takes the entry out of the archive into the review.
func TestArchivedScreenAPI(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	for _, n := range []string{"dirty", "pushed"} {
		e, _ := s.ledger.ByAgent(f.ids[n])
		if _, err := s.ledger.Archive(e.ID, ledger.Archival{By: "hub", Why: "7 days with no progress", BuildFreed: 1 << 20, BuildDirs: []string{"target"}, WorktreeBytes: 4096, Unmerged: 1,
			Left: []ledger.AskItem{{Type: "followup", Text: "run tf-apply"}}}); err != nil {
			t.Fatal(err)
		}
	}

	if w := do(s, "GET", "/api/archived", nil, ""); w.Code != 401 && w.Code != 403 {
		t.Fatalf("anonymous list = %d %s", w.Code, w.Body.String())
	}
	for who, h := range map[string]map[string]string{"user": userHdr, "agent": agentHdr("w3:p1")} {
		w := do(s, "GET", "/api/archived", h, "")
		var v archivedView
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("%s list = %d %s", who, w.Code, w.Body.String())
		}
		if v.Count != 2 || v.BuildFreed != 2<<20 || v.WorktreeBytes != 8192 {
			t.Fatalf("%s totals = %+v", who, v)
		}
		r := v.Archived[0]
		if r.AgentID == "" || r.Project != "proj" || r.Archive.Why == "" || len(r.Archive.BuildDirs) != 1 || len(r.Archive.Left) != 1 || r.Archive.Left[0].Type != "followup" {
			t.Fatalf("%s row = %+v", who, r)
		}
	}
	if w := do(s, "GET", "/api/board", userHdr, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"archived":2`) {
		t.Fatalf("board loose = %d %s", w.Code, w.Body.String())
	}

	// Unarchive: not a plain agent; a coordinator may.
	if w := do(s, "POST", "/api/archived/restore", agentHdr("w1:p1"), `{"ref":"dirty"}`); w.Code != 403 {
		t.Fatalf("restore by a plain agent = %d %s", w.Code, w.Body.String())
	}
	if w := do(s, "POST", "/api/archived/restore", agentHdr("w5:p1"), `{"ref":"dirty"}`); w.Code != 200 || statusOf(s, f.ids["dirty"]) != ledger.Finished {
		t.Fatalf("restore by the coordinator = %d %s (%s)", w.Code, w.Body.String(), statusOf(s, f.ids["dirty"]))
	}

	// Propose removal: the mark takes it out of the archive into the review.
	body := `{"agent_id":"` + f.ids["pushed"] + `","reason":"Proposed from the Archived screen","no_ask":true}`
	if w := do(s, "POST", "/api/marks", userHdr, body); w.Code != 200 || statusOf(s, f.ids["pushed"]) != ledger.ReadyToRemove {
		t.Fatalf("propose removal = %d %s (%s)", w.Code, w.Body.String(), statusOf(s, f.ids["pushed"]))
	}
	w := do(s, "GET", "/api/archived", userHdr, "")
	if !strings.Contains(w.Body.String(), `"count":0`) {
		t.Fatalf("after both = %s", w.Body.String())
	}
}
