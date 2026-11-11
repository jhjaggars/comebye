package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/transcript"
)

func fromLastServer(t *testing.T, text string) *Server {
	t.Helper()
	s, _, _ := todoServer(t)
	s.latest = func(context.Context, string, string) *transcript.Latest {
		return &transcript.Latest{Text: text, Time: "2026-11-11T10:00:00Z"}
	}
	return s
}

type fromLastResp struct {
	Todos    []Todo `json:"todos"`
	Filed    int    `json:"filed"`
	Existing int    `json:"existing"`
	Found    int    `json:"found"`
}

func TestFromLastFilesOneItemPerQuestion(t *testing.T) {
	s := fromLastServer(t, provenanceText)
	w := do(s, "POST", "/api/todos/from-last", userHdr, `{"agent":"desk/w2:p1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got fromLastResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Found != 4 || got.Filed != 4 || got.Existing != 0 || len(got.Todos) != 4 {
		t.Fatalf("%+v", got)
	}
	first := got.Todos[0]
	if first.Suggested || first.Source != SourceQuestion || first.State != TodoOpen || first.Waiting == nil || first.Waiting.ID != "desk/w2:p1" ||
		len(first.Options) != 2 || len(first.Recommend) != 1 || !strings.Contains(first.Context, "sent to your To do by hand") {
		t.Fatalf("first = %+v", first)
	}
	// Pressing it again files nothing new.
	w = do(s, "POST", "/api/todos/from-last", userHdr, `{"agent":"desk/w2:p1"}`)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Filed != 0 || got.Existing != 4 || len(s.todos.list("")) != 4 {
		t.Fatalf("second press: %+v, %d items", got, len(s.todos.list("")))
	}
}

func TestFromLastNeedsAnAgentAndAMessage(t *testing.T) {
	s := fromLastServer(t, "All done, tests pass.")
	if w := do(s, "POST", "/api/todos/from-last", userHdr, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no agent: %d", w.Code)
	}
	if w := do(s, "POST", "/api/todos/from-last", userHdr, `{"agent":"desk/nope:p9"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", w.Code)
	}
	w := do(s, "POST", "/api/todos/from-last", userHdr, `{"agent":"desk/w2:p1"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"found":0`) || len(s.todos.list("")) != 0 {
		t.Fatalf("no questions: %d %s", w.Code, w.Body)
	}
	s.latest = func(context.Context, string, string) *transcript.Latest { return nil }
	if w := do(s, "POST", "/api/todos/from-last", userHdr, `{"agent":"desk/w2:p1"}`); w.Code != http.StatusNotFound {
		t.Fatalf("no message: %d", w.Code)
	}
}

func TestFromLastAgentsMaySendOnlyTheirOwn(t *testing.T) {
	s := fromLastServer(t, "Should I push it to main, or open a PR first?")
	// Itself, by default.
	w := do(s, "POST", "/api/todos/from-last", agentHdr("w2:p1"), `{}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"filed":1`) {
		t.Fatalf("self: %d %s", w.Code, w.Body)
	}
	it := s.todos.list("")[0]
	if it.CreatedByID != "desk/w2:p1" || it.Waiting.ID != "desk/w2:p1" {
		t.Fatalf("%+v", it)
	}
	// Someone else's: no.
	if w := do(s, "POST", "/api/todos/from-last", agentHdr("w2:p1"), `{"agent":"desk/w1:p1"}`); w.Code != http.StatusForbidden {
		t.Fatalf("other: %d %s", w.Code, w.Body)
	}
	// Even a person's item already waiting on the agent does not hold it back.
	s2 := fromLastServer(t, "Should I push it to main, or open a PR first?")
	if _, _, err := s2.todos.add(Todo{Question: "may I?", Kind: "decision", CreatedBy: "me", Waiting: &TodoAgent{ID: "desk/w2:p1", Instance: s2.hostInstance("desk")}}); err != nil {
		t.Fatal(err)
	}
	if w := do(s2, "POST", "/api/todos/from-last", userHdr, `{"agent":"desk/w2:p1"}`); !strings.Contains(w.Body.String(), `"filed":1`) {
		t.Fatalf("held back: %s", w.Body)
	}
}
