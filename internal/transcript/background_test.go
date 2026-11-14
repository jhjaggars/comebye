package transcript

import (
	"fmt"
	"os"
	"regexp"
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

// fixture reads a trimmed excerpt of a real Claude Code session
// (testdata/*.jsonl, paths and commands scrubbed). Timestamps are moved to
// now so the age limit does not retire the task as the file gets old.
func fixture(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	re := regexp.MustCompile(`"timestamp": "[^"]*"`)
	return strings.Split(strings.TrimSpace(re.ReplaceAllString(string(b), `"timestamp": "`+now+`"`)), "\n")
}

// A pre-landing run watched by a Monitor, as in a real session: open after
// the launch, closed by the notice that the stream ended.
func TestBackgroundRealMonitorOpenThenStreamEnded(t *testing.T) {
	open := fixture(t, "monitor_open.jsonl")
	l := bgLines(t, open...)
	if len(l.Background) != 1 || l.Background[0].Tool != "Monitor" || l.Background[0].Desc != "Wait for prelanding.sh" {
		t.Fatalf("watch not seen: %+v", l.Background)
	}
	l = bgLines(t, append(open, fixture(t, "monitor_stream_ended.jsonl")...)...)
	if len(l.Background) != 0 {
		t.Errorf("a Monitor whose stream ended is still listed: %+v", l.Background)
	}
}

// A Monitor that runs out its timeout is announced with no <status>; it must
// not stay "running" for hours.
func TestBackgroundRealMonitorExpired(t *testing.T) {
	rec := fixture(t, "monitor_expired.jsonl")
	l := bgLines(t, rec[:2]...)
	if len(l.Background) != 1 {
		t.Fatalf("watch not seen before it expired: %+v", l.Background)
	}
	l = bgLines(t, rec...)
	if len(l.Background) != 0 {
		t.Errorf("an expired Monitor is still listed: %+v", l.Background)
	}
}

// A mid-watch event is not the end of the watch.
func TestBackgroundMonitorEventKeepsWatch(t *testing.T) {
	ev := `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>mon1</task-id>\n<summary>Monitor event: \"w\"</summary>\n<event>ssh not ready</event>\n</task-notification>"}`
	l := bgLines(t,
		bgCall("m", "Monitor", `{"command":"x","description":"w"}`),
		bgResult("m", "Monitor started (task mon1, expires in 3m)"), ev)
	if len(l.Background) != 1 {
		t.Errorf("an event ended the watch: %+v", l.Background)
	}
}

// A background agent that stopped with work of its own still running sends
// "completed" yet is not over (its task-id notifies again).
func TestBackgroundAgentWaitingOnItsOwnWorkStaysOpen(t *testing.T) {
	launch := []string{
		bgCall("a", "Agent", `{"description":"scale","prompt":"x","run_in_background":true}`),
		bgResult("a", "Async agent launched successfully.\nagentId: a2e2fde92b124ad6b (internal ID)"),
	}
	waiting := `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>a2e2fde92b124ad6b</task-id>\n<status>completed</status>\n<summary>Agent \"scale\" is waiting on its own background work</summary>\n<note>This agent stopped with background work of its own still running.</note>\n</task-notification>"}`
	if l := bgLines(t, append(append([]string{}, launch...), waiting)...); len(l.Background) != 1 {
		t.Errorf("an agent waiting on its own work was closed: %+v", l.Background)
	}
	done := strings.Replace(waiting, " is waiting on its own background work", " finished", 1)
	if l := bgLines(t, append(append([]string{}, launch...), waiting, done)...); len(l.Background) != 0 {
		t.Errorf("a finished agent is still listed: %+v", l.Background)
	}
}
