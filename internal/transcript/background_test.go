package transcript

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func bgLines(t *testing.T, recs ...string) *Latest {
	t.Helper()
	l, err := claudeLatestFrom(strings.NewReader(strings.Join(recs, "\n")+"\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func bgCall(id, name, input string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":"m%s","role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`, time.Now().UTC().Format(time.RFC3339Nano), id, id, name, input)
}

func bgResult(id, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`, time.Now().UTC().Format(time.RFC3339Nano), id, text)
}

func notif(id, status string) string {
	return fmt.Sprintf(`{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>%s</task-id>\n<status>%s</status>\n</task-notification>"}`, id, status)
}

func TestBackgroundBashOpenUntilNotified(t *testing.T) {
	launch := []string{
		bgCall("t1", "Bash", `{"command":"go test ./...","description":"full UI suite","run_in_background":true}`),
		bgResult("t1", "Command running in background with ID: bk1. Output is being written to: /tmp/x.output"),
		`{"type":"assistant","message":{"id":"m9","role":"assistant","content":[{"type":"text","text":"waiting for the suite"}]}}`,
	}
	l := bgLines(t, launch...)
	if len(l.Background) != 1 || l.Background[0].ID != "bk1" || l.Background[0].Tool != "Bash" || l.Background[0].Desc != "full UI suite" {
		t.Fatalf("open task not seen: %+v", l.Background)
	}
	if l.Text != "waiting for the suite" {
		t.Errorf("latest text lost: %q", l.Text)
	}
	l = bgLines(t, append(launch, notif("bk1", "completed"))...)
	if len(l.Background) != 0 {
		t.Errorf("a completed task is still listed: %+v", l.Background)
	}
}

func TestBackgroundKindsAndEnds(t *testing.T) {
	l := bgLines(t,
		bgCall("a", "Agent", `{"description":"research","prompt":"x","run_in_background":true}`),
		bgResult("a", "Async agent launched successfully.\nagentId: a1b2c3d4e5f6 (internal ID)"),
		bgCall("m", "Monitor", `{"command":"until x; do sleep 1; done","description":"wait for x"}`),
		bgResult("m", "Monitor started (task mon1, expires in 3m unless the source ends first)"),
		bgCall("s", "Bash", `{"command":"sleep 99","run_in_background":true}`),
		bgResult("s", "Command running in background with ID: sl1."),
		bgCall("k", "TaskStop", `{"task_id":"sl1"}`),
		bgResult("k", `{"message":"Successfully stopped task: sl1 (sleep 99)"}`),
		// a foreground Bash call and a failed launch are not tasks
		bgCall("f", "Bash", `{"command":"ls"}`),
		bgResult("f", "Command running in background with ID: nope"),
	)
	got := map[string]string{}
	for _, b := range l.Background {
		got[b.ID] = b.Tool
	}
	if len(got) != 2 || got["a1b2c3d4e5f6"] != "Agent" || got["mon1"] != "Monitor" {
		t.Errorf("tasks: %+v", l.Background)
	}
	l = bgLines(t,
		bgCall("m", "Monitor", `{"command":"x","description":"w"}`),
		bgResult("m", "Monitor started (task mon1, expires in 3m)"),
		notif("mon1", "failed"))
	if len(l.Background) != 0 {
		t.Errorf("a failed task is still listed: %+v", l.Background)
	}
}

func TestBackgroundNoticeInOtherRecordShapes(t *testing.T) {
	launch := []string{
		bgCall("t1", "Bash", `{"command":"x","run_in_background":true}`),
		bgResult("t1", "Command running in background with ID: bk2."),
	}
	user := `{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>bk2</task-id>\n<status>completed</status>\n</task-notification>"}}`
	att := `{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>\n<task-id>bk2</task-id>\n<status>completed</status>\n</task-notification>"}}`
	for name, rec := range map[string]string{"user": user, "attachment": att} {
		if l := bgLines(t, append(append([]string{}, launch...), rec)...); len(l.Background) != 0 {
			t.Errorf("%s notification did not end the task: %+v", name, l.Background)
		}
	}
}

func TestBackgroundOldTaskIgnored(t *testing.T) {
	old := time.Now().Add(-13 * time.Hour).UTC().Format(time.RFC3339Nano)
	l := bgLines(t,
		bgCall("t1", "Bash", `{"command":"x","run_in_background":true}`),
		fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"Command running in background with ID: old1."}]}}`, old))
	if len(l.Background) != 0 {
		t.Errorf("a task from 13h ago is believed: %+v", l.Background)
	}
}

func TestBackgroundMarksCommandAsLabel(t *testing.T) {
	l := bgLines(t,
		bgCall("a", "Bash", `{"command":"go test ./...","description":"suite","run_in_background":true}`),
		bgResult("a", "Command running in background with ID: d1."),
		bgCall("b", "Bash", `{"command":"gh run watch 9","run_in_background":true}`),
		bgResult("b", "Command running in background with ID: c1."))
	if len(l.Background) != 2 || l.Background[0].Cmd || !l.Background[1].Cmd {
		t.Errorf("Cmd flag: %+v", l.Background)
	}
}
