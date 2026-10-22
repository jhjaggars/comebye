package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSession(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(join(lines...)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func join(lines ...string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

func TestReadPairsToolResultsAndSkipsNoise(t *testing.T) {
	path := writeSession(t,
		`{"type":"session","version":3,"id":"s1"}`,
		`{"type":"model_change","id":"m1"}`,
		`{"type":"message","id":"u1","timestamp":"2026-10-20T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"list files"}]}}`,
		`{"type":"message","id":"a1","timestamp":"2026-10-20T10:00:01Z","message":{"role":"assistant","model":"test-model","content":[{"type":"thinking","thinking":"should run ls"},{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"ls -la"}}]}}`,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"tc1","content":[{"type":"text","text":"file1\nfile2"}],"isError":false,"durationMs":1200}}`,
		`{"type":"custom_message","customType":"gate","content":"hidden","display":false,"id":"c1"}`,
		`{"type":"message","id":"a2","timestamp":"2026-10-20T10:00:05Z","message":{"role":"assistant","model":"test-model","content":[{"type":"text","text":"**Done.** Found `+"`2`"+` files."}]}}`,
	)

	tr, err := Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Total != 3 { // user, tool card, assistant text
		t.Fatalf("Total = %d, want 3 (msgs: %+v)", tr.Total, tr.Messages)
	}
	if tr.More {
		t.Error("More = true, want false")
	}

	kinds := []string{}
	for _, m := range tr.Messages {
		kinds = append(kinds, m.Kind)
	}
	want := []string{"user", "tool", "assistant"}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}

	tool := tr.Messages[1]
	if tool.Tool != "bash" || tool.Args != "ls -la" {
		t.Errorf("tool card = %+v", tool)
	}
	if tool.Result != "file1\nfile2" || tool.Ms != 1200 || tool.Err {
		t.Errorf("paired result missing: %+v", tool)
	}

	asst := tr.Messages[2]
	if asst.Model != "test-model" || asst.Text == "" {
		t.Errorf("assistant = %+v", asst)
	}
}

func TestReadPendingAsk(t *testing.T) {
	call := `{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"q1","name":"ask_user_question","arguments":{"questions":[{"question":"Which cache?","header":"Cache","options":[{"label":"LRU","description":"bounded"},{"label":"TTL","description":"expiring","preview":"ttl: 60s"}]},{"question":"Tests?","header":"Tests","multiSelect":true,"options":[{"label":"Unit","description":""},{"label":"E2E","description":""}]}]}}]}}`
	path := writeSession(t,
		`{"type":"message","id":"u1","message":{"role":"user","content":"add caching"}}`,
		call,
	)
	tr, err := Read(path, 1, 0) // the window does not hide the pending ask
	if err != nil {
		t.Fatal(err)
	}
	if tr.Ask == nil || tr.Ask.ToolCallID != "q1" || len(tr.Ask.Questions) != 2 {
		t.Fatalf("Ask = %+v", tr.Ask)
	}
	q0, q1 := tr.Ask.Questions[0], tr.Ask.Questions[1]
	if q0.Question != "Which cache?" || q0.Header != "Cache" || q0.MultiSelect || len(q0.Options) != 2 {
		t.Errorf("q0 = %+v", q0)
	}
	if q0.Options[0].Label != "LRU" || q0.Options[0].Description != "bounded" || q0.Options[0].HasPreview || !q0.Options[1].HasPreview {
		t.Errorf("q0 options = %+v", q0.Options)
	}
	if !q1.MultiSelect {
		t.Errorf("q1 = %+v", q1)
	}

	// Once answered, nothing is pending.
	path = writeSession(t, call,
		`{"type":"message","id":"r1","message":{"role":"toolResult","toolCallId":"q1","toolName":"ask_user_question","content":[{"type":"text","text":"User has answered"}]}}`,
	)
	if tr, err = Read(path, 100, 0); err != nil || tr.Ask != nil {
		t.Fatalf("answered: Ask = %+v, err %v", tr.Ask, err)
	}
}

func TestReadWindowAndSkip(t *testing.T) {
	lines := []string{`{"type":"session","version":3,"id":"s1"}`}
	for i := 0; i < 5; i++ {
		lines = append(lines,
			`{"type":"message","id":"u`+string(rune('a'+i))+`","message":{"role":"user","content":[{"type":"text","text":"m`+string(rune('a'+i))+`"}]}}`)
	}
	path := writeSession(t, lines...)

	// Last 2.
	tr, err := Read(path, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Messages) != 2 || tr.Messages[0].Text != "md" || tr.Messages[1].Text != "me" {
		t.Fatalf("limit window wrong: %+v", tr.Messages)
	}
	if !tr.More {
		t.Error("More = false, want true")
	}

	// Two before that.
	tr, err = Read(path, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Messages) != 2 || tr.Messages[0].Text != "mb" || tr.Messages[1].Text != "mc" {
		t.Fatalf("skip window wrong: %+v", tr.Messages)
	}
}

func TestReadMissingFile(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "nope.jsonl"), 10, 0)
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("ab", 3); got != "ab" {
		t.Errorf("truncate = %q", got)
	}
}

func TestSummarizeArgsPerTool(t *testing.T) {
	for _, tc := range []struct {
		tool, args, want string
	}{
		{"Bash", `{"command":"ls -la","description":"list"}`, "ls -la"},
		{"Agent", `{"description":"Fix the bug","subagent_type":"general-purpose","prompt":"A very long prompt"}`, "Fix the bug"},
		{"Task", `{"prompt":"only a prompt"}`, "only a prompt"},
		{"visible_subagent", `{"prompt":"long task text","name":"reviewer"}`, "reviewer"},
		{"spawn_agent", `{"task":"do it","name":"helper"}`, "helper"},
		{"fork_to_pane", `{"prompt":"p"}`, "p"},
		{"bash", `{"prompt":"p","name":"n"}`, "p"}, // other tools keep the default order
		{"x", `{"n":1}`, `{"n":1}`},
	} {
		if got := summarizeArgs(tc.tool, []byte(tc.args)); got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.tool, tc.args, got, tc.want)
		}
	}
}
