package transcript

// Background tasks of a Claude Code session. herdr reports only the pane's
// status (working, idle, done, blocked), and Claude Code shows "idle" once a
// turn ends even when a task it started with run_in_background (a Bash
// command, a background Agent, a Monitor) is still running: the session
// wakes when the task finishes. The session file is the only place that
// says so:
//
//   - launch: the tool_result of the call says "Command running in
//     background with ID: <id>" (Bash), "Async agent launched ... agentId:
//     <id>" (Agent/Task) or "Monitor started (task <id>, ..." (Monitor); a
//     foreground Bash call that outruns its timeout is moved to the
//     background, and its result says "... was moved to the background
//     (ID: <id>)";
//   - a ScheduleWakeup (without stop) is a wait too: the session wakes when
//     its delay runs out, so it is listed (Tool "Wakeup") until then, or
//     until a later ScheduleWakeup replaces or stops it;
//   - end: a <task-notification> naming <task-id> and a terminal <status>
//     (completed, failed, killed, stopped), as a queue-operation record, an
//     attachment (mid-turn) or a user record; or TaskStop's "Successfully
//     stopped task: <id>".
//
// A task still open after the tail of the file is read is reported. Limits,
// both of which err towards reporting nothing: a task launched before the
// window read (the last tailWindow bytes) is not seen, and a task whose
// session process was restarted never notifies, so tasks older than
// maxBackgroundAge are ignored.

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// BackgroundTask is a task a session started in the background and has not
// seen end.
type BackgroundTask struct {
	ID    string `json:"id"`
	Tool  string `json:"tool"` // Bash, Agent, Monitor or Wakeup
	Desc  string `json:"desc,omitempty"`
	Since string `json:"since,omitempty"`
	// Cmd: Desc is the command line (the call had no description), which
	// can hold secrets; show only a short label made from it.
	Cmd bool `json:"cmd,omitempty"`
}

// maxBackgroundAge is how long an unfinished task is believed.
const maxBackgroundAge = 12 * time.Hour

var (
	bgBashRE    = regexp.MustCompile(`Command running in background with ID:\s*([A-Za-z0-9_-]+)`)
	bgMovedRE   = regexp.MustCompile(`moved to the background \(ID:\s*([A-Za-z0-9_-]+)\)`)
	bgMonitorRE = regexp.MustCompile(`Monitor started \(task ([A-Za-z0-9_-]+)`)
	bgAgentRE   = regexp.MustCompile(`agentId:\s*([A-Za-z0-9_-]+)`)
	bgStoppedRE = regexp.MustCompile(`Successfully stopped task:\s*([A-Za-z0-9_-]+)`)
	taskIDRE    = regexp.MustCompile(`<task-id>\s*([A-Za-z0-9_-]+)\s*</task-id>`)
	taskStateRE = regexp.MustCompile(`<status>\s*([a-z]+)\s*</status>`)
)

type bgLaunch struct {
	tool, desc string
	cmd        bool
	// fg: a Bash call not run in the background, which starts a task only
	// if it is moved there after its timeout.
	fg bool
}

// bgTracker follows launches and endings through a stream of records.
type bgTracker struct {
	calls map[string]bgLaunch // tool_use id -> a call that may start a task
	open  map[string]BackgroundTask
	order []string
	// wake is the pending ScheduleWakeup, if any, and wakeAt when it fires.
	wake   *BackgroundTask
	wakeAt time.Time
}

func newBgTracker() *bgTracker {
	return &bgTracker{calls: map[string]bgLaunch{}, open: map[string]BackgroundTask{}}
}

// call notes an assistant tool_use that can start a background task.
func (t *bgTracker) call(id, name string, input json.RawMessage) {
	if id == "" {
		return
	}
	var in struct {
		Background  any    `json:"run_in_background"`
		Description string `json:"description"`
		Command     string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	bg := false
	switch v := in.Background.(type) {
	case bool:
		bg = v
	case string:
		bg = v == "true"
	}
	if !bg && name != "Monitor" && name != "Bash" {
		return // only a Bash call can be moved to the background later
	}
	tool := name
	if isAgentTool(name) {
		tool = "Agent"
	}
	desc := strings.TrimSpace(in.Description)
	cmd := false
	if desc == "" {
		desc, cmd = strings.TrimSpace(in.Command), true
	}
	t.calls[id] = bgLaunch{tool: tool, desc: Excerpt(desc, 120), cmd: cmd, fg: !bg && name == "Bash"}
	if len(t.calls) > 200 { // keep the map small on a long window
		for k := range t.calls {
			delete(t.calls, k)
			break
		}
	}
}

// wakeup notes a ScheduleWakeup call made at at: the session waits for it
// until its delay runs out. A later call replaces it; one with stop ends it.
func (t *bgTracker) wakeup(id, at string, input json.RawMessage) {
	var in struct {
		Stop         bool    `json:"stop"`
		DelaySeconds float64 `json:"delaySeconds"`
		Reason       string  `json:"reason"`
	}
	_ = json.Unmarshal(input, &in)
	ts, err := time.Parse(time.RFC3339Nano, at)
	if in.Stop || err != nil || in.DelaySeconds <= 0 {
		t.wake = nil
		return
	}
	desc := strings.TrimSpace(in.Reason)
	if desc == "" {
		desc = "scheduled wakeup"
	}
	t.wake = &BackgroundTask{ID: orID(id, "wakeup"), Tool: "Wakeup", Desc: Excerpt(desc, 120), Since: at}
	t.wakeAt = ts.Add(time.Duration(in.DelaySeconds * float64(time.Second)))
}

func orID(id, def string) string {
	if id == "" {
		return def
	}
	return id
}

// result applies a tool_result: a launch, or TaskStop.
func (t *bgTracker) result(toolUseID, text, at string) {
	if m := bgStoppedRE.FindStringSubmatch(text); m != nil {
		t.end(m[1])
		return
	}
	call, ok := t.calls[toolUseID]
	if !ok {
		return
	}
	delete(t.calls, toolUseID)
	var id string
	switch {
	case call.tool == "Bash":
		re := bgBashRE
		if call.fg {
			re = bgMovedRE
		}
		if m := re.FindStringSubmatch(text); m != nil {
			id = m[1]
		}
	case call.tool == "Monitor":
		if m := bgMonitorRE.FindStringSubmatch(text); m != nil {
			id = m[1]
		}
	case call.tool == "Agent":
		if strings.Contains(text, "Async agent launched") {
			if m := bgAgentRE.FindStringSubmatch(text); m != nil {
				id = m[1]
			}
		}
	}
	if id == "" {
		return
	}
	if _, dup := t.open[id]; !dup {
		t.order = append(t.order, id)
	}
	t.open[id] = BackgroundTask{ID: id, Tool: call.tool, Desc: call.desc, Since: at, Cmd: call.cmd}
}

// notification applies a raw record that holds a <task-notification>.
//
// Two notices need care, both seen in real sessions:
//   - a Monitor that runs out its timeout says "<event>[Monitor expired
//     after 30m ...]" and carries no <status>; without this it would read as
//     running until maxBackgroundAge;
//   - a background agent that stopped while work of its own still runs says
//     "<status>completed</status>" with a summary "... is waiting on its own
//     background work": it is not over, and the same task-id notifies again.
func (t *bgTracker) notification(raw []byte) {
	s := string(raw)
	id := taskIDRE.FindStringSubmatch(s)
	if id == nil {
		return
	}
	if strings.Contains(s, "<event>[Monitor expired") {
		t.end(id[1])
		return
	}
	st := taskStateRE.FindStringSubmatch(s)
	if st == nil || strings.Contains(s, "is waiting on its own background work") {
		return
	}
	switch st[1] {
	case "completed", "failed", "killed", "stopped":
		t.end(id[1])
	}
}

func (t *bgTracker) end(id string) { delete(t.open, id) }

// tasks lists the open tasks, oldest first, leaving out ones older than
// maxBackgroundAge.
func (t *bgTracker) tasks(now time.Time) []BackgroundTask {
	var out []BackgroundTask
	for _, id := range t.order {
		bt, ok := t.open[id]
		if !ok {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, bt.Since); err == nil && now.Sub(at) > maxBackgroundAge {
			continue
		}
		out = append(out, bt)
		delete(t.open, id) // an id listed twice in order shows once
	}
	if t.wake != nil && now.Before(t.wakeAt) {
		out = append(out, *t.wake)
	}
	return out
}
