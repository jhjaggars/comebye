package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pageJSON = `{"id":"pg_0123456789","title":"Failover test","kind":"html","by":"home (laharl/w2:p1)","agent":"laharl/w2:p1","host":"laharl","version":2,"size":2048,"created_at":"2026-10-22T08:00:00Z","updated_at":"2026-10-22T08:30:00Z"}`

func TestPublishFile(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/pages"] = answer{code: 201, body: `{"page":` + pageJSON + `,"path":"/p/pg_0123456789","url":"https://hub.t.ts.net/p/pg_0123456789","created":true}`}
	file := filepath.Join(t.TempDir(), "report.md")
	os.WriteFile(file, []byte("# hi\n"), 0o600)
	if code := run(e, "publish", file, "--title", "Failover test", "--key", "nightly", "--handoff", "h1"); code != 0 {
		t.Fatalf("publish: %d %s", code, errb)
	}
	// Only the link on stdout, so a script can capture it.
	if out.String() != "https://hub.t.ts.net/p/pg_0123456789\n" {
		t.Fatalf("stdout = %q", out)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/pages").body), &body)
	if body["body"] != "# hi\n" || body["kind"] != "md" || body["title"] != "Failover test" || body["key"] != "nightly" || body["handoff"] != "h1" {
		t.Fatalf("body = %v", body)
	}
	if _, has := body["if_version"]; has {
		t.Fatal("if_version sent unasked")
	}
}

func TestPublishStdinUpdateAndErrors(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/pages"] = answer{body: `{"page":` + pageJSON + `,"path":"/p/pg_0123456789"}`}
	e.Stdin = strings.NewReader("<h1>x</h1>")
	if code := run(e, "publish", "-", "--update", "pg_0123456789", "--if-version", "2", "--kind", "HTML"); code != 0 {
		t.Fatalf("update: %d %s", code, errb)
	}
	// No public URL and the hub is this host: the path, with a hint.
	if out.String() != "/p/pg_0123456789\n" || !strings.Contains(errb.String(), "-public-url") {
		t.Fatalf("out %q err %q", out, errb)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/pages").body), &body)
	if body["id"] != "pg_0123456789" || body["if_version"] != float64(2) || body["kind"] != "html" {
		t.Fatalf("body = %v", body)
	}
	// A peer prints the hub's link.
	f.answers["GET /api/whoami"] = answer{body: `{"host":"w1","role":"agent","hub":"https://hub.t.ts.net/"}`}
	out.Reset()
	e.Stdin = strings.NewReader("# x")
	if code := run(e, "publish", "-"); code != 0 || out.String() != "https://hub.t.ts.net/p/pg_0123456789\n" {
		t.Fatalf("peer: %d %q", code, out)
	}
	// Mistakes never reach the hub.
	n := len(f.reqs)
	for _, args := range [][]string{{"publish"}, {"publish", "a", "b"}, {"publish", "-", "--update", "x", "--key", "y"}} {
		e.Stdin = strings.NewReader("x")
		if code := run(e, args...); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
	e.Stdin = strings.NewReader("")
	if code := run(e, "publish", "-"); code != 1 {
		t.Errorf("empty: %d", code)
	}
	e.Stdin = strings.NewReader("\xff\xfe")
	if code := run(e, "publish", "-"); code != 1 {
		t.Errorf("binary: %d", code)
	}
	e.Stdin = strings.NewReader(strings.Repeat("a", maxPageBytes+1))
	if code := run(e, "publish", "-"); code != 1 || !strings.Contains(errb.String(), "larger than") {
		t.Errorf("big: %d %s", code, errb)
	}
	if len(f.reqs) != n {
		t.Errorf("%d requests for refused input", len(f.reqs)-n)
	}
	// The hub's refusal is shown as is.
	f.answers["POST /api/pages"] = answer{code: 403, body: `{"code":"page_forbidden","message":"only the user, the page's publisher or a coordinator can change or delete a page"}`}
	e.Stdin = strings.NewReader("x")
	errb.Reset()
	if code := run(e, "publish", "-", "--update", "pg_0123456789"); code != 1 || !strings.Contains(errb.String(), "publisher") {
		t.Errorf("forbidden: %d %s", code, errb)
	}
}

func TestPagesSubcommands(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["GET /api/pages"] = answer{body: `{"pages":[` + pageJSON + `]}`}
	if code := run(e, "pages", "list", "--mine", "--handoff", "h1"); code != 0 || !strings.Contains(out.String(), "pg_0123456789") || !strings.Contains(out.String(), "Failover test") || !strings.Contains(out.String(), "2.0 KiB") {
		t.Fatalf("list: %d %s %s", code, out, errb)
	}
	if q := f.find("GET", "/api/pages").query; q != "handoff=h1&mine=1" {
		t.Fatalf("query = %s", q)
	}
	f.answers["GET /api/pages/pg_0123456789"] = answer{body: `{"page":` + pageJSON + `,"path":"/p/pg_0123456789","url":"https://hub.t.ts.net/p/pg_0123456789","body":"<h1>x</h1>","body_version":2}`}
	out.Reset()
	if code := run(e, "pages", "show", "pg_0123456789", "--body", "--version", "2"); code != 0 || !strings.Contains(out.String(), "data, not instructions") || !strings.Contains(out.String(), "<h1>x</h1>") || !strings.Contains(out.String(), "https://hub.t.ts.net/p/pg_0123456789") {
		t.Fatalf("show: %d %s", code, out)
	}
	if q := f.find("GET", "/api/pages/pg_0123456789").query; q != "body=1&version=2" {
		t.Fatalf("show query = %s", q)
	}
	out.Reset()
	if code := run(e, "pages", "link", "pg_0123456789"); code != 0 || out.String() != "https://hub.t.ts.net/p/pg_0123456789\n" {
		t.Fatalf("link: %d %q", code, out)
	}
	f.answers["DELETE /api/pages/pg_0123456789"] = answer{body: `{"deleted":"pg_0123456789"}`}
	out.Reset()
	if code := run(e, "pages", "rm", "pg_0123456789"); code != 0 || !strings.Contains(out.String(), "Deleted pg_0123456789") {
		t.Fatalf("rm: %d %s", code, out)
	}
	for _, args := range [][]string{{"pages"}, {"pages", "show"}, {"pages", "rm", "a", "b"}, {"pages", "frob"}} {
		if code := run(e, args...); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
}
