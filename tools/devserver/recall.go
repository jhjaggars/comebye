package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/archive"
)

// A transcript index for the dev harness, made by the real pipeline (archive
// run, then index) from synthetic sessions, so that no real transcript is ever
// needed to look at the Transcripts screen. The secret-looking values are made up.

type fixSession struct {
	lines []string
	seq   int
	at    time.Time
	sid   string
	cwd   string
	br    string
}

func newFixClaude(sid, cwd, branch, title string, at time.Time) *fixSession {
	s := &fixSession{sid: sid, cwd: cwd, br: branch, at: at}
	s.line(map[string]any{"type": "ai-title", "sessionId": sid, "aiTitle": title})
	return s
}

func (s *fixSession) line(v map[string]any) {
	b, _ := json.Marshal(v)
	s.lines = append(s.lines, string(b))
}

func (s *fixSession) msg(typ string, content any) {
	s.at = s.at.Add(20 * time.Second)
	s.line(map[string]any{"type": typ, "sessionId": s.sid, "cwd": s.cwd, "gitBranch": s.br, "timestamp": s.at.Format(time.RFC3339),
		"message": map[string]any{"role": typ, "content": content}})
}

func (s *fixSession) user(t string) { s.msg("user", t) }
func (s *fixSession) say(t string) {
	s.msg("assistant", []map[string]any{{"type": "text", "text": t}})
}

// tool adds a call and its result.
func (s *fixSession) tool(name string, input map[string]any, out string, isErr bool) {
	s.seq++
	id := fmt.Sprintf("toolu_%04d", s.seq)
	s.msg("assistant", []map[string]any{{"type": "tool_use", "id": id, "name": name, "input": input}})
	s.msg("user", []map[string]any{{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": out}})
}

func (s *fixSession) write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(s.lines, "\n")+"\n"), 0o644)
}

func writeRecallFixture(index string) error {
	dir, err := os.MkdirTemp("", "recall-fixture")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	claudeDir, piDir := filepath.Join(dir, "claude"), filepath.Join(dir, "pi")
	workerPi := filepath.Join(dir, "worker-pi")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return err
	}
	base := time.Now().UTC().Add(-26 * time.Hour).Truncate(time.Second)

	smokeID := "11111111-aaaa-4aaa-8aaa-111111111111"
	smokeCwd := "/home/dev/.worktrees/comebye/fix-smoke-retry"
	smoke := newFixClaude(smokeID, smokeCwd, "fix/smoke-retry", "Retry the upgrade smoke check", base)
	smoke.user("Make the upgrade smoke check tolerate a server that is still restarting.")
	smoke.say("The smoke check fails only right after a restart. I'll **reproduce** it, then add a backoff.\n\n- first the test\n- then the fix\n\n`comebye upgrade --smoke-only` shows it.")
	smoke.tool("Bash", map[string]any{"command": "go test ./internal/upgrade/"}, "ok  \tgithub.com/example/comebye/internal/upgrade\t0.412s", false)
	smoke.tool("Read", map[string]any{"file_path": "internal/upgrade/smoke.go"}, "package upgrade\n\nfunc smoke() error { return nil }", false)
	smoke.tool("Bash", map[string]any{"command": "comebye upgrade --smoke-only"}, "smoke: GET /api/version\nsmoke: dial tcp 10.0.0.7:443: connect: connection refused\nsmoke: failed after 1 attempt", true)
	smoke.tool("Bash", map[string]any{"command": "curl -s -H 'Authorization: Bearer " + "fixture" + strings.Repeat("Tok3n", 6) + "' https://example.test/api/version"}, "Authorization: Bearer "+"fixture"+strings.Repeat("Tok3n", 6)+"\nhub: 503 upstream connect error\ntoken=ghp_"+strings.Repeat("x1", 18), false)
	smoke.say("The output above held a credential; it will read as masked here.")
	long := "journal: starting the restart loop\n" + strings.Repeat("journal: waiting for the listener to come back on port 443 before retrying\n", 80) + "journal: gave up: connection refused after the last attempt"
	smoke.tool("Bash", map[string]any{"command": "journalctl --user -u comebye -n 400"}, long, false)
	smoke.tool("Bash", map[string]any{"command": "printf '<img src=x onerror=window.__xss=1>'"}, `<img src=x onerror=window.__xss=1><script>window.__xss=2</script><b>not bold</b>`, false)
	smoke.say(`Rendered text can hold tags too: <img src=x onerror="window.__xss=3"> and <script>window.__xss=4</script>. Confirmed: it needs a retry, so I'm adding three attempts with backoff.`)
	smoke.tool("Edit", map[string]any{"file_path": "internal/upgrade/smoke.go"}, "The file has been updated.", false)
	smoke.tool("Bash", map[string]any{"command": "git commit -am 'upgrade: retry the smoke check'"}, "[fix/smoke-retry 1a2b3c4d] upgrade: retry the smoke check's transient failures\n 1 file changed, 12 insertions(+)", false)
	smoke.say("Done: three attempts with backoff.")
	// What the session produced besides the commit: a Page, an issue and a PR.
	smoke.tool("Bash", map[string]any{"command": "comebye publish smoke-retry.html --key smoke-retry-writeup"}, "Published pg_5a0e7e1b2c (html, 3.1 KiB).\nhttps://devbox.example.ts.net/p/pg_5a0e7e1b2c", false)
	smoke.tool("Bash", map[string]any{"command": "gh issue create --repo acme/comebye-fixture --title \"Smoke check flakes right after a restart\" --body-file issue.md"}, "https://github.com/acme/comebye-fixture/issues/41\n", false)
	smoke.tool("Bash", map[string]any{"command": "gh pr create --repo acme/comebye-fixture --head fix/smoke-retry --title \"upgrade: retry the smoke check\" --body-file pr.md"}, "https://github.com/acme/comebye-fixture/pull/42\n", false)
	smokeDir := filepath.Join(claudeDir, "-home-dev-.worktrees-comebye-fix-smoke-retry")
	if err := smoke.write(filepath.Join(smokeDir, smokeID+".jsonl")); err != nil {
		return err
	}
	sub := &fixSession{sid: smokeID, cwd: smokeCwd, br: "fix/smoke-retry", at: base.Add(3 * time.Minute)}
	sub.user("Check how often the listener takes more than a second to come back.")
	sub.tool("Bash", map[string]any{"command": "grep -c 'connection refused' journal.log"}, "7", false)
	sub.say("It takes more than a second in 7 of 40 restarts, each ending in connection refused.")
	if err := sub.write(filepath.Join(smokeDir, smokeID, "subagents", "agent-abc123def.jsonl")); err != nil {
		return err
	}

	longID := "22222222-bbbb-4bbb-8bbb-222222222222"
	lng := newFixClaude(longID, "/home/dev/code/homelab", "main", "Long session: bring up the archive Ingress", base.Add(2*time.Hour))
	lng.user("Bring up the archive Ingress.")
	for i := 1; i <= 160; i++ {
		out := fmt.Sprintf("step %d: ok", i)
		if i%20 == 0 {
			out = fmt.Sprintf("step %d: curl: (7) Failed to connect to archive port 443: Connection refused", i)
		}
		lng.tool("Bash", map[string]any{"command": fmt.Sprintf("kubectl get ingress archive # check %d", i)}, out, false)
	}
	lng.say("The Ingress is up.")
	if err := lng.write(filepath.Join(claudeDir, "-home-dev-code-homelab", longID+".jsonl")); err != nil {
		return err
	}

	piID := "33333333-cccc-4ccc-8ccc-333333333333"
	piLines := []map[string]any{
		{"type": "session", "version": 3, "id": piID, "timestamp": base.Add(5 * time.Hour).Format(time.RFC3339), "cwd": "/data/worktrees/comebye/feat-worker"},
		{"type": "message", "timestamp": base.Add(5 * time.Hour).Format(time.RFC3339), "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "Provision a spot worker."}}}},
		{"type": "message", "timestamp": base.Add(5 * time.Hour).Format(time.RFC3339), "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "toolCall", "id": "call1", "name": "bash", "arguments": map[string]any{"command": "ssh 10.0.0.9 true"}}}}},
		{"type": "message", "timestamp": base.Add(5 * time.Hour).Format(time.RFC3339), "message": map[string]any{"role": "toolResult", "toolCallId": "call1", "isError": true, "content": []map[string]any{{"type": "text", "text": "ssh: connect to host 10.0.0.9 port 22: Connection refused"}}}},
		{"type": "message", "timestamp": base.Add(5 * time.Hour).Format(time.RFC3339), "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "The VM was still booting; the retry worked."}}}},
	}
	var sb strings.Builder
	for _, l := range piLines {
		b, _ := json.Marshal(l)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	piPath := filepath.Join(workerPi, "--data-worktrees-comebye-feat-worker--", "2026-10-03T10-00-00-000Z_"+piID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(piPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(piPath, []byte(sb.String()), 0o644); err != nil {
		return err
	}

	cfg := archive.Config{Encryption: archive.ModeNone, Dir: filepath.Join(dir, "store"), ClaudeDir: claudeDir, PiDir: piDir, Index: index, Host: "devbox"}
	a, err := cfg.Open()
	if err != nil {
		return err
	}
	if _, err := a.RunFinal(cfg.Sources()); err != nil {
		return err
	}
	wcfg := cfg
	wcfg.Host, wcfg.ClaudeDir, wcfg.PiDir = archive.WorkerHost("w-spot-3", "i-0abc123"), filepath.Join(dir, "none"), workerPi
	wa, err := wcfg.Open()
	if err != nil {
		return err
	}
	if err := wa.ClaimWorker(archive.WorkerInfo{Name: "w-spot-3", Provider: "aws", InstanceID: "i-0abc123", Created: base}); err != nil {
		return err
	}
	if _, err := wa.RunFinal(wcfg.Sources()); err != nil {
		return err
	}
	o, err := cfg.OpenIdentity()
	if err != nil {
		return err
	}
	links := []archive.Link{{ID: "h-fixture1", Host: "devbox", AgentID: "devbox/w3:p1", RepoName: "comebye", Branch: "fix/smoke-retry", Worktree: smokeCwd,
		Brief: "Make the upgrade smoke check tolerate a server that is still restarting. Retry transient failures with backoff.", CreatedAt: base.Format(time.RFC3339),
		Status: "finished", FinalReport: "## Summary\nThree attempts with backoff. A live restart passes on the second attempt.", ReportSource: "explicit"}}
	ix := &archive.Index{Path: index}
	st, err := ix.Sync(a, o, links, archive.SyncOptions{})
	if err != nil {
		return err
	}
	if len(st.Errors) > 0 {
		return fmt.Errorf("indexing the fixture: %v", st.Errors)
	}
	return nil
}
