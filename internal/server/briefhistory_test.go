package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	// Spaced past the merge window, so each one is its own entry.
	t0 := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	for i := 1; i <= maxBriefHistory+5; i++ {
		if err := s.brief.set(Brief{Markdown: fmt.Sprintf("briefing %d", i), By: "c", At: t0.Add(time.Duration(i) * 20 * time.Minute)}); err != nil {
			t.Fatal(err)
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

const needsA = "## Needs you\n- t0123456789: decide A\n\n## Work\n"
const needsB = "## Needs you\n- t0123456789: decide A\n- tabcdef0123: decide B\n\n## Work\n"

func TestBriefHistoryMergesBriefingsWithinTheWindow(t *testing.T) {
	s, _, _ := coordServer(t)
	t0 := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	pub := func(md string, min int) {
		t.Helper()
		if err := s.brief.set(Brief{Markdown: md, By: "c", At: t0.Add(time.Duration(min) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	pub(needsA+"one", 0)
	pub(needsA+"two", 3)   // merged into the entry from 0
	pub(needsA+"three", 9) // merged
	pub(needsA+"four", 14) // still inside 15 min of the first
	h := getHistory(t, s, "").Briefs
	if len(h) != 1 || h[0].Markdown != needsA+"four" || !h[0].At.Equal(t0.Add(14*time.Minute)) || !h[0].Since.Equal(t0) {
		t.Fatalf("after 4 close publishes: %+v", h)
	}
	// The window runs from the first publish, so a steady stream still
	// yields an entry about every 15 minutes (not one forever).
	pub(needsA+"five", 16)
	h = getHistory(t, s, "").Briefs
	if len(h) != 2 || h[0].Markdown != needsA+"five" || h[1].Markdown != needsA+"four" {
		t.Fatalf("after the window: %+v", h)
	}
	// A change in "Needs you" is never merged, even a minute later.
	pub(needsB+"six", 17)
	h = getHistory(t, s, "").Briefs
	if len(h) != 3 || h[0].Markdown != needsB+"six" || h[1].Markdown != needsA+"five" {
		t.Fatalf("after a Needs you change: %+v", h)
	}
	// The current briefing is always the latest publish.
	if b, _ := s.brief.get(); b == nil || b.Markdown != needsB+"six" {
		t.Fatalf("current = %+v", b)
	}
}

func TestBriefHistoryMergeWithoutNeedsSection(t *testing.T) {
	s, _, _ := coordServer(t)
	t0 := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	s.brief.set(Brief{Markdown: "plain one", At: t0})
	s.brief.set(Brief{Markdown: "plain two", At: t0.Add(time.Minute)})
	// Gaining a Needs you section is a change, so it gets its own entry.
	s.brief.set(Brief{Markdown: needsA, At: t0.Add(2 * time.Minute)})
	// A clock that went backwards does not merge.
	s.brief.set(Brief{Markdown: needsA + "x", At: t0.Add(-time.Hour)})
	h := getHistory(t, s, "").Briefs
	if len(h) != 3 || h[2].Markdown != "plain two" {
		t.Fatalf("history = %+v", h)
	}
}
