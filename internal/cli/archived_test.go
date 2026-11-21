package cli

import (
	"strings"
	"testing"
)

func TestArchivedCommands(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["GET /api/archived"] = answer{body: `{"count":1,"build_freed":18400000000,"worktree_bytes":2000000,"archived":[{"id":"h1","name":"kv-spoke","host":"desk","project":"homelab","branch":"poc/kv-spoke","purpose":"poc",
		"archive":{"at":"2026-11-20T10:00:00Z","by":"hub","why":"poc work, reported; no progress for 3 days","from":"finished","build_freed":18400000000,"build_dirs":["target"],"worktree_bytes":2000000,"unmerged":12,"unpushed":0,
		"left":[{"type":"cleanup","text":"a git stash entry"}]}}]}`}
	if code := run(e, "archived"); code != 0 {
		t.Fatalf("archived: %d %s", code, errb)
	}
	for _, want := range []string{"kv-spoke", "poc/kv-spoke", "12 (pushed)", "a git stash entry", "18.4 GB: target", "1 archived; 18.4 GB of build output freed", "No source is ever deleted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("archived output lacks %q:\n%s", want, out)
		}
	}
	f.answers["POST /api/archived"] = answer{body: `{"entry":{"id":"h1","agent_name":"kv-spoke","branch":"poc/kv-spoke","archive":{"park_id":"pk_1","build_freed":5000000}}}`}
	out.Reset()
	if code := run(e, "archived", "add", "kv-spoke", "--reason", "superseded"); code != 0 || !strings.Contains(out.String(), "Archived kv-spoke (h1, poc/kv-spoke)") || !strings.Contains(out.String(), "Freed 5 MB") {
		t.Fatalf("add: %d %s %s", code, out, errb)
	}
	if b := f.find("POST", "/api/archived").body; b != `{"reason":"superseded","ref":"kv-spoke"}` {
		t.Fatalf("add body %s", b)
	}
	f.answers["POST /api/archived/restore"] = answer{body: `{"entry":{"id":"h1","agent_name":"kv-spoke","status":"finished"},"resume_error":"it has no park record"}`}
	out.Reset()
	if code := run(e, "archived", "restore", "kv-spoke", "--resume"); code != 0 || !strings.Contains(out.String(), "Restored kv-spoke (h1): it is finished again.") || !strings.Contains(out.String(), "Not resumed: it has no park record") {
		t.Fatalf("restore: %d %s %s", code, out, errb)
	}
	if b := f.find("POST", "/api/archived/restore").body; b != `{"ref":"kv-spoke","resume":true}` {
		t.Fatalf("restore body %s", b)
	}
}
