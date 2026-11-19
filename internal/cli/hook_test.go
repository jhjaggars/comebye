package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hookAsk = "Built it.\n\n## Decisions for the user\n\n1. Keep the CSV format?\n2. Which compression, gzip or zstd?\n   1. gzip\n   2. zstd\n   Recommend: 2"

func runStop(t *testing.T, in map[string]any) string {
	t.Helper()
	data, _ := json.Marshal(in)
	var out bytes.Buffer
	e := &Env{Stdin: bytes.NewReader(data), Stdout: &out, Stderr: &bytes.Buffer{}}
	if err := cmdHook(e, []string{"stop"}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func writeTranscript(t *testing.T, rows ...string) string {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	rowPrompt = `{"type":"user","message":{"role":"user","content":"do the thing"}}`
	rowResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}`
	rowAdd    = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"comebye todo add \"q?\" --kind decision"}}]}}`
	rowOther  = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
)

func TestStopHookRemindsOnce(t *testing.T) {
	tr := writeTranscript(t, rowPrompt, rowOther, rowResult)
	out := runStop(t, map[string]any{"last_assistant_message": hookAsk, "transcript_path": tr})
	var got struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Decision != "block" {
		t.Fatalf("out = %q", out)
	}
	for _, want := range []string{"2 questions", "Keep the CSV format?", "comebye todo add", "gzip | zstd"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason lacks %q:\n%s", want, got.Reason)
		}
	}
	// The Stop that follows the reminder comes with stop_hook_active: silence.
	if out := runStop(t, map[string]any{"stop_hook_active": true, "last_assistant_message": hookAsk, "transcript_path": tr}); out != "" {
		t.Fatalf("second stop: %q", out)
	}
}

func TestStopHookSilentWhenThereIsNothingToFile(t *testing.T) {
	tr := writeTranscript(t, rowPrompt, rowAdd, rowResult)
	for name, in := range map[string]map[string]any{
		"filed in the turn": {"last_assistant_message": hookAsk, "transcript_path": tr},
		"no question":       {"last_assistant_message": "All done, tests pass.", "transcript_path": tr},
		"empty":             {},
	} {
		if out := runStop(t, in); out != "" {
			t.Errorf("%s: %q", name, out)
		}
	}
	// An item filed in an earlier turn does not count for this one.
	tr = writeTranscript(t, rowPrompt, rowAdd, rowResult, `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"next"}]}}`, rowOther)
	if out := runStop(t, map[string]any{"last_assistant_message": hookAsk, "transcript_path": tr}); !strings.Contains(out, "block") {
		t.Fatalf("a new turn needs its own: %q", out)
	}
	// Garbage in, silence out.
	var out bytes.Buffer
	if err := cmdHook(&Env{Stdin: strings.NewReader("not json"), Stdout: &out}, []string{"stop"}); err != nil || out.Len() != 0 {
		t.Fatalf("garbage: %v %q", err, out.String())
	}
}

func rowReport(cmd string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "tool_use", "name": "Bash", "input": map[string]string{"command": cmd}}}}})
	return string(b)
}

// A turn that ran `comebye report` with decisions gets no nag: the hub files
// them. A report with "None.", or a progress note, still gets it.
func TestStopHookSilentAfterReportWithDecisions(t *testing.T) {
	withDecisions := rowReport("comebye report - <<'EOF'\n## Summary\nx\n## Decisions for the user\n1. Keep CSV?\n   1. yes\n   2. no\nEOF")
	tr := writeTranscript(t, rowPrompt, withDecisions, rowResult)
	if out := runStop(t, map[string]any{"last_assistant_message": hookAsk, "transcript_path": tr}); out != "" {
		t.Fatalf("nagged after a report with decisions: %q", out)
	}
	for name, cmd := range map[string]string{
		"none":     "comebye report - <<'EOF'\n## Decisions for the user\nNone.\nEOF",
		"progress": "comebye report --progress \"## Decisions for the user\n1. x?\"",
		"empty":    "comebye report - <<'EOF'\n## Decisions for the user\n## Follow-ups\n1. x\nEOF",
	} {
		tr := writeTranscript(t, rowPrompt, rowReport(cmd), rowResult)
		if out := runStop(t, map[string]any{"last_assistant_message": hookAsk, "transcript_path": tr}); out == "" {
			t.Errorf("%s: no reminder", name)
		}
	}
	// A report from an earlier turn does not silence this one.
	tr = writeTranscript(t, withDecisions, rowPrompt, rowOther, rowResult)
	if out := runStop(t, map[string]any{"last_assistant_message": hookAsk, "transcript_path": tr}); out == "" {
		t.Error("earlier turn's report silenced the reminder")
	}
}
