package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type historyResp struct {
	Briefs []Brief `json:"briefs"`
	Max    int     `json:"max"`
}

func getHistory(t *testing.T, s *Server, query string) historyResp {
	t.Helper()
	w := do(s, "GET", "/api/brief/history"+query, userHdr, "")
	if w.Code != 200 {
		t.Fatalf("history: %d %s", w.Code, w.Body)
	}
	var r historyResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return r
}

func TestBriefHistoryKeepsTheLastBriefings(t *testing.T) {
	s, _, dir := coordServer(t)
	if r := getHistory(t, s, ""); r.Briefs == nil || len(r.Briefs) != 0 {
		t.Fatalf("empty history = %+v", r)
	}
	for i := 1; i <= maxBriefHistory+5; i++ {
		body := fmt.Sprintf(`{"markdown":"briefing %d"}`, i)
		if w := do(s, "POST", "/api/brief", userHdr, body); w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	r := getHistory(t, s, "")
	if len(r.Briefs) != maxBriefHistory || r.Max != maxBriefHistory {
		t.Fatalf("kept %d (max %d), want %d", len(r.Briefs), r.Max, maxBriefHistory)
	}
	if first, last := r.Briefs[0].Markdown, r.Briefs[len(r.Briefs)-1].Markdown; first != fmt.Sprintf("briefing %d", maxBriefHistory+5) || last != "briefing 6" {
		t.Fatalf("order: first %q last %q", first, last)
	}
	if r := getHistory(t, s, "?limit=3"); len(r.Briefs) != 3 || r.Briefs[0].Markdown != fmt.Sprintf("briefing %d", maxBriefHistory+5) {
		t.Fatalf("limit=3 -> %+v", r)
	}
	// The current briefing is still the latest one, and the file is bounded.
	if b, err := s.brief.get(); err != nil || b.Markdown != fmt.Sprintf("briefing %d", maxBriefHistory+5) {
		t.Fatalf("current = %+v, %v", b, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "brief-history.json")); err != nil || st.Size() > 3<<20 {
		t.Fatalf("history file: %v %v", st, err)
	}
}

func TestBriefHistoryIsForTheUser(t *testing.T) {
	s, _, _ := coordServer(t)
	if w := do(s, "GET", "/api/brief/history", map[string]string{}, ""); w.Code == 200 {
		t.Fatalf("anonymous got %d", w.Code)
	}
}
