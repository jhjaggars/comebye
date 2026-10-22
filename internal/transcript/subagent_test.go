package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Records shaped like Claude Code 2.1's (trimmed; no real content).
const (
	sUser = `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"review the code"},"uuid":"u1","timestamp":"2026-10-20T10:00:00Z","sessionId":"s1"}`
	// Two background agents launched in parallel (one assistant message).
	sCallA   = `{"parentUuid":"u1","isSidechain":false,"type":"assistant","message":{"id":"msg1","model":"claude-test","role":"assistant","content":[{"type":"tool_use","id":"toolu_A","name":"Agent","input":{"description":"Review server","subagent_type":"general-purpose","isolation":"worktree","run_in_background":"true","prompt":"Repo: x. Review internal/server for bugs."},"caller":{"type":"direct"}}]},"uuid":"a1","timestamp":"2026-10-20T10:00:01Z","sessionId":"s1"}`
	sCallB   = `{"parentUuid":"a1","isSidechain":false,"type":"assistant","message":{"id":"msg1","model":"claude-test","role":"assistant","content":[{"type":"tool_use","id":"toolu_B","name":"Agent","input":{"description":"Review web","subagent_type":"Explore","run_in_background":true,"prompt":"Review web/app.js."},"caller":{"type":"direct"}}]},"uuid":"a2","timestamp":"2026-10-20T10:00:08Z","sessionId":"s1"}`
	sLaunchA = `{"parentUuid":"a2","isSidechain":false,"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_A","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully. (This tool result is internal metadata.)\nagentId: a0123456789abcdef (internal ID)\nThe agent is working in the background."}]}]},"uuid":"r1","timestamp":"2026-10-20T10:00:09Z","toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"a0123456789abcdef","description":"Review server","prompt":"Repo: x."},"sessionId":"s1"}`
	// No toolUseResult: the id comes from the launch text.
	sLaunchB = `{"parentUuid":"a2","isSidechain":false,"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_B","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully. (This tool result is internal metadata.)\nagentId: abbbbbbbbbbbbbbbb (internal ID)\nThe agent is working in the background."}]}]},"uuid":"r2","timestamp":"2026-10-20T10:00:09.500Z","sessionId":"s1"}`
	sSaid    = `{"parentUuid":"r2","isSidechain":false,"type":"assistant","message":{"id":"msg2","model":"claude-test","role":"assistant","content":[{"type":"text","text":"Two reviewers are running."}]},"uuid":"a3","timestamp":"2026-10-20T10:00:10Z","sessionId":"s1"}`
	// A's report, then its completion notice while the parent is idle.
	sHandbackA = `{"parentUuid":"a3","isSidechain":false,"type":"user","message":{"role":"user","content":"Another Claude session sent a message:\n<agent-message from=\"a0123456789abcdef\">\n[Subagent hand-back] ...\n  **Server**: two bugs.\n</agent-message>"},"isMeta":true,"uuid":"h1","timestamp":"2026-10-20T10:03:00Z","origin":{"kind":"peer","from":"a0123456789abcdef","senderTaskId":"a0123456789abcdef","name":"general-purpose","body":"[Subagent hand-back] ...","handback":true,"handbackReportText":"**Server**: two bugs."},"sessionId":"s1"}`
	sNotifyA   = `{"parentUuid":"h1","isSidechain":false,"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>a0123456789abcdef</task-id>\n<tool-use-id>toolu_A</tool-use-id>\n<output-file>/tmp/x.output</output-file>\n<status>completed</status>\n<summary>Agent \"Review server\" finished</summary>\n<result>This agent's report was delivered to you as a message from \"a0123456789abcdef\" (its SubagentHandback call).</result>\n<usage><subagent_tokens>72435</subagent_tokens><tool_uses>39</tool_uses><duration_ms>172575</duration_ms></usage>\n</task-notification>"},"uuid":"n1","timestamp":"2026-10-20T10:03:01Z","origin":{"kind":"task-notification","producer":"session-task"},"sessionId":"s1"}`
	// B's report and failure notice, delivered mid-turn as attachments.
	sHandbackB = `{"parentUuid":"n1","isSidechain":false,"type":"attachment","attachment":{"type":"queued_command","prompt":"<agent-message from=\"abbbbbbbbbbbbbbbb\">\n[Subagent hand-back] ...</agent-message>","commandMode":"prompt","origin":{"kind":"peer","from":"abbbbbbbbbbbbbbbb","handback":true,"handbackReportText":"Web: could not finish."},"isMeta":true},"uuid":"h2","timestamp":"2026-10-20T10:04:00Z","sessionId":"s1"}`
	sNotifyB   = `{"parentUuid":"h2","isSidechain":false,"type":"attachment","attachment":{"type":"queued_command","prompt":"<task-notification>\n<task-id>abbbbbbbbbbbbbbbb</task-id>\n<tool-use-id>toolu_B</tool-use-id>\n<status>failed</status>\n<summary>Agent \"Review web\" failed</summary>\n</task-notification>","commandMode":"task-notification","origin":{"kind":"task-notification","producer":"session-task"},"usage":{"totalTokens":5000,"toolUses":7,"durationMs":61000}},"uuid":"n2","timestamp":"2026-10-20T10:04:01Z","sessionId":"s1"}`
	// A was resumed and notified again.
	sNotifyA2 = `{"parentUuid":"n2","isSidechain":false,"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>a0123456789abcdef</task-id>\n<tool-use-id>toolu_A</tool-use-id>\n<status>completed</status>\n<summary>Agent \"Review server\" finished</summary>\n<usage><subagent_tokens>90000</subagent_tokens><tool_uses>45</tool_uses><duration_ms>200000</duration_ms></usage>\n</task-notification>"},"uuid":"n3","timestamp":"2026-10-20T10:06:00Z","sessionId":"s1"}`
	// A background shell task's notice has no agent card: it stays a note.
	sNotifyBash = `{"parentUuid":"n3","isSidechain":false,"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>bq1w2e3</task-id>\n<tool-use-id>toolu_bash</tool-use-id>\n<status>completed</status>\n<summary>Background command \"make\" completed</summary>\n</task-notification>"},"uuid":"n4","timestamp":"2026-10-20T10:06:30Z","sessionId":"s1"}`
	// A synchronous agent: its result is the report.
	sCallC   = `{"parentUuid":"n4","isSidechain":false,"type":"assistant","message":{"id":"msg3","model":"claude-test","role":"assistant","content":[{"type":"tool_use","id":"toolu_C","name":"Task","input":{"description":"Find callers","subagent_type":"Explore","prompt":"Find callers of Read."}}]},"uuid":"a4","timestamp":"2026-10-20T10:07:00Z","sessionId":"s1"}`
	sResultC = `{"parentUuid":"a4","isSidechain":false,"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_C","type":"tool_result","content":[{"type":"text","text":"Callers: server.go:293.\nagentId: accccccccc (for resuming to continue this agent's work if needed)"}]}]},"uuid":"r3","timestamp":"2026-10-20T10:07:30Z","toolUseResult":{"status":"completed","agentId":"accccccccc","content":[{"type":"text","text":"Callers: server.go:293."}],"totalDurationMs":29000,"totalTokens":1234,"totalToolUseCount":5},"sessionId":"s1"}`
	// A synchronous agent still running.
	sCallD = `{"parentUuid":"r3","isSidechain":false,"type":"assistant","message":{"id":"msg4","model":"claude-test","role":"assistant","content":[{"type":"tool_use","id":"toolu_D","name":"Agent","input":{"description":"Write tests","prompt":"Write tests."}}]},"uuid":"a5","timestamp":"2026-10-20T10:08:00Z","sessionId":"s1"}`
)

func byID(t *testing.T, tr *Transcript, id string) Message {
	t.Helper()
	for _, m := range tr.Messages {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("no message %s in %+v", id, tr.Messages)
	return Message{}
}

func runningIDs(tr *Transcript) string {
	var ids []string
	for _, r := range tr.RunningSubagents {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, ",")
}

func TestClaudeSubagents(t *testing.T) {
	path := writeSession(t, sUser, sCallA, sCallB, sLaunchA, sLaunchB, sSaid)
	tr, err := Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := byID(t, tr, "toolu_A")
	if a.Args != "Review server" || a.Result != "" || a.Time == "" || a.Subagent == nil {
		t.Fatalf("card A %+v", a)
	}
	sa := *a.Subagent
	if sa.AgentID != "a0123456789abcdef" || sa.Type != "general-purpose" || !sa.Background || !sa.Worktree ||
		sa.Status != "running" || !strings.HasPrefix(sa.Prompt, "Repo: x.") || sa.Report != "" {
		t.Fatalf("subagent A %+v", sa)
	}
	// Measured from when the message's last call was written, not from
	// A's own block 7s earlier.
	if a.Ms != 1000 {
		t.Errorf("A ms = %d, want 1000", a.Ms)
	}
	b := byID(t, tr, "toolu_B")
	if b.Subagent.AgentID != "abbbbbbbbbbbbbbbb" || b.Subagent.Worktree || !b.Subagent.Background || b.Result != "" {
		t.Fatalf("subagent B %+v", b.Subagent)
	}
	if got := runningIDs(tr); got != "toolu_A,toolu_B" {
		t.Fatalf("running = %s", got)
	}
	held := a.Subagent // handed out: must not change under later reads

	appendLines(t, path, sHandbackA, sNotifyA, sHandbackB, sNotifyB)
	if tr, err = Read(path, 100, 0); err != nil {
		t.Fatal(err)
	}
	sa = *byID(t, tr, "toolu_A").Subagent
	if sa.Status != "completed" || sa.Report != "**Server**: two bugs." || sa.Tokens != 72435 || sa.ToolUses != 39 || sa.DurMs != 172575 {
		t.Fatalf("A after notify %+v", sa)
	}
	sb := *byID(t, tr, "toolu_B").Subagent
	if sb.Status != "failed" || sb.Report != "Web: could not finish." || sb.Tokens != 5000 || sb.ToolUses != 7 || sb.DurMs != 61000 {
		t.Fatalf("B after notify %+v", sb)
	}
	if held.Status != "running" || held.Report != "" {
		t.Fatalf("a handed-out SubagentInfo changed: %+v", held)
	}
	if got := runningIDs(tr); got != "" {
		t.Fatalf("running = %s", got)
	}
	// Linked notifications and hand-backs are not shown on their own.
	for _, m := range tr.Messages {
		if m.Kind != "tool" && m.Kind != "user" && m.Kind != "assistant" {
			t.Fatalf("unexpected %+v", m)
		}
	}

	appendLines(t, path, sNotifyA2, sNotifyBash, sCallC, sResultC, sCallD)
	if tr, err = Read(path, 100, 0); err != nil {
		t.Fatal(err)
	}
	sa = *byID(t, tr, "toolu_A").Subagent
	if sa.Tokens != 90000 || sa.ToolUses != 45 || sa.DurMs != 200000 || sa.Report != "**Server**: two bugs." {
		t.Fatalf("A after second notify %+v", sa)
	}
	if n := byID(t, tr, "n4"); n.Kind != "note" || !strings.Contains(n.Note, "make") {
		t.Fatalf("bash notification %+v", n)
	}
	c := byID(t, tr, "toolu_C")
	if c.Tool != "Task" || c.Args != "Find callers" || c.Result != "" || c.Ms != 30000 {
		t.Fatalf("card C %+v", c)
	}
	if sc := *c.Subagent; sc.Status != "completed" || sc.Background || sc.Report != "Callers: server.go:293." ||
		sc.AgentID != "accccccccc" || sc.DurMs != 29000 || sc.Tokens != 1234 || sc.ToolUses != 5 {
		t.Fatalf("subagent C %+v", sc)
	}
	if got := runningIDs(tr); got != "toolu_D" {
		t.Fatalf("running = %s", got)
	}
}

// A synchronous agent's result without toolUseResult: the text minus the
// resume footer is the report. An errored call fails the card.
func TestClaudeSubagentSyncFallbacks(t *testing.T) {
	path := writeSession(t, sUser,
		`{"type":"assistant","isSidechain":false,"message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"description":"d1","prompt":"p"}},{"type":"tool_use","id":"t2","name":"Agent","input":{"description":"d2","prompt":"p"}}]},"uuid":"a1","timestamp":"2026-10-20T10:00:01Z"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"All good.\nagentId: a1234567 (for resuming)"}]},"uuid":"r1","timestamp":"2026-10-20T10:00:05Z"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"tool_use_id":"t2","type":"tool_result","content":"Agent type 'nope' not found","is_error":true}]},"uuid":"r2","timestamp":"2026-10-20T10:00:05Z"}`,
	)
	tr, err := Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s := byID(t, tr, "t1").Subagent; s.Status != "completed" || s.Report != "All good." {
		t.Fatalf("t1 %+v", s)
	}
	t2 := byID(t, tr, "t2")
	if t2.Subagent.Status != "failed" || !t2.Err || !strings.Contains(t2.Result, "not found") {
		t.Fatalf("t2 %+v %+v", t2, t2.Subagent)
	}
	if len(tr.RunningSubagents) != 0 {
		t.Fatalf("running %+v", tr.RunningSubagents)
	}
}

// Pi sessions are untouched: no subagent cards, Pi tool names preview by
// name.
func TestPiSubagentToolsUnaffected(t *testing.T) {
	path := writeSession(t,
		`{"type":"session","version":3,"id":"s1"}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"visible_subagent","arguments":{"name":"reviewer","prompt":"a long task"}},{"type":"toolCall","id":"tc2","name":"Agent","arguments":{"description":"d","prompt":"p"}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"tc1","content":[{"type":"text","text":"started"}],"durationMs":50}}`,
	)
	tr, err := Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Format != FormatPi || len(tr.Messages) != 2 || len(tr.RunningSubagents) != 0 {
		t.Fatalf("%+v", tr)
	}
	if m := tr.Messages[0]; m.Args != "reviewer" || m.Result != "started" || m.Subagent != nil {
		t.Fatalf("pi card %+v", m)
	}
	if m := tr.Messages[1]; m.Args != "d" || m.Subagent != nil {
		t.Fatalf("pi Agent card %+v", m)
	}
}

func TestSubagentPathAndRead(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s1.jsonl")
	if err := os.WriteFile(session, []byte(sUser+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subs := filepath.Join(dir, "s1", "subagents")
	if err := os.MkdirAll(subs, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"parentUuid":null,"isSidechain":true,"agentId":"a0123456789abcdef","type":"user","message":{"role":"user","content":"Review internal/server."},"uuid":"s1","timestamp":"2026-10-20T10:00:01Z"}`,
		`{"parentUuid":"s1","isSidechain":true,"agentId":"a0123456789abcdef","type":"user","message":{"role":"user","content":"<system-reminder>hand back</system-reminder>"},"isMeta":true,"uuid":"s2","timestamp":"2026-10-20T10:00:01Z"}`,
		`{"parentUuid":"s2","isSidechain":true,"agentId":"a0123456789abcdef","type":"assistant","message":{"id":"m1","model":"claude-test","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/server.go"}}]},"uuid":"s3","timestamp":"2026-10-20T10:00:02Z"}`,
		`{"parentUuid":"s3","isSidechain":true,"agentId":"a0123456789abcdef","type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"package server"}]},"uuid":"s4","timestamp":"2026-10-20T10:00:03Z"}`,
		`{"parentUuid":"s4","isSidechain":true,"agentId":"a0123456789abcdef","type":"assistant","message":{"id":"m2","model":"claude-test","role":"assistant","content":[{"type":"tool_use","id":"t2","name":"SubagentHandback","input":{"message":"**Server**: two bugs."}}]},"uuid":"s5","timestamp":"2026-10-20T10:00:04Z"}`,
		`{"parentUuid":"s5","isSidechain":true,"agentId":"a0123456789abcdef","type":"user","message":{"role":"user","content":[{"tool_use_id":"t2","type":"tool_result","content":"{\"success\":true}"}]},"uuid":"s6","timestamp":"2026-10-20T10:00:05Z"}`,
	}
	if err := os.WriteFile(filepath.Join(subs, "agent-a0123456789abcdef.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(subs, "agent-addddddd.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(session, filepath.Join(subs, "agent-aeeeeeee.jsonl")); err != nil {
		t.Fatal(err)
	}

	p, err := SubagentPath(session, "a0123456789abcdef")
	if err != nil || p != filepath.Join(subs, "agent-a0123456789abcdef.jsonl") {
		t.Fatalf("path %q %v", p, err)
	}
	for _, id := range []string{"", "a", "a123", "../a1234567", "a1234567/../../x", "A0123456", "b0123456", "a0123456789abcdef0123456789abcdef0", "a0123456\x00", "a9999999", "addddddd", "aeeeeeee"} {
		if p, err := SubagentPath(session, id); err == nil {
			t.Errorf("SubagentPath(%q) = %q, want error", id, p)
		}
	}
	if _, err := SubagentPath(filepath.Join(dir, "s1"), "a0123456789abcdef"); err == nil {
		t.Error("a session path without .jsonl must be refused")
	}

	tr, err := ReadSubagent(p, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, m := range tr.Messages {
		kinds = append(kinds, m.Kind)
	}
	if got := strings.Join(kinds, ","); got != "user,tool,assistant" {
		t.Fatalf("kinds %s (%+v)", got, tr.Messages)
	}
	if m := tr.Messages[1]; m.Tool != "Read" || m.Result != "package server" {
		t.Fatalf("tool %+v", m)
	}
	if m := tr.Messages[2]; m.Text != "**Server**: two bugs." || m.ID != "t2" {
		t.Fatalf("hand-back %+v", m)
	}
	// The main transcript reader still ignores sidechain records.
	if tr, err := Read(session, 100, 0); err != nil || len(tr.Messages) != 1 {
		t.Fatalf("session %+v %v", tr, err)
	}
}
