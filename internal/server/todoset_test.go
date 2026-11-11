package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const setBody = `{"question":"How do we add the backend?","kind":"decision","set":{
 "decided":["S3 protocol"],
 "risks":[{"id":"R1","text":"data lost","decisions":["D3"]}],
 "decisions":[
  {"id":"D1","title":"Layout","options":[{"label":"One bucket"},{"label":"Bucket each"}],"recommend":1,"release_early":true},
  {"id":"D3","title":"Migration","one_way":true,"options":[{"label":"Dual-write"},{"label":"Big-bang"}],"recommend":1},
  {"id":"D6","title":"Rollout","options":[{"label":"Per-tenant flag"},{"label":"All at once"}],"recommend":1}],
 "facts":[{"id":"F1","text":"strong reads?","test":"loop on staging","affects":["D1"]}],
 "rules":[
  {"id":"r1","level":"block","when":{"D3":2,"D6":1},"say":"big-bang cannot be flagged","fix":{"decision":"D3","option":1}},
  {"id":"r2","level":"warn","when":{"D1":2,"D6":2},"say":"all at once with a bucket each is heavy"}]}}`

func setAdd(t *testing.T, s *Server, body string) Todo {
	t.Helper()
	w := do(s, "POST", "/api/todos", agentHdr("w2:p1"), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	return todoOf(t, w.Body.Bytes())
}

func putDraft(s *Server, id, body string) (int, map[string]any) {
	w := do(s, "PUT", "/api/todos/"+id+"/draft", userHdr, body)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func TestSetAddCleansAndRefuses(t *testing.T) {
	s, _, _ := todoServer(t)
	it := setAdd(t, s, setBody)
	if it.Set == nil || len(it.Set.Decisions) != 3 || it.Set.Decisions[0].Options[0].Short != "One bucket" {
		t.Fatalf("set = %+v", it.Set)
	}
	if !strings.Contains(it.Context, "D3 Migration") || !strings.Contains(it.Context, "one-way door") || !strings.Contains(it.Context, "Cheapest test") {
		t.Fatalf("context = %q", it.Context)
	}
	for name, mut := range map[string]string{
		"dup id":         strings.Replace(setBody, `"id":"D6"`, `"id":"D3"`, 1),
		"bad rule opt":   strings.Replace(setBody, `"D3":2,"D6":1`, `"D3":5,"D6":1`, 1),
		"one-sided rule": strings.Replace(setBody, `"D3":2,"D6":1`, `"D3":2`, 1),
		"bad level":      strings.Replace(setBody, `"level":"block"`, `"level":"stop"`, 1),
		"bad recommend":  strings.Replace(setBody, `"recommend":1,"release_early"`, `"recommend":3,"release_early"`, 1),
		"risk ghost":     strings.Replace(setBody, `"decisions":["D3"]`, `"decisions":["D9"]`, 1),
		"with options":   strings.Replace(setBody, `"kind":"decision",`, `"kind":"decision","options":[{"label":"a"},{"label":"b"}],`, 1),
		"not a decision": strings.Replace(setBody, `"kind":"decision"`, `"kind":"action"`, 1),
	} {
		if w := do(s, "POST", "/api/todos", agentHdr("w2:p1"), mut); w.Code != 400 {
			t.Errorf("%s = %d %s", name, w.Code, w.Body)
		}
	}
}

func TestSetDraftAcceptAllHoldsBackOneWayDoors(t *testing.T) {
	s, _, _ := todoServer(t)
	it := setAdd(t, s, setBody)
	code, m := putDraft(s, it.ID, `{"accept_suggestions":true}`)
	if code != 200 {
		t.Fatalf("draft = %d %v", code, m)
	}
	g, _ := s.todos.get(it.ID)
	ch := g.Set.Draft.Choices
	if !ch["D1"].Confirmed || !ch["D6"].Confirmed || ch["D3"].Option != 1 || ch["D3"].Confirmed {
		t.Fatalf("choices = %+v", ch)
	}
	// Submit refuses until the one-way door is confirmed.
	w := do(s, "POST", "/api/todos/"+it.ID+"/submit", userHdr, ``)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "set_incomplete") || !strings.Contains(w.Body.String(), "D3") {
		t.Fatalf("submit early = %d %s", w.Code, w.Body)
	}
	if code, _ := putDraft(s, it.ID, `{"choices":{"D3":{"option":1,"confirmed":true}}}`); code != 200 {
		t.Fatal(code)
	}
	// Earlier choices stay when the body names only D3? No: choices replace.
	g, _ = s.todos.get(it.ID)
	if g.Set.Draft.Choices["D1"].Option != 0 {
		t.Fatalf("a body with choices replaces the draft: %+v", g.Set.Draft.Choices)
	}
}

func TestSetRulesBlockAndWarn(t *testing.T) {
	s, _, _ := todoServer(t)
	it := setAdd(t, s, setBody)
	full := func(d3, d1, d6 int) string {
		b, _ := json.Marshal(map[string]any{"choices": map[string]SetChoice{"D1": {d1, true}, "D3": {d3, true}, "D6": {d6, true}}})
		return string(b)
	}
	_, m := putDraft(s, it.ID, full(2, 1, 1))
	cs, _ := m["conflicts"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["level"] != "block" {
		t.Fatalf("conflicts = %v", m["conflicts"])
	}
	w := do(s, "POST", "/api/todos/"+it.ID+"/submit", userHdr, ``)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "set_blocked") {
		t.Fatalf("blocked submit = %d %s", w.Code, w.Body)
	}
	// A warning goes along and is recorded.
	putDraft(s, it.ID, full(1, 2, 2))
	w = do(s, "POST", "/api/todos/"+it.ID+"/submit", userHdr, ``)
	got := todoOf(t, w.Body.Bytes())
	if w.Code != 200 || got.State != TodoAnswered || got.Set.Result == nil || len(got.Set.Result.Warnings) != 1 || got.Set.Result.Warnings[0].Rule != "r2" {
		t.Fatalf("submit = %d %s", w.Code, w.Body)
	}
	if r := got.Set.Result.Decisions["D1"]; r.Option != 2 || r.Suggested || r.Label != "Bucket each" {
		t.Fatalf("result D1 = %+v", r)
	}
	if !strings.Contains(got.Answer.Text, "overrides the suggestion") || !strings.Contains(got.Answer.Text, "Warnings recorded") || got.Answer.Via != "app" {
		t.Fatalf("answer = %+v", got.Answer)
	}
	if got.Set.Result.Facts["F1"] != "unknown" {
		t.Fatalf("facts = %v", got.Set.Result.Facts)
	}
	// Submitted: no more drafts, no second submit.
	if code, _ := putDraft(s, it.ID, `{"accept_suggestions":true}`); code != 409 {
		t.Fatalf("draft after submit = %d", code)
	}
	if w := do(s, "POST", "/api/todos/"+it.ID+"/submit", userHdr, ``); w.Code != 409 {
		t.Fatalf("resubmit = %d", w.Code)
	}
}

func TestSetDraftValidatesAndGuardsRevision(t *testing.T) {
	s, _, _ := todoServer(t)
	it := setAdd(t, s, setBody)
	for _, b := range []string{`{"choices":{"D9":{"option":1}}}`, `{"choices":{"D1":{"option":3}}}`, `{"facts":{"F9":true}}`} {
		if code, _ := putDraft(s, it.ID, b); code != 400 {
			t.Errorf("%s = %d", b, code)
		}
	}
	if code, _ := putDraft(s, it.ID, `{"rev":0,"facts":{"F1":true}}`); code != 200 {
		t.Fatal("rev 0")
	}
	code, m := putDraft(s, it.ID, `{"rev":0,"facts":{"F1":false}}`)
	if code != 409 || m["code"] != "draft_changed" || m["rev"].(float64) != 1 {
		t.Fatalf("stale = %d %v", code, m)
	}
	// Facts, unlike choices, are kept when the body leaves them out.
	putDraft(s, it.ID, `{"choices":{"D1":{"option":2,"confirmed":true}}}`)
	g, _ := s.todos.get(it.ID)
	if !g.Set.Draft.Facts["F1"] || g.Set.Draft.Rev != 2 {
		t.Fatalf("draft = %+v", g.Set.Draft)
	}
	// An agent that is not a coordinator may not answer; a plain item is no set.
	if w := do(s, "PUT", "/api/todos/"+it.ID+"/draft", agentHdr("w2:p1"), `{}`); w.Code != 403 {
		t.Fatalf("agent draft = %d", w.Code)
	}
	plain := todoOf(t, do(s, "POST", "/api/todos", agentHdr("w2:p1"), `{"question":"plain"}`).Body.Bytes())
	if code, m := putDraft(s, plain.ID, `{}`); code != 400 || m["code"] != "not_a_set" {
		t.Fatalf("plain = %d %v", code, m)
	}
}

func TestSetEarlyReleaseNoticesOnceThenSubmitNoticesAnswered(t *testing.T) {
	s, sent, mu := todoServer(t)
	it := setAdd(t, s, setBody)
	got := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), *sent...) }
	// D1 is unconfirmed: nothing goes out.
	putDraft(s, it.ID, `{"choices":{"D1":{"option":1}}}`)
	time.Sleep(60 * time.Millisecond)
	if n := got(); len(n) != 0 {
		t.Fatalf("unconfirmed sent %v", n)
	}
	putDraft(s, it.ID, `{"choices":{"D1":{"option":1,"confirmed":true}}}`)
	s.deliverTodoNotices(s.fleet.Agents())
	time.Sleep(80 * time.Millisecond)
	n := got()
	if len(n) != 1 || !strings.Contains(n[0], "settled decision(s) D1") || !strings.Contains(n[0], it.ID) || !strings.Contains(n[0], "leave the item open") {
		t.Fatalf("early notice = %v", n)
	}
	g, _ := s.todos.get(it.ID)
	if g.State != TodoOpen || g.Notice.State != todoNoticeSent || len(g.Set.EarlyPending) != 0 || len(g.Set.Released) != 1 {
		t.Fatalf("after early = %+v %+v", g.Set, g.Notice)
	}
	// Re-saving the same confirmation does not notice again.
	putDraft(s, it.ID, `{"choices":{"D1":{"option":1,"confirmed":true},"D3":{"option":1,"confirmed":true},"D6":{"option":1,"confirmed":true}}}`)
	s.deliverTodoNotices(s.fleet.Agents())
	time.Sleep(80 * time.Millisecond)
	if n := got(); len(n) != 1 {
		t.Fatalf("second notice = %v", n)
	}
	// Submit: the answered notice, as for any item (the pacing guard reset).
	s.todos.mu.Lock()
	s.todos.last = time.Time{}
	s.todos.mu.Unlock()
	if w := do(s, "POST", "/api/todos/"+it.ID+"/submit", userHdr, ``); w.Code != 200 {
		t.Fatalf("submit = %d %s", w.Code, w.Body)
	}
	time.Sleep(80 * time.Millisecond)
	n = got()
	if len(n) != 2 || !strings.Contains(n[1], "answered to-do item(s) "+it.ID) {
		t.Fatalf("submit notice = %v", n)
	}
	// The result marks D1 as released early.
	g, _ = s.todos.get(it.ID)
	if !g.Set.Result.Decisions["D1"].Early || g.Set.Result.Decisions["D3"].Early {
		t.Fatalf("result = %+v", g.Set.Result)
	}
}

func TestSetRelayedSubmitSendsNoNotice(t *testing.T) {
	s, sent, mu := todoServer(t)
	it := setAdd(t, s, setBody)
	w := do(s, "PUT", "/api/todos/"+it.ID+"/draft", agentHdr("w1:p1"), `{"accept_suggestions":true,"choices":{"D3":{"option":1,"confirmed":true}}}`)
	if w.Code != 200 {
		t.Fatalf("relay draft = %d %s", w.Code, w.Body)
	}
	putDraft(s, it.ID, `{"accept_suggestions":true,"choices":{"D1":{"option":1,"confirmed":true},"D3":{"option":1,"confirmed":true},"D6":{"option":1,"confirmed":true}}}`)
	w = do(s, "POST", "/api/todos/"+it.ID+"/submit", agentHdr("w1:p1"), ``)
	if g := todoOf(t, w.Body.Bytes()); w.Code != 200 || g.Answer.Via != "relay" || g.Notice != nil {
		t.Fatalf("relay submit = %d %+v", w.Code, g)
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for _, m := range *sent {
		if strings.Contains(m, "answered to-do") {
			t.Fatalf("notice after a relayed submit: %v", *sent)
		}
	}
}
