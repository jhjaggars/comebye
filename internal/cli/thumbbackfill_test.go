package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func listPage(id, kind string, ver int, thumbs string) string {
	th := ""
	if thumbs != "" {
		th = `,"thumbs":["light","dark"]`
	}
	return `{"id":"` + id + `","title":"t","kind":"` + kind + `","version":` + string(rune('0'+ver)) + `,"size":10,"versions":[{"n":` + string(rune('0'+ver)) + th + `}]}`
}

func TestBackfillStoresOnlyWhatLacksThumbnails(t *testing.T) {
	f, e, out, _ := newFake(t)
	f.answers["GET /api/pages"] = answer{body: `{"pages":[` + strings.Join([]string{
		listPage("pg_0000000001", "html", 2, ""),
		listPage("pg_0000000002", "html", 1, "x"),
		listPage("pg_0000000003", "md", 1, ""),
		listPage("pg_0000000004", "html", 3, ""),
	}, ",") + `]}`}
	f.answers["GET /api/pages/pg_0000000001"] = answer{body: `{"page":` + listPage("pg_0000000001", "html", 2, "") + `,"body":"<p>one","body_version":2}`}
	f.answers["GET /api/pages/pg_0000000004"] = answer{body: `{"page":` + listPage("pg_0000000004", "html", 3, "") + `,"body":"<p>four","body_version":3}`}
	f.answers["PUT /api/pages/pg_0000000001/thumbs"] = answer{body: `{}`}
	f.answers["PUT /api/pages/pg_0000000004/thumbs"] = answer{code: 400, body: `{"code":"bad_thumb","message":"nope"}`}
	chromeEnv(e, nil)
	fakeShots(t, "")
	if code := run(e, "pages", "thumbs", "--backfill", "--all"); code != 1 { // one page failed
		t.Fatalf("exit %d: %s", code, out)
	}
	s := out.String()
	for _, w := range []string{"stored   pg_0000000001 v2", "failed   pg_0000000004 v3", "the hub did not take it", "1 stored, 2 skipped, 1 failed (of 4 pages)", "1 already has thumbnails", "1 not HTML (md)"} {
		if !strings.Contains(s, w) {
			t.Errorf("output lacks %q:\n%s", w, s)
		}
	}
	put := f.find("PUT", "/api/pages/pg_0000000001/thumbs")
	if put == nil || !strings.Contains(put.body, `"version":2`) || !strings.Contains(put.body, "light_b64") || !strings.Contains(put.body, "dark_b64") {
		t.Fatalf("put = %+v", put)
	}
	for _, id := range []string{"pg_0000000002", "pg_0000000003"} {
		if f.find("GET", "/api/pages/"+id) != nil || f.find("PUT", "/api/pages/"+id+"/thumbs") != nil {
			t.Errorf("%s was touched", id)
		}
	}
	// Never content: no POST, DELETE.
	for _, r := range f.reqs {
		if r.method != "GET" && r.method != "PUT" {
			t.Errorf("%s %s", r.method, r.path)
		}
	}
}

func TestBackfillDryRunRendersAndSendsNothing(t *testing.T) {
	f, e, out, _ := newFake(t)
	f.answers["GET /api/pages/pg_0000000001"] = answer{body: `{"page":` + listPage("pg_0000000001", "html", 2, "") + `}`}
	chromeEnv(e, nil)
	called := 0
	fakeShots(t, "")
	old := shotRender
	shotRender = func(_ context.Context, _, _, _ string) ([]byte, error) { called++; return nil, errors.New("no") }
	t.Cleanup(func() { shotRender = old })
	if code := run(e, "pages", "thumbs", "--backfill", "--dry-run", "pg_0000000001"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if called != 0 || f.find("PUT", "/api/pages/pg_0000000001/thumbs") != nil || !strings.Contains(out.String(), "dry run: would render and upload") {
		t.Errorf("called %d: %s", called, out)
	}
}

func TestBackfillSaysWhyChromeFailed(t *testing.T) {
	f, e, out, _ := newFake(t)
	f.answers["GET /api/pages/pg_0000000001"] = answer{body: `{"page":` + listPage("pg_0000000001", "html", 2, "") + `,"body":"<p>x","body_version":2}`}
	if code := run(e, "pages", "thumbs", "--backfill", "pg_0000000001"); code != 1 || !strings.Contains(out.String(), "failed   pg_0000000001 v2") || !strings.Contains(out.String(), "no Chrome found") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestBackfillUsage(t *testing.T) {
	_, e, _, errb := newFake(t)
	for _, args := range [][]string{{"pages", "thumbs"}, {"pages", "thumbs", "--backfill"}, {"pages", "thumbs", "--backfill", "--all", "pg_0000000001"}} {
		if code := run(e, args...); code == 0 {
			t.Errorf("%v accepted", args)
		}
	}
	_ = errb
}
