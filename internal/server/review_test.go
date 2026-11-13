package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// reviewServer is todoServer with a Page published by the worker w2:p1.
func reviewServer(t *testing.T, publisher string) (*Server, Page, *[]string, *sync.Mutex) {
	t.Helper()
	s, sent, mu := todoServer(t)
	if s.pages == nil {
		t.Skip("pages off")
	}
	who := pageWho{by: "home", agent: publisher, host: "desk"}
	if publisher == "" {
		who = pageWho{by: "me", user: true}
	}
	p, _, err := s.pages.publish(pageIn{Title: "Slice 1 hub", Kind: "md", Body: []byte("# hi")}, who)
	if err != nil {
		t.Fatal(err)
	}
	return s, p, sent, mu
}

func addReview(t *testing.T, s *Server, p Page, extra string) Todo {
	t.Helper()
	w := do(s, "POST", "/api/todos", agentHdr("w1:p1"), `{"question":"Review slice 1","kind":"review","context":"try it","page":{"id":"`+p.ID+`"}`+extra+`}`)
	if w.Code != 201 {
		t.Fatalf("add review = %d %s", w.Code, w.Body)
	}
	return todoOf(t, w.Body.Bytes())
}

func TestReviewAddNeedsPage(t *testing.T) {
	s, p, _, _ := reviewServer(t, "desk/w2:p1")
	for _, body := range []string{
		`{"question":"q","kind":"review"}`,
		`{"question":"q","kind":"review","page":{"id":"pg_0000000000"}}`,
		`{"question":"q","kind":"review","page":{"id":"` + p.ID + `","version":9}}`,
		`{"question":"q","kind":"decision","page":{"id":"nope"}}`,
	} {
		if w := do(s, "POST", "/api/todos", userHdr, body); w.Code != 400 {
			t.Errorf("%s = %d %s", body, w.Code, w.Body)
		}
	}
	it := addReview(t, s, p, "")
	if it.Page == nil || it.Page.ID != p.ID || it.Page.Version != 1 || it.Page.Title != "Slice 1 hub" || it.Page.Agent != "desk/w2:p1" {
		t.Fatalf("page ref = %+v", it.Page)
	}
}

func TestPageRefOnEveryKindAndSets(t *testing.T) {
	s, p, _, _ := reviewServer(t, "desk/w2:p1")
	for _, k := range []string{"decision", "action", "followup"} {
		w := do(s, "POST", "/api/todos", userHdr, `{"question":"about `+k+`","kind":"`+k+`","page":{"id":"`+p.ID+`","version":1}}`)
		if it := todoOf(t, w.Body.Bytes()); w.Code != 201 || it.Page == nil || it.Page.URL != "/#/pages/"+p.ID {
			t.Fatalf("%s: %d %s", k, w.Code, w.Body)
		}
	}
	set := `{"question":"plan","page":{"id":"` + p.ID + `"},"set":{"decisions":[{"id":"a","title":"A","options":[{"label":"x"},{"label":"y"}]}]}}`
	w := do(s, "POST", "/api/todos", userHdr, set)
	if it := todoOf(t, w.Body.Bytes()); w.Code != 201 || it.Page == nil || it.Set == nil {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
}

func TestReviewThumbsExposed(t *testing.T) {
	s, p, _, _ := reviewServer(t, "desk/w2:p1")
	it := addReview(t, s, p, "")
	if it.Page.ThumbURL != "" {
		t.Fatalf("no thumbs yet: %+v", it.Page)
	}
	s.pages.mu.Lock()
	s.pages.data.Pages[0].History[0].Thumbs = []string{"light", "dark"}
	s.pages.mu.Unlock()
	w := do(s, "GET", "/api/todos/"+it.ID, userHdr, "")
	got := todoOf(t, w.Body.Bytes())
	if len(got.Page.Thumbs) != 2 || !strings.Contains(got.Page.ThumbURL, "/api/pages/"+p.ID+"/thumb?v=1&scheme={scheme}") {
		t.Fatalf("thumbs = %+v", got.Page)
	}
}

func TestReviewLooksGoodClosesAndTellsPublisherAfterUndoWindow(t *testing.T) {
	s, p, sent, mu := reviewServer(t, "desk/w2:p1")
	it := addReview(t, s, p, "")
	w := do(s, "POST", "/api/todos/"+it.ID+"/verdict", userHdr, `{"verdict":"good","note":"nice"}`)
	if w.Code != 200 {
		t.Fatalf("verdict = %d %s", w.Code, w.Body)
	}
	got := todoOf(t, w.Body.Bytes())
	if got.State != TodoDone || got.Review.Verdict.Verdict != "good" || !got.Review.CanUndo || got.Answer == nil || got.Answer.Via != "verdict" {
		t.Fatalf("closed = %+v %+v", got, got.Review)
	}
	// Inside the undo window nothing is sent.
	s.deliverReviewRoutes(s.fleet.Agents())
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := len(*sent)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("sent inside the undo window: %v", *sent)
	}
	// Past it, the publisher hears, once.
	s.todos.mutate(it.ID, func(t *Todo) error {
		t.Review.Route.Due = time.Now().Add(-time.Second)
		return nil
	})
	s.deliverReviewRoutes(s.fleet.Agents())
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(*sent) == 1 })
	mu.Lock()
	if !strings.HasPrefix((*sent)[0], "desk/w2:p1: ") || !strings.Contains((*sent)[0], "Looks good") || !strings.Contains((*sent)[0], "nice") {
		t.Fatalf("sent = %v", *sent)
	}
	mu.Unlock()
	waitFor(t, func() bool { g, _ := s.todos.get(it.ID); return g.Review.Route.State == "sent" })
	g, _ := s.todos.get(it.ID)
	if g.Review.Route.Via != "publisher" {
		t.Fatalf("route = %+v", g.Review.Route)
	}
	s.deliverReviewRoutes(s.fleet.Agents())
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*sent) != 1 {
		t.Fatalf("sent twice: %v", *sent)
	}
}

func TestReviewUndo(t *testing.T) {
	s, p, sent, mu := reviewServer(t, "desk/w2:p1")
	it := addReview(t, s, p, "")
	do(s, "POST", "/api/todos/"+it.ID+"/verdict", userHdr, `{"verdict":"good"}`)
	w := do(s, "POST", "/api/todos/"+it.ID+"/undo", userHdr, `{}`)
	if got := todoOf(t, w.Body.Bytes()); w.Code != 200 || got.State != TodoOpen || got.Review.Verdict != nil || got.Answer != nil {
		t.Fatalf("undo = %d %s", w.Code, w.Body)
	}
	s.deliverReviewRoutes(s.fleet.Agents())
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(*sent) != 0 {
		t.Fatalf("sent after undo: %v", *sent)
	}
	mu.Unlock()
	// After the window it cannot be undone.
	do(s, "POST", "/api/todos/"+it.ID+"/verdict", userHdr, `{"verdict":"good"}`)
	s.todos.mutate(it.ID, func(t *Todo) error {
		past := time.Now().Add(-time.Second)
		t.Review.Verdict.UndoUntil = &past
		return nil
	})
	if w := do(s, "POST", "/api/todos/"+it.ID+"/undo", userHdr, `{}`); w.Code != 409 {
		t.Fatalf("late undo = %d %s", w.Code, w.Body)
	}
}

func TestReviewNeedsChangesFilesLinkedFix(t *testing.T) {
	s, p, sent, mu := reviewServer(t, "desk/w2:p1")
	it := addReview(t, s, p, "")
	w := do(s, "POST", "/api/todos/"+it.ID+"/verdict", userHdr, `{"verdict":"changes","note":"the card is cut off at 390px","kind":"fix"}`)
	got := todoOf(t, w.Body.Bytes())
	if w.Code != 200 || got.State != TodoDone || got.Review.FixID == "" || got.Review.Verdict.Verdict != "changes" {
		t.Fatalf("changes = %d %s", w.Code, w.Body)
	}
	fix, ok := s.todos.get(got.Review.FixID)
	if !ok || fix.ReviewOf != it.ID || fix.Kind != "action" || fix.State != TodoOpen || fix.Waiting != nil ||
		fix.Page == nil || fix.Page.ID != p.ID || !strings.Contains(fix.Question, "Fix:") || !strings.Contains(fix.Context, "cut off") {
		t.Fatalf("fix = %+v", fix)
	}
	// An idea for later is a followup.
	it2 := addReview(t, s, p, "")
	w = do(s, "POST", "/api/todos/"+it2.ID+"/verdict", userHdr, `{"verdict":"changes","kind":"idea","note":"dark thumbs"}`)
	f2, _ := s.todos.get(todoOf(t, w.Body.Bytes()).Review.FixID)
	if f2.Kind != "followup" || !strings.HasPrefix(f2.Question, "Idea for later:") {
		t.Fatalf("idea = %+v", f2)
	}
	// The verdict is told right away, with the Fix id.
	s.deliverReviewRoutes(s.fleet.Agents())
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(*sent) >= 1 })
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains((*sent)[0], got.Review.FixID) || !strings.Contains((*sent)[0], "Needs changes") {
		t.Fatalf("sent = %v", *sent)
	}
	// A closed review takes no second verdict.
	if w := do(s, "POST", "/api/todos/"+it.ID+"/verdict", userHdr, `{"verdict":"good"}`); w.Code != 409 {
		t.Fatalf("second verdict = %d", w.Code)
	}
}

func TestReviewRoutesToCoordinatorWhenPublisherGone(t *testing.T) {
	// Published by the user: no publisher pane at all.
	s, p, sent, mu := reviewServer(t, "")
	it := addReview(t, s, p, "")
	do(s, "POST", "/api/todos/"+it.ID+"/verdict", userHdr, `{"verdict":"changes"}`)
	s.deliverReviewRoutes(s.fleet.Agents())
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(*sent) == 1 })
	mu.Lock()
	if !strings.HasPrefix((*sent)[0], "desk/w1:p1: ") {
		t.Fatalf("sent = %v", *sent)
	}
	mu.Unlock()
	// Published by a pane that is no longer in the fleet.
	s2, p2, sent2, mu2 := reviewServer(t, "desk/w9:p9")
	it2 := addReview(t, s2, p2, "")
	do(s2, "POST", "/api/todos/"+it2.ID+"/verdict", userHdr, `{"verdict":"changes"}`)
	s2.deliverReviewRoutes(s2.fleet.Agents())
	waitFor(t, func() bool { mu2.Lock(); defer mu2.Unlock(); return len(*sent2) == 1 })
	mu2.Lock()
	defer mu2.Unlock()
	if !strings.HasPrefix((*sent2)[0], "desk/w1:p1: ") {
		t.Fatalf("sent = %v", *sent2)
	}
}

func TestReviewStaleNeverClosed(t *testing.T) {
	s, p, _, _ := reviewServer(t, "desk/w2:p1")
	it := addReview(t, s, p, "")
	get := func() Todo {
		return todoOf(t, do(s, "GET", "/api/todos/"+it.ID, userHdr, "").Body.Bytes())
	}
	if g := get(); g.Review != nil && g.Review.Stale {
		t.Fatal("fresh review is stale")
	}
	s.todos.mutate(it.ID, func(t *Todo) error { t.CreatedAt = time.Now().Add(-4 * 24 * time.Hour); return nil })
	g := get()
	if g.Review == nil || !g.Review.Stale || g.State != TodoOpen {
		t.Fatalf("stale = %+v %s", g.Review, g.State)
	}
	// Listing agrees, and the sweep leaves it open.
	s.deliverReviewRoutes(s.fleet.Agents())
	var l struct{ Todos []Todo }
	json.Unmarshal(do(s, "GET", "/api/todos", userHdr, "").Body.Bytes(), &l)
	if len(l.Todos) != 1 || !l.Todos[0].Review.Stale || l.Todos[0].State != TodoOpen {
		t.Fatalf("list = %+v", l.Todos)
	}
}

func TestReviewVerdictValidationAndRoles(t *testing.T) {
	s, p, _, _ := reviewServer(t, "desk/w2:p1")
	it := addReview(t, s, p, "")
	for _, body := range []string{`{"verdict":"meh"}`, `{"verdict":"changes","kind":"nope"}`} {
		if w := do(s, "POST", "/api/todos/"+it.ID+"/verdict", userHdr, body); w.Code != 400 {
			t.Errorf("%s = %d", body, w.Code)
		}
	}
	// A plain worker may not give a verdict.
	if w := do(s, "POST", "/api/todos/"+it.ID+"/verdict", agentHdr("w2:p1"), `{"verdict":"good"}`); w.Code != 403 {
		t.Errorf("worker verdict = %d", w.Code)
	}
	// Not a review.
	d := todoOf(t, do(s, "POST", "/api/todos", userHdr, `{"question":"q"}`).Body.Bytes())
	if w := do(s, "POST", "/api/todos/"+d.ID+"/verdict", userHdr, `{"verdict":"good"}`); w.Code != 400 {
		t.Errorf("non-review verdict = %d", w.Code)
	}
	if w := do(s, "POST", "/api/todos/tnope/verdict", userHdr, `{"verdict":"good"}`); w.Code != 404 {
		t.Errorf("missing = %d", w.Code)
	}
}
