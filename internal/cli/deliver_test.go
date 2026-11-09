package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeliverSelfAndOther(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/deliveries"] = answer{body: `{"delivery":{}}`}
	// Yours by default.
	if code := run(e, "deliver", "pg_0123456789", "pg_abcdef0123"); code != 0 {
		t.Fatalf("deliver: %d %s", code, errb)
	}
	var b map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/deliveries").body), &b)
	if b["agent_id"] != "laharl/w1:p1" || len(b["pages"].([]any)) != 2 {
		t.Fatalf("body = %v", b)
	}
	if !strings.Contains(out.String(), "Recorded 2 pages") {
		t.Fatalf("stdout = %q", out)
	}
	// Another agent, and what the pages lack.
	out.Reset()
	if code := run(e, "deliver", "w2:p1", "pg_0123456789", "--not-captured", "src/ has a fix, not in the page"); code != 0 {
		t.Fatalf("deliver: %d %s", code, errb)
	}
	json.Unmarshal([]byte(f.find("POST", "/api/deliveries").body), &b)
	if b["agent_id"] == "laharl/w1:p1" || b["not_captured"].([]any)[0] != "src/ has a fix, not in the page" {
		t.Fatalf("body = %v", b)
	}
	if !strings.Contains(out.String(), "stays a warning") {
		t.Fatalf("stdout = %q", out)
	}
	if code := run(e, "deliver", "w2:p1"); code == 0 {
		t.Fatal("deliver with no page succeeded")
	}
}

func TestMarkDeliveredRecordsBeforeMarking(t *testing.T) {
	f, e, _, errb := newFake(t)
	f.answers["POST /api/deliveries"] = answer{body: `{"delivery":{}}`}
	f.answers["POST /api/marks"] = answer{body: `{"mark":{}}`}
	if code := run(e, "mark", "w2:p1", "--reason", "published", "--discard", "--delivered", "pg_0123456789,pg_abcdef0123"); code != 0 {
		t.Fatalf("mark: %d %s", code, errb)
	}
	var order []string
	for _, r := range f.reqs {
		if r.path == "/api/deliveries" || r.path == "/api/marks" {
			order = append(order, r.path)
		}
	}
	if strings.Join(order, " ") != "/api/deliveries /api/marks" {
		t.Fatalf("order = %v", order)
	}
	if code := run(e, "mark", "w2:p1", "--reason", "x", "--delivered", "pg_0123456789"); code == 0 {
		t.Fatal("--delivered without --discard was accepted")
	}
}
