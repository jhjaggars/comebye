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

func TestTodoLinkAndOpenLine(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/todos"] = answer{code: 201, body: `{"todo":` + todoJSON + `}`}
	f.answers["GET /api/todos/t1"] = answer{body: `{"todo":` + todoJSON + `}`}

	// A peer's route to the hub wins; the hub's public URL is next.
	for _, c := range []struct{ who, want string }{
		{`{"host":"w1","role":"agent","hub":"https://hub.t.ts.net/"}`, "https://hub.t.ts.net/#/todo/t1"},
		{`{"host":"w1","role":"agent","hub":"self","public_url":"https://pub.example.ts.net"}`, "https://pub.example.ts.net/#/todo/t1"},
	} {
		f.answers["GET /api/whoami"] = answer{body: c.who}
		out.Reset()
		errb.Reset()
		if code := run(e, "todo", "link", "t1"); code != 0 || out.String() != c.want+"\n" || errb.Len() != 0 {
			t.Fatalf("link: %d %q %q", code, out, errb)
		}
		out.Reset()
		if code := run(e, "todo", "link", "t1", "--json"); code != 0 {
			t.Fatalf("link json: %d %s", code, errb)
		}
		var j map[string]any
		if json.Unmarshal(out.Bytes(), &j); j["id"] != "t1" || j["url"] != c.want || j["full"] != true {
			t.Fatalf("link json = %s", out)
		}
		out.Reset()
		if code := run(e, "todo", "add", "q"); code != 0 || !strings.Contains(out.String(), "Added t1") || !strings.Contains(out.String(), "Open: "+c.want+"\n") {
			t.Fatalf("add: %d %s", code, out)
		}
		out.Reset()
		if code := run(e, "todo", "add", "q", "--json"); code != 0 {
			t.Fatalf("add json: %d %s", code, errb)
		}
		j = nil
		if json.Unmarshal(out.Bytes(), &j); j["url"] != c.want || j["full"] != true || j["todo"].(map[string]any)["id"] != "t1" {
			t.Fatalf("add json = %s", out)
		}
		out.Reset()
		if code := run(e, "todo", "set", "create", "--from", "-", "--json"); code != 2 {
			// no stdin set: usage error, not a crash
			t.Fatalf("set no input: %d", code)
		}
		e.Stdin = strings.NewReader(`{"question":"pick","decisions":[]}`)
		if code := run(e, "todo", "set", "create", "--from", "-"); code != 0 || !strings.Contains(out.String(), "Open: "+c.want+"\n") {
			t.Fatalf("set create: %d %s %s", code, out, errb)
		}
		out.Reset()
		e.Stdin = strings.NewReader(`{"question":"pick","decisions":[]}`)
		if code := run(e, "todo", "set", "create", "--from", "-", "--json"); code != 0 {
			t.Fatalf("set json: %d %s", code, errb)
		}
		j = nil
		if json.Unmarshal(out.Bytes(), &j); j["url"] != c.want {
			t.Fatalf("set json = %s", out)
		}
	}

	// A duplicate prints the link too.
	f.answers["POST /api/todos"] = answer{body: `{"duplicate":true,"todo":` + todoJSON + `}`}
	out.Reset()
	if code := run(e, "todo", "add", "q"); code != 0 || !strings.Contains(out.String(), "Already on the list as t1.") || !strings.Contains(out.String(), "Open: https://pub.example.ts.net/#/todo/t1") {
		t.Fatalf("dup: %d %s", code, out)
	}

	// No known hub URL: a path, said so on stderr.
	f.answers["GET /api/whoami"] = answer{body: `{"host":"w1","role":"agent","hub":"self"}`}
	out.Reset()
	errb.Reset()
	if code := run(e, "todo", "link", "t1"); code != 0 || out.String() != "/#/todo/t1\n" || !strings.Contains(errb.String(), "path") {
		t.Fatalf("path: %d %q %q", code, out, errb)
	}

	// A typo exits non-zero and prints no URL.
	f.answers["GET /api/todos/t9"] = answer{code: 404, body: `{"code":"not_found","message":"no such to-do"}`}
	out.Reset()
	if code := run(e, "todo", "link", "t9"); code != 1 || out.Len() != 0 {
		t.Fatalf("unknown: %d %q", code, out)
	}
	if code := run(e, "todo", "link"); code != 2 {
		t.Fatalf("no id: %d", code)
	}
}

func TestTodoShowSaysTheHubRelayedIt(t *testing.T) {
	f, e, out, _ := newFake(t)
	f.answers["GET /api/todos/t9"] = answer{body: `{"todo":{"id":"t9","question":"q","kind":"decision","state":"done","created_by":"x","closed_by":"hub","waiting":{"id":"desk/w2:p1","name":"sweep-rebased"},"answer":{"text":"ok 1","by":"me","via":"app","at":"2026-11-21T12:19:00Z"},"relay":{"subject":"relay/t9/1","state":"delivered","to":"sweep-rebased","notice":"b4edba","at":"2026-11-21T12:20:00Z"}}}`}
	if code := run(e, "todo", "show", "t9"); code != 0 {
		t.Fatal(code)
	}
	if want := "Relayed by the hub to sweep-rebased at 12:20 UTC on 2026-11-21; it reached the agent's transcript (notice b4edba)."; !strings.Contains(out.String(), want) {
		t.Fatalf("out = %q", out)
	}
	out.Reset()
	f.answers["GET /api/todos/t9"] = answer{body: `{"todo":{"id":"t9","question":"q","kind":"decision","state":"answered","created_by":"x","relay":{"state":"gaveup","why":"the waiting agent is gone","at":"2026-11-21T12:20:00Z"}}}`}
	if code := run(e, "todo", "show", "t9"); code != 0 || !strings.Contains(out.String(), "The hub could not relay it (the waiting agent is gone)") {
		t.Fatalf("out = %q", out)
	}
}
