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
