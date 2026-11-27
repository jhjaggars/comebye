package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/archive"
)

// Without --index or --local, recall asks this host's server, which answers
// from the hub's index; the question and its filters go as one body.
func TestRecallAsksTheServer(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/recall/query"] = answer{body: `{"rows":[{"host":"desk","kind":"claude","id":"44444444-4444-4444-8444-444444444444","title":"zebra work","started":"2026-10-03T08:00:00Z","n_msgs":3}]}`}
	if code := run(e, "recall", "search", "zebra", "stripes", "--host", "desk", "--limit", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if !strings.Contains(out.String(), "zebra work") {
		t.Fatalf("out = %s", out)
	}
	r := f.find("POST", "/api/recall/query")
	if r == nil {
		t.Fatal("no query sent")
	}
	var q archive.RecallQuery
	if err := json.Unmarshal([]byte(r.body), &q); err != nil {
		t.Fatal(err)
	}
	if q.Op != "search" || strings.Join(q.Args, " ") != "zebra stripes" || q.Filter.Host != "desk" || q.Filter.Limit != 5 {
		t.Fatalf("query = %+v", q)
	}
	// The hub has no index yet: said plainly.
	f.answers["POST /api/recall/query"] = answer{code: 503, body: `{"code":"no_index","message":"this host has no transcript index yet"}`}
	errb.Reset()
	if code := run(e, "recall", "list"); code == 0 || !strings.Contains(errb.String(), "the hub has no transcript index yet") {
		t.Fatalf("exit %d: %s", code, errb)
	}
	// A question with its arguments missing is refused before anything is sent.
	n := len(f.reqs)
	if code := run(e, "recall", "show"); code == 0 || len(f.reqs) != n {
		t.Fatalf("show with no id: exit %d, %d requests", code, len(f.reqs)-n)
	}
}
