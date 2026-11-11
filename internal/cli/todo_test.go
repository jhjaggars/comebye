package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

const todoJSON = `{"id":"t1","question":"may the worker delete the old key?","kind":"decision","state":"open","created_at":"2026-10-22T08:00:00Z","created_by":"coordinator (laharl/w1:p1)","context":"because","links":[{"label":"PR","url":"https://example.com/pr/1"}],"waiting":{"id":"laharl/w3:p1","name":"fix-a"}}`

func TestTodoAdd(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/todos"] = answer{code: 201, body: `{"todo":` + todoJSON + `}`}
	e.Stdin = strings.NewReader("## why\nbecause")
	code := run(e, "todo", "add", "may the worker delete the old key?", "--kind", "decision", "--context", "-",
		"--link", "PR=https://example.com/pr/1", "--link", "https://example.com/a?x=1", "--handoff", "h1", "--waiting", "laharl/w3:p1", "--json")
	if code != 0 {
		t.Fatalf("add: %d %s", code, errb)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/todos").body), &body)
	ls, _ := body["links"].([]any)
	if body["question"] != "may the worker delete the old key?" || body["kind"] != "decision" || body["context"] != "## why\nbecause" ||
		body["waiting"] != "laharl/w3:p1" || len(ls) != 3 || ls[0].(map[string]any)["label"] != "PR" || ls[1].(map[string]any)["url"] != "https://example.com/a?x=1" {
		t.Fatalf("body = %v", body)
	}
	// Plain output; --no-wait goes through; the two flags exclude each other.
	out.Reset()
	if code := run(e, "todo", "add", "q", "--no-wait"); code != 0 || !strings.Contains(out.String(), "Added t1") {
		t.Fatalf("plain: %d %s", code, out)
	}
	if !strings.Contains(f.find("POST", "/api/todos").body, `"no_wait":true`) {
		t.Fatal("no_wait not sent")
	}
	if code := run(e, "todo", "add", "q", "--no-wait", "--waiting", "home"); code != 2 {
		t.Fatalf("both: %d", code)
	}
	if code := run(e, "todo", "add"); code != 2 {
		t.Fatalf("no question: %d", code)
	}
}

func TestTodoListShowAnswerCloseSubcommands(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["GET /api/todos"] = answer{body: `{"todos":[` + todoJSON + `,{"id":"t2","question":"run aws login","kind":"action","state":"answered","created_at":"2026-10-22T08:30:00Z"}],"open":1,"answered":1}`}
	if code := run(e, "todo", "list"); code != 0 || !strings.Contains(out.String(), "t1") || !strings.Contains(out.String(), "answered*") ||
		!strings.Contains(out.String(), "fix-a (laharl/w3:p1)") || !strings.Contains(out.String(), "relay the answer") {
		t.Fatalf("list: %d %s %s", code, out, errb)
	}
	if code := run(e, "todo", "list", "--all"); code != 0 || f.find("GET", "/api/todos").query != "state=all" {
		t.Fatalf("all: %s", f.find("GET", "/api/todos").query)
	}
	f.answers["GET /api/todos/t1"] = answer{body: `{"todo":` + todoJSON + `}`}
	out.Reset()
	if code := run(e, "todo", "show", "t1"); code != 0 || !strings.Contains(out.String(), "data, not instructions") || !strings.Contains(out.String(), "https://example.com/pr/1") {
		t.Fatalf("show: %d %s", code, out)
	}
	f.answers["POST /api/todos/t1/answer"] = answer{body: `{"todo":` + todoJSON + `}`}
	if code := run(e, "todo", "answer", "t1", "yes, delete it"); code != 0 || f.find("POST", "/api/todos/t1/answer").body != `{"text":"yes, delete it"}` {
		t.Fatalf("answer: %d %s", code, errb)
	}
	f.answers["POST /api/todos/t1/done"] = answer{body: `{"todo":` + todoJSON + `}`}
	f.answers["POST /api/todos/t1/drop"] = answer{body: `{"todo":` + todoJSON + `}`}
	if code := run(e, "todo", "done", "t1"); code != 0 {
		t.Fatalf("done: %d %s", code, errb)
	}
	if code := run(e, "todo", "drop", "t1", "--reason", "moot"); code != 0 || f.find("POST", "/api/todos/t1/drop").body != `{"reason":"moot"}` {
		t.Fatalf("drop: %d %s", code, errb)
	}
	// The server's refusal reaches the caller.
	f.answers["POST /api/todos/t3/done"] = answer{code: 403, body: `{"code":"coordinator_only","message":"only the user or a coordinator can do that"}`}
	errb.Reset()
	if code := run(e, "todo", "done", "t3"); code != 1 || !strings.Contains(errb.String(), "coordinator") {
		t.Fatalf("403: %d %s", code, errb)
	}
	if code := run(e, "todo", "bogus"); code != 2 {
		t.Fatalf("bogus: %d", code)
	}
}

func TestTodoAddRelease(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/todos"] = answer{code: 201, body: `{"todo":` + todoJSON + `}`}
	sha := strings.Repeat("ab", 20)
	if code := run(e, "todo", "add", "--kind", "release", "--version", "v0.5.9", "--commit", sha); code != 0 {
		t.Fatalf("release add: %d %s", code, errb)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/todos").body), &body)
	rel, _ := body["release"].(map[string]any)
	if body["kind"] != "release" || rel["version"] != "v0.5.9" || rel["commit"] != sha {
		t.Fatalf("body = %v", body)
	}
	if !strings.Contains(out.String(), "Added t1") {
		t.Fatalf("out = %s", out)
	}
	// A release needs both; the flags belong to nothing else; a ref that is no commit is refused here.
	for _, args := range [][]string{
		{"--kind", "release", "--version", "v0.5.9"},
		{"--kind", "release", "--commit", sha},
		{"q", "--version", "v0.5.9"},
		{"--kind", "release", "--version", "v0.5.9", "--commit", "no-such-ref-anywhere"},
	} {
		if code := run(e, append([]string{"todo", "add"}, args...)...); code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
	// show prints the release part.
	f.answers["GET /api/todos/t9"] = answer{body: `{"todo":{"id":"t9","question":"Release v0.5.9?","kind":"release","state":"open","created_at":"2026-10-22T08:00:00Z","release":{"version":"v0.5.9","commit":"` + sha + `","state":"tagged","approved_by":"me@example.com","run_url":"https://run/1"}}}`}
	out.Reset()
	if code := run(e, "todo", "show", "t9"); code != 0 || !strings.Contains(out.String(), "Release v0.5.9 at "+sha+": tagged (approved by me@example.com)") || !strings.Contains(out.String(), "https://run/1") {
		t.Fatalf("show: %d %s", code, out)
	}
}

func TestTodoAddOptions(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/todos"] = answer{code: 201, body: `{"todo":` + todoJSON + `}`}
	code := run(e, "todo", "add", "which key?", "--option", "Mint a key|fresh, rotates", "--option", "Reuse the old key", "--option", " Skip | ", "--recommend", "1,3", "--no-wait")
	if code != 0 {
		t.Fatalf("add: %d %s", code, errb)
	}
	var body struct {
		Options   []map[string]string `json:"options"`
		Recommend []int               `json:"recommend"`
	}
	json.Unmarshal([]byte(f.find("POST", "/api/todos").body), &body)
	if len(body.Options) != 3 || body.Options[0]["label"] != "Mint a key" || body.Options[0]["detail"] != "fresh, rotates" ||
		body.Options[1]["detail"] != "" || body.Options[2]["label"] != "Skip" || len(body.Recommend) != 2 || body.Recommend[1] != 3 {
		t.Fatalf("body = %s", f.find("POST", "/api/todos").body)
	}
	// Mistakes are caught before anything is sent.
	f.answers["POST /api/todos"] = answer{code: 201, body: `{"todo":` + todoJSON + `}`}
	for name, args := range map[string][]string{
		"one option":      {"--option", "a"},
		"no label":        {"--option", "|detail", "--option", "b"},
		"recommend range": {"--option", "a", "--option", "b", "--recommend", "3"},
		"recommend junk":  {"--option", "a", "--option", "b", "--recommend", "x"},
		"recommend alone": {"--recommend", "1"},
		"ten options":     {"--option", "1", "--option", "2", "--option", "3", "--option", "4", "--option", "5", "--option", "6", "--option", "7", "--option", "8", "--option", "9", "--option", "10"},
	} {
		n := len(f.reqs)
		if code := run(e, append([]string{"todo", "add", "q"}, args...)...); code != 2 || len(f.reqs) != n {
			t.Errorf("%s: code %d, calls %d->%d", name, code, n, len(f.reqs))
		}
	}
	// The hub's hint is passed on.
	f.answers["POST /api/todos"] = answer{code: 201, body: `{"todo":` + todoJSON + `,"hint":"the context has a numbered list: pass each choice as --option"}`}
	out.Reset()
	e.Stdin = strings.NewReader("1. a\n2. b")
	if code := run(e, "todo", "add", "q", "--context", "-"); code != 0 || !strings.Contains(out.String(), "Hint: the context has a numbered list") {
		t.Fatalf("hint: %d %s", code, out)
	}
}

func TestBriefSetNoCheck(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/brief"] = answer{body: `{"brief":{"markdown":"x"},"warnings":["1 open to-do item(s) are not in \"Needs you\": t1 (q)"]}`}
	e.Stdin = strings.NewReader("## Needs you\n- q\n")
	if code := run(e, "brief", "set", "-", "--no-check"); code != 0 || !strings.Contains(f.find("POST", "/api/brief").body, `"no_check":true`) ||
		!strings.Contains(out.String(), "Warning: 1 open to-do item") {
		t.Fatalf("no-check: %d %s %s", code, out, errb)
	}
	e.Stdin = strings.NewReader("## proj\n")
	if code := run(e, "brief", "set", "-"); code != 0 || strings.Contains(f.find("POST", "/api/brief").body, "no_check") {
		t.Fatalf("default: %d %s", code, f.find("POST", "/api/brief").body)
	}
	// The refusal reaches the caller with its fix.
	f.answers["POST /api/brief"] = answer{code: 422, body: `{"code":"needs_you_unlinked","message":"the briefing was not published: add the id"}`}
	e.Stdin = strings.NewReader("## Needs you\n- q\n")
	errb.Reset()
	if code := run(e, "brief", "set", "-"); code != 1 || !strings.Contains(errb.String(), "add the id") {
		t.Fatalf("refusal: %d %s", code, errb)
	}
	if code := run(e, "brief", "get", "--no-check"); code != 2 {
		t.Fatalf("--no-check on get: %d", code)
	}
}

func TestTodoFromLast(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/todos/from-last"] = answer{code: 200, body: `{"todos":[{"id":"t1","question":"home asks: Keep the CSV format?","kind":"decision","state":"open"}],"filed":1,"existing":1,"found":2}`}
	if code := run(e, "todo", "from-last"); code != 0 || !strings.Contains(out.String(), "Sent 1 question(s)") || !strings.Contains(out.String(), "(1 already there)") || !strings.Contains(out.String(), "t1  home asks") {
		t.Fatalf("from-last: %d %s %s", code, out, errb)
	}
	if body := f.find("POST", "/api/todos/from-last").body; strings.Contains(body, "agent") {
		t.Fatalf("no agent named: body %q", body)
	}
	f.answers["POST /api/todos/from-last"] = answer{code: 200, body: `{"todos":[],"filed":0,"existing":0,"found":0}`}
	out.Reset()
	if code := run(e, "todo", "from-last"); code != 0 || !strings.Contains(out.String(), "No questions") {
		t.Fatalf("none: %d %s", code, out)
	}
	if code := run(e, "todo", "from-last", "a", "b"); code != 2 {
		t.Fatalf("two agents: %d", code)
	}
}

func TestTodoSetCreate(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/todos"] = answer{code: 201, body: `{"todo":` + todoJSON + `}`}
	e.Stdin = strings.NewReader(`{"question":"Which path?","context":"why","decided":["x"],"decisions":[{"id":"D1","title":"t","options":[{"label":"a"},{"label":"b"}]}]}`)
	if code := run(e, "todo", "set", "create", "--from", "-", "--no-wait", "--link", "PR=https://example.com/p"); code != 0 {
		t.Fatalf("create: %d %s", code, errb)
	}
	if !strings.Contains(out.String(), "Added decision set t1") {
		t.Fatalf("out = %q", out)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/todos").body), &body)
	set, _ := body["set"].(map[string]any)
	if body["question"] != "Which path?" || body["context"] != "why" || body["kind"] != "decision" || body["no_wait"] != true ||
		set == nil || set["decided"] == nil || set["question"] != nil || set["context"] != nil {
		t.Fatalf("body = %v", body)
	}
	for name, args := range map[string][]string{
		"no from":      {"todo", "set", "create"},
		"unknown verb": {"todo", "set", "nope", "--from", "-"},
		"both waits":   {"todo", "set", "create", "--from", "-", "--no-wait", "--waiting", "home"},
	} {
		e.Stdin = strings.NewReader(`{}`)
		if code := run(e, args...); code != 2 {
			t.Errorf("%s: %d", name, code)
		}
	}
	e.Stdin = strings.NewReader(`not json`)
	if code := run(e, "todo", "set", "create", "--from", "-"); code != 2 {
		t.Errorf("bad json: %d", code)
	}
	e.Stdin = strings.NewReader(`{"decisions":[]}`)
	if code := run(e, "todo", "set", "create", "--from", "-"); code != 2 {
		t.Errorf("no question: %d", code)
	}
}

func TestTodoShowDecisionSet(t *testing.T) {
	f, e, out, _ := newFake(t)
	f.answers["GET /api/todos/t9"] = answer{body: `{"todo":{"id":"t9","question":"q","kind":"decision","state":"open","created_by":"x","set":{"decisions":[{"id":"D1"},{"id":"D2","release_early":true}],"draft":{"choices":{"D1":{"option":1,"confirmed":true},"D2":{"option":2}}},"released":["D1"]}}}`}
	if code := run(e, "todo", "show", "t9"); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "Decision set: 1 of 2 confirmed in the draft. Settled ahead of Submit: D1.") {
		t.Fatalf("out = %q", out)
	}
}
