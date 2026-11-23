package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The steward's pass (tools/steward/pass.py) with the built binary against a
// throwaway hub and a fake gh (tools/steward/tests/fake_gh.py): its checks in
// tools/steward/tests/pass_check.py, then the script as the steward runs it.
func TestStewardPassAgainstAHub(t *testing.T) {
	ago := hubAgo
	h := startAPIHub(t, []any{
		hubEntry(1, "fix-detail-menu", "w3:p1", "running", "progress", map[string]any{"text": "running the browser tests", "at": ago(0.05)}),
		hubEntry(2, "nap-loop", "w4:p1", "running", "loop", "run a pass", "loop_every_minutes", 60, "last_pass_at", ago(3)),
		hubEntry(3, "decider", "w5:p1", "finished", "final_report", "## Summary\nx\n\n## Decisions for the user\n1. Land it?\n   Recommend: 1\n",
			"report_source", "explicit", "report_seq", 1, "report_at", ago(3), "finished_at", ago(3)),
		hubEntry(4, "ghost-worker", "w99:p1", "finished", "finished_at", ago(5)),
		hubEntry(5, "landed-worker", "w98:p1", "landed", "finished_at", ago(4)),
		hubEntry(9, "launch-fail", "", "failed", "agent_id", "", "created_at", ago(2), "error", "outside_roots"),
		hubEntry(10, "retry-me", "w3:p1", "failed", "branch", "feat/retry", "created_at", ago(5), "error", "outside_roots"),
		hubEntry(11, "retry-me", "w3:p1", "running", "branch", "feat/retry", "created_at", ago(1), "brief", "fix the launch for issues/44",
			"progress", map[string]any{"text": "retrying", "at": ago(0.1)}),
	}, []any{
		map[string]any{"id": "t0000000001", "question": "Land OWNER/REPO#12?", "kind": "followup", "state": "open",
			"created_at": ago(2), "updated_at": ago(2)},
		map[string]any{"id": "t0000000002", "question": "Land PR #7?", "kind": "decision", "state": "done",
			"waiting": map[string]any{"id": "devbox/w99:p1", "name": "ghost-worker"},
			"answer":  map[string]any{"text": "ok 1", "by": "user", "at": ago(3)}, "created_at": ago(4), "updated_at": ago(3), "closed_at": ago(3)},
		map[string]any{"id": "t0000000003", "question": "Land PR #8?", "kind": "decision", "state": "done",
			"waiting": map[string]any{"id": "devbox/w98:p1", "name": "landed-worker"},
			"answer":  map[string]any{"text": "ok 1", "by": "user", "at": ago(3)}, "created_at": ago(4), "updated_at": ago(3), "closed_at": ago(3)},
	})
	const dir = "tools/steward"
	h.must(dir, "", "prompt", "needs-input", "please check the flaky test", "--queue")

	// gh is the fake, first on PATH.
	bin := t.TempDir()
	fake := filepath.Join(h.root, dir, "tests", "fake_gh.py")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexec python3 '"+fake+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ghLog := filepath.Join(bin, "gh.log")
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "FAKE_GH_LOG=" + ghLog}
	for i, kv := range h.env { // the PATH above wins
		if strings.HasPrefix(kv, "PATH=") {
			h.env = append(h.env[:i:i], h.env[i+1:]...)
			break
		}
	}

	code, out, errs := h.cli(dir, "", env, "api", "run", "tests/pass_check.py")
	if code != 0 || !strings.Contains(errs, "OK") {
		t.Fatalf("pass_check.py: exit %d\n%s%s", code, out, errs)
	}
	t.Logf("pass_check.py:\n%s", errs)

	// As the steward runs it: a config file, the text out.
	cfg := filepath.Join(bin, "steward.json")
	b, _ := json.Marshal(map[string]any{
		"prs":             []any{map[string]any{"repo": "OWNER/REPO"}},
		"comment_watches": []any{map[string]any{"repo": "ORG/UP", "number": 5}},
	})
	os.WriteFile(cfg, b, 0o600)
	code, out, errs = h.cli(dir, "", env, "api", "run", "pass.py", "--config", cfg)
	if code != 0 {
		t.Fatalf("pass.py: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"steward pass ", "FINDINGS (", "[user] prs: OWNER/REPO#13 Red and nobody on it",
		"https://github.com/OWNER/REPO/pull/13", "ORG/UP#5: maintainer comment unanswered",
		"answered to-do t0000000002: ghost-worker has not acted", "WATCH (", "PILE-UP ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pass.py: no %q in\n%s%s", want, out, errs)
		}
	}
	if strings.Contains(out, "ERRORS") {
		t.Errorf("pass.py: errors\n%s", out)
	}
	// An unknown config key is refused, not ignored.
	os.WriteFile(cfg, []byte(`{"prz": []}`), 0o600)
	if code, out, errs := h.cli(dir, "", env, "api", "run", "pass.py", "--config", cfg); code == 0 || !strings.Contains(out+errs, "unknown config keys: prz") {
		t.Errorf("a bad key: exit %d\n%s%s", code, out, errs)
	}
	// Read-only: gh was asked only to list and view.
	logb, _ := os.ReadFile(ghLog)
	for _, l := range strings.Split(strings.TrimSpace(string(logb)), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || !(f[1] == "list" || f[1] == "view" || (f[0] == "api" && f[1] == "user")) {
			t.Errorf("gh was asked %q", l)
		}
	}
}
