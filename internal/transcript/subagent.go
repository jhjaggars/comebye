package transcript

// Claude Code subagents. A call of the Agent tool (older versions: Task)
// starts one:
//
//	{"type":"tool_use","name":"Agent","input":{"description":"..",
//	 "subagent_type":"..","prompt":"..","isolation":"worktree"?,
//	 "run_in_background":true?}}
//
// A synchronous call's tool_result is the subagent's report. A background
// (async) call returns at once with "Async agent launched successfully ...
// agentId: <id> ..." and a record-level toolUseResult {isAsync:true,
// status:"async_launched", agentId}; later the parent session gets
//
//   - the report, as an isMeta user record (or a queued_command attachment
//     marked isMeta) whose origin is {kind:"peer", from:<agentId>,
//     handback:true, handbackReportText:".."}, and
//   - a <task-notification> naming <task-id> (the agentId), <tool-use-id>,
//     <status> (completed|failed|killed|stopped) and <usage>, as a user
//     record when the parent is idle or as a queued_command attachment with
//     commandMode "task-notification" mid-turn. A resumed agent notifies
//     again.
//
// All of it is folded into the Agent call's card (Message.Subagent). The
// subagent's own transcript is <session-id>/subagents/agent-<agentId>.jsonl
// next to the parent session; every record there is isSidechain, and its
// last step is a SubagentHandback call carrying the report.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SubagentInfo describes the subagent an Agent/Task call started.
type SubagentInfo struct {
	AgentID     string `json:"agent_id,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"` // subagent_type
	Background  bool   `json:"background,omitempty"`
	Worktree    bool   `json:"worktree,omitempty"`
	// Status is running, completed, failed, killed or stopped.
	Status   string `json:"status"`
	DurMs    int64  `json:"dur_ms,omitempty"`
	ToolUses int    `json:"tool_uses,omitempty"`
	Tokens   int64  `json:"tokens,omitempty"`
	Report   string `json:"report,omitempty"` // markdown
	Prompt   string `json:"prompt,omitempty"`
}

// RunningSubagent is a subagent of this session that has not finished.
type RunningSubagent struct {
	ID          string `json:"id"` // the Agent call's message id
	AgentID     string `json:"agent_id,omitempty"`
	Description string `json:"description,omitempty"`
	Since       string `json:"since,omitempty"`
}

const (
	maxReportChars = 20000
	maxPromptChars = 8000
	// handbackTool is the subagent's final call; its input.message is the
	// report.
	handbackTool = "SubagentHandback"
)

func isAgentTool(name string) bool { return name == "Agent" || name == "Task" }

// claudeOrigin is where a user record or queued command came from.
type claudeOrigin struct {
	Kind               string `json:"kind"`
	From               string `json:"from"`
	Handback           bool   `json:"handback"`
	HandbackReportText string `json:"handbackReportText"`
}

// newSubagent parses an Agent/Task call's input.
func newSubagent(raw json.RawMessage) *SubagentInfo {
	var in struct {
		Description string `json:"description"`
		Type        string `json:"subagent_type"`
		Prompt      string `json:"prompt"`
		Isolation   string `json:"isolation"`
		Background  any    `json:"run_in_background"`
	}
	_ = json.Unmarshal(raw, &in)
	bg := false
	switch v := in.Background.(type) {
	case bool:
		bg = v
	case string:
		bg, _ = strconv.ParseBool(v)
	}
	return &SubagentInfo{
		Description: truncate(strings.TrimSpace(in.Description), maxArgsChars),
		Type:        truncate(in.Type, maxArgsChars),
		Background:  bg,
		Worktree:    in.Isolation == "worktree",
		Status:      "running",
		Prompt:      truncate(in.Prompt, maxPromptChars),
	}
}

// sub returns a copy of the card's SubagentInfo to modify, installed in
// its place. Windows handed out earlier share the old pointer, which is
// never written again (copy on write), so they can be encoded unlocked.
func (p *claudeParser) sub(idx int) *SubagentInfo {
	s := *p.all[idx].Subagent
	p.all[idx].Subagent = &s
	return &s
}

var launchIDRE = regexp.MustCompile(`agentId:\s*([A-Za-z0-9_-]+)`)

// subagentResult applies an Agent call's tool_result: the launch of a
// background agent, or a synchronous agent's report.
func (p *claudeParser) subagentResult(idx int, r *claudeRecord, b claudeBlock, text string) {
	var res struct {
		IsAsync           bool          `json:"isAsync"`
		Status            string        `json:"status"`
		AgentID           string        `json:"agentId"`
		TotalDurationMs   int64         `json:"totalDurationMs"`
		TotalTokens       int64         `json:"totalTokens"`
		TotalToolUseCount int           `json:"totalToolUseCount"`
		Content           []claudeBlock `json:"content"`
	}
	if len(r.ToolUseResult) > 0 && r.ToolUseResult[0] == '{' {
		_ = json.Unmarshal(r.ToolUseResult, &res)
	}
	s := p.sub(idx)
	if s.AgentID == "" {
		s.AgentID = res.AgentID
	}
	async := res.IsAsync || res.Status == "async_launched" || strings.HasPrefix(text, "Async agent launched")
	if s.AgentID == "" && async {
		if m := launchIDRE.FindStringSubmatch(text); m != nil {
			s.AgentID = m[1]
		}
	}
	if s.AgentID != "" {
		p.subIdx[s.AgentID] = idx
	}
	switch {
	case b.IsError:
		s.Status = "failed"
		p.all[idx].Result = truncate(text, maxResultChars)
	case async:
		// The launch boilerplate is for the model, not the reader.
		s.Background = true
		p.all[idx].Result = ""
	default:
		s.Status = "completed"
		report := claudeText(res.Content)
		if report == "" {
			report = stripAgentFooter(text)
		}
		s.Report = truncate(strings.TrimSpace(report), maxReportChars)
		s.DurMs, s.Tokens, s.ToolUses = res.TotalDurationMs, res.TotalTokens, res.TotalToolUseCount
		p.all[idx].Result = ""
	}
}

// stripAgentFooter drops the "agentId: ... (for resuming ...)" and usage
// lines Claude appends to a synchronous agent's result text.
func stripAgentFooter(s string) string {
	if i := strings.LastIndex(s, "\nagentId: "); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "\n<usage>"); i >= 0 {
		s = s[:i]
	}
	return s
}

// handback records a background agent's report.
func (p *claudeParser) handback(o *claudeOrigin) {
	idx, ok := p.subIdx[o.From]
	if !ok || strings.TrimSpace(o.HandbackReportText) == "" {
		return
	}
	s := p.sub(idx)
	s.Report = truncate(strings.TrimSpace(o.HandbackReportText), maxReportChars)
	// Handing back ends the run; the notification that follows refines
	// the status and adds usage.
	if s.Status == "running" {
		s.Status = "completed"
	}
}

// notifyUsage is a queued task-notification's own usage summary.
type notifyUsage struct {
	TotalTokens int64 `json:"totalTokens"`
	ToolUses    int   `json:"toolUses"`
	DurationMs  int64 `json:"durationMs"`
}

// taskNotification folds a <task-notification> into the card of the
// agent it is about. It reports false when no card matches (a background
// shell task, or an agent launched before this window of the file), so
// the caller can show it as a note instead.
func (p *claudeParser) taskNotification(text string, u *notifyUsage) bool {
	taskID, _ := tagInner(text, "task-id")
	toolUseID, _ := tagInner(text, "tool-use-id")
	taskID, toolUseID = strings.TrimSpace(taskID), strings.TrimSpace(toolUseID)
	idx, ok := p.subIdx[taskID]
	if !ok {
		i, found := p.toolIdx[toolUseID]
		if !found || p.all[i].Subagent == nil {
			return false
		}
		idx = i
	}
	s := p.sub(idx)
	if s.AgentID == "" && taskID != "" {
		s.AgentID = taskID
		p.subIdx[taskID] = idx
	}
	if st, _ := tagInner(text, "status"); strings.TrimSpace(st) != "" {
		s.Status = strings.TrimSpace(st)
	} else if s.Status == "running" {
		s.Status = "completed"
	}
	if usage, ok := tagInner(text, "usage"); ok {
		num := func(tags ...string) int64 {
			for _, tag := range tags {
				if v, ok := tagInner(usage, tag); ok {
					if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
						return n
					}
				}
			}
			return 0
		}
		s.Tokens = num("subagent_tokens", "total_tokens")
		s.ToolUses = int(num("tool_uses"))
		s.DurMs = num("duration_ms")
	} else if u != nil {
		s.Tokens, s.ToolUses, s.DurMs = u.TotalTokens, u.ToolUses, u.DurationMs
	}
	// Without a hand-back (older versions) the report is the <result>.
	if res, ok := tagInner(text, "result"); ok && s.Report == "" && !strings.Contains(res, handbackTool) {
		s.Report = truncate(strings.TrimSpace(res), maxReportChars)
	}
	return true
}

// running lists the subagents still running, oldest first.
func (p *claudeParser) running() []RunningSubagent {
	var out []RunningSubagent
	for _, idx := range p.subs {
		m := p.all[idx]
		if m.Subagent.Status != "running" {
			continue
		}
		out = append(out, RunningSubagent{ID: m.ID, AgentID: m.Subagent.AgentID, Description: m.Subagent.Description, Since: m.Time})
	}
	return out
}

// agentIDRE is what a subagent id looks like ("a" and hex digits); it
// becomes part of a file name, so nothing else is accepted.
var agentIDRE = regexp.MustCompile(`^a[0-9a-f]{6,32}$`)

// ValidAgentID reports whether id is a well-formed Claude subagent id.
func ValidAgentID(id string) bool { return agentIDRE.MatchString(id) }

// ErrNoSubagent means the subagent's transcript does not exist.
var ErrNoSubagent = errors.New("no such subagent transcript")

// SubagentPath returns the transcript of subagent agentID of the Claude
// session at sessionPath: <session dir>/<session id>/subagents/
// agent-<agentID>.jsonl. It must be a regular file inside that directory.
func SubagentPath(sessionPath, agentID string) (string, error) {
	if !ValidAgentID(agentID) || !strings.HasSuffix(sessionPath, ".jsonl") {
		return "", ErrNoSubagent
	}
	dir := filepath.Join(strings.TrimSuffix(sessionPath, ".jsonl"), "subagents")
	p := filepath.Join(dir, "agent-"+agentID+".jsonl")
	if rel, err := filepath.Rel(dir, p); err != nil || rel != filepath.Base(p) {
		return "", ErrNoSubagent
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", ErrNoSubagent
	}
	return p, nil
}

// ReadSubagent parses a subagent transcript (see SubagentPath) like Read
// does a session: its sidechain records are the conversation, and the
// final SubagentHandback call is shown as the agent's last message.
func ReadSubagent(path string, limit, skip int) (*Transcript, error) {
	if limit <= 0 {
		limit = 100
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readClaude(f, path, limit, skip, true)
}
