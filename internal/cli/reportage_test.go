package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// The ledger and `read --report` say how old a report is, and show a newer
// reply (a pass or a re-finish without a report) beside it.
func TestReportAge(t *testing.T) {
	f, e, out, errb := newFake(t)
	// Now is 2026-10-22 09:00 UTC.
	f.answers["GET /api/ledger"] = answer{body: `{"entries":[
		{"id":"h1","host":"laharl","repo_name":"proj","branch":"fix/a","agent_name":"fix-a","agent_id":"laharl/w3:p1","status":"finished",
		 "final_report":"## Summary\nCI flaked.","report_source":"explicit","report_seq":3,"report_at":"2026-10-22T07:00:00Z",
		 "last_pass_report":"CI is green now.","last_pass_report_at":"2026-10-22T08:50:00Z"},
		{"id":"h2","host":"laharl","repo_name":"proj","branch":"fix/b","agent_name":"fix-b","status":"finished",
		 "final_report":"old text","report_source":"explicit","report_seq":1},
		{"id":"h3","host":"laharl","repo_name":"proj","branch":"loop/steward","agent_name":"steward","status":"running","loop":"pass",
		 "last_pass_report":"nothing new","last_pass_report_at":"2026-10-22T08:30:00Z"},
		{"id":"h4","host":"laharl","repo_name":"proj","branch":"fix/c","agent_name":"fix-c","status":"finished",
		 "final_report":"took it","report_at":"2026-10-22T08:59:30Z"}]}`}
	if code := run(e, "ledger"); code != 0 {
		t.Fatalf("ledger: %d %s", code, errb)
	}
	s := out.String()
	for _, want := range []string{"yes, 2h ago, newer reply 10m ago", "yes, age unknown", "-, newer reply 30m ago", "yes, 30s ago"} {
		if !strings.Contains(s, want) {
			t.Errorf("ledger lacks %q:\n%s", want, s)
		}
	}

	out.Reset()
	if code := run(e, "read", "h1", "--report"); code != 0 {
		t.Fatalf("read: %d %s", code, errb)
	}
	s = out.String()
	for _, want := range []string{
		"Final report of fix-a on proj/fix/a (h1, finished, report 3 from 2h ago):",
		"the worker's last reply, from 10m ago, is newer than this report",
		"CI flaked.",
		"── Newer: the worker's last reply, from 10m ago (a turn that ended without a report: not a report)",
		"CI is green now.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("read --report lacks %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "CI flaked.") > strings.Index(s, "CI is green now.") {
		t.Errorf("the newer reply comes after the report:\n%s", s)
	}

	out.Reset()
	run(e, "read", "h2", "--report", "--summary")
	if !strings.Contains(out.String(), "(h2, finished, report from an unrecorded time (before report times were kept)):") {
		t.Errorf("unknown age:\n%s", out)
	}
	out.Reset()
	run(e, "read", "h4", "--report")
	if !strings.Contains(out.String(), "report (its last reply, taken by the hub) from 30s ago") {
		t.Errorf("hub-taken report:\n%s", out)
	}

	// No report yet, but a pass ended: its reply, said to be no report.
	out.Reset()
	if code := run(e, "read", "h3", "--report"); code != 0 || !strings.Contains(out.String(), "has no final report yet. Its last reply, from 30m ago") || !strings.Contains(out.String(), "nothing new") {
		t.Errorf("pass reply: %d %s %s", code, out, errb)
	}

	out.Reset()
	if code := run(e, "read", "h1", "--report", "--json"); code != 0 {
		t.Fatalf("json: %d %s", code, errb)
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v["report_age"] != "2h ago" || v["report_at"] != "2026-10-22T07:00:00Z" || v["report_seq"] != float64(3) ||
		v["newer_reply"] != "CI is green now." || v["newer_reply_age"] != "10m ago" {
		t.Errorf("json = %v", v)
	}
}
