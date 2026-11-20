package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readClaudeCfg(t *testing.T, dir string) map[string]map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Projects map[string]map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	return top.Projects
}

func compact(r json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, r) != nil {
		return string(r)
	}
	return b.String()
}

func TestTrustClaudeFolder(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	wt := filepath.Join(t.TempDir(), "wt")
	os.MkdirAll(wt, 0o755)

	// No config yet: it is created, private, with just this folder.
	if err := trustClaudeFolder(wt); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(cfg, ".claude.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	p := readClaudeCfg(t, cfg)
	real, _ := filepath.EvalSymlinks(wt)
	if string(p[real]["hasTrustDialogAccepted"]) != "true" || len(p) != 1 {
		t.Fatalf("projects = %v", p)
	}

	// An existing config keeps every other key, and the project's own.
	other := filepath.Join(t.TempDir(), "other")
	os.MkdirAll(other, 0o755)
	os.Remove(filepath.Join(cfg, ".claude.json"))
	os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(`{"userID":"u1","numStartups":7,"projects":{"/x":{"allowedTools":["a"],"hasTrustDialogAccepted":false},"`+real+`":{"history":[1]}}}`), 0o640)
	if err := trustClaudeFolder(wt); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cfg, ".claude.json"))
	var top map[string]json.RawMessage
	json.Unmarshal(b, &top)
	if string(top["userID"]) != `"u1"` || string(top["numStartups"]) != "7" {
		t.Errorf("top-level keys lost: %s", b)
	}
	p = readClaudeCfg(t, cfg)
	if string(p["/x"]["hasTrustDialogAccepted"]) != "false" || compact(p["/x"]["allowedTools"]) != `["a"]` {
		t.Errorf("another project changed: %v", p["/x"])
	}
	if string(p[real]["hasTrustDialogAccepted"]) != "true" || compact(p[real]["history"]) != "[1]" {
		t.Errorf("entry = %v", p[real])
	}
	if fi, _ := os.Stat(filepath.Join(cfg, ".claude.json")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}

	// Nothing but this folder is trusted: not its parent, not another.
	if _, ok := p[filepath.Dir(real)]; ok {
		t.Error("parent trusted")
	}

	// A config that is not an object is left untouched.
	os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(`[1,2`), 0o600)
	if err := trustClaudeFolder(wt); err == nil {
		t.Error("broken config accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(cfg, ".claude.json")); string(b) != `[1,2` {
		t.Errorf("broken config rewritten: %s", b)
	}
	if err := trustClaudeFolder("relative/dir"); err == nil {
		t.Error("relative path accepted")
	}
}

func TestWorkerPreTrustsClaudeWorktree(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	root, repo, wtRoot := workerTree(t)
	fh, client := startFakeLaunchHerdr(t)
	wts := []map[string]any{{"path": repo, "branch": "main", "is_linked_worktree": false, "label": "proj"}}
	fh.set(func(f *fakeLaunchHerdr) { f.hook = worktreeHook(t, &wts, nil) })
	s := newWorkerServer(t, client, root, wtRoot)

	// A pi worker's folder is not Claude's business.
	if code, r := startWorkerReq(t, s, `{"repo":"`+repo+`","branch":"a-pi","kind":"pi","prompt":"hi"}`); code != 200 {
		t.Fatalf("%d %v", code, r)
	}
	if _, err := os.Stat(filepath.Join(cfg, ".claude.json")); err == nil {
		t.Fatal("a pi worker touched the claude config")
	}

	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	code, r := startWorkerReq(t, s, `{"repo":"`+repo+`","branch":"fix/trust","kind":"claude","prompt":"the brief"}`)
	if code != 200 || r["prompt_error"] != nil || r["prompt_note"] != nil || r["warnings"] != nil {
		t.Fatalf("worker = %d %v", code, r)
	}
	want := filepath.Join(wtRoot, "proj", "fix-trust")
	p := readClaudeCfg(t, cfg)
	if len(p) != 1 || string(p[want]["hasTrustDialogAccepted"]) != "true" {
		t.Fatalf("trusted = %v, want only %s", p, want)
	}
	// The folder was trusted before claude was started, and the brief was
	// typed (not queued behind a dialog).
	ms := strings.Join(fh.methods(), ",")
	if !strings.Contains(ms, "agent.start") || !strings.HasSuffix(ms, "agent.prompt") {
		t.Fatalf("calls = %s", ms)
	}
	if got := fh.call("agent.prompt").Params["text"]; got != "the brief" {
		t.Fatalf("brief = %v", got)
	}
}
