package transcript

// Claude Code session transcripts live at
// $CLAUDE_CONFIG_DIR/projects/<cwd-slug>/<session-id>.jsonl (default
// ~/.claude/projects). Each line is one record; the ones that matter here:
//
//	{"type":"user","uuid":..,"timestamp":..,"isSidechain":false,"isMeta":?,
//	 "isCompactSummary":?,"message":{"role":"user","content":"text"|[blocks]}}
//	{"type":"assistant","uuid":..,"message":{"id":"msg_..","model":..,
//	 "content":[one block]}}
//	{"type":"system","subtype":"compact_boundary"|"away_summary"|..,"content":".."}
//
// Claude writes one assistant record per content block (thinking, text,
// tool_use), all sharing message.id. Tool results arrive as user records
// whose content is [{type:"tool_result",tool_use_id,content,is_error}].
// User records flagged isMeta (injected context, command caveats) and
// sidechain records are not part of the visible conversation. A prompt
// sent while Claude is working arrives as
//
//	{"type":"attachment","attachment":{"type":"queued_command",
//	 "commandMode":"prompt","prompt":"text"}}
//
// when Claude takes it up mid-turn, and is shown as a user message. Everything
// else (other attachments, file-history-snapshot, mode, ai-title, cost-state,
// queue-operation, api-request*, summary, ...) is bookkeeping and skipped.
//
// Records form a parentUuid tree. Parallel tool results and rewinds create
// branches; this parser renders records in file order, which matches what
// the terminal showed except that an abandoned (rewound) branch stays
// visible.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Session file formats.
const (
	FormatPi     = "pi"
	FormatClaude = "claude"
)

// claudeAskTool is Claude Code's built-in structured question tool. Its
// input has the same shape as Pi's ask_user_question.
const claudeAskTool = "AskUserQuestion"

type claudeRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	Timestamp        string          `json:"timestamp"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Content          json.RawMessage `json:"content"` // system records
	Message          *claudeMessage  `json:"message"`
	Attachment       *claudeAttach   `json:"attachment"`
}

// claudeAttach is an attachment record's payload. The one that matters is
// queued_command: a prompt typed (or sent through herdr) while Claude was
// working, recorded when Claude takes it up mid-turn instead of as a user
// record. commandMode "prompt" is the user's text; "task-notification"
// and agent hand-backs (isMeta) are Claude's own bookkeeping.
type claudeAttach struct {
	Type        string          `json:"type"`
	CommandMode string          `json:"commandMode"`
	Prompt      json.RawMessage `json:"prompt"` // string or [blocks]
	IsMeta      bool            `json:"isMeta"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"` // string or [blocks]
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result: string or [blocks]
	IsError   bool            `json:"is_error"`
}

// sniffFormat inspects the first records of a session file to tell Pi from
// Claude Code. It leaves f at an unspecified offset; callers seek after.
func sniffFormat(f io.ReadSeeker) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return FormatPi
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for i := 0; i < 64 && sc.Scan(); i++ {
		var r struct {
			Type       string          `json:"type"`
			SessionID  *string         `json:"sessionId"`
			ParentUUID json.RawMessage `json:"parentUuid"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.Type {
		case "session", "message", "custom_message", "compaction", "model_change", "thinking_level_change":
			return FormatPi
		case "user", "assistant", "system", "attachment", "summary", "file-history-snapshot":
			return FormatClaude
		}
		if r.SessionID != nil || len(r.ParentUUID) > 0 {
			return FormatClaude
		}
	}
	return FormatPi
}

func decodeClaudeBlocks(raw json.RawMessage) ([]claudeBlock, string, bool) {
	if len(raw) == 0 {
		return nil, "", false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return nil, s, true
		}
		return nil, "", false
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, "", false
	}
	return blocks, "", false
}

// claudeParser accumulates messages from Claude records in file order.
type claudeParser struct {
	all []Message
	// tool_use id -> index into all, patched when the result arrives.
	toolIdx map[string]int
	// tool_use id -> when the call was recorded, for result durations.
	toolStart map[string]time.Time
	// The assistant message in progress and its thinking, which is
	// attached to the next text bubble of the same message.
	curMsgID string
	think    string
	ask      *Ask
	// Index of a compact_boundary placeholder awaiting its summary, or -1.
	compactIdx int
}

func newClaudeParser() *claudeParser {
	return &claudeParser{toolIdx: map[string]int{}, toolStart: map[string]time.Time{}, compactIdx: -1}
}

func (p *claudeParser) feed(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var r claudeRecord
	if json.Unmarshal(line, &r) != nil || r.IsSidechain {
		return
	}
	switch r.Type {
	case "assistant":
		if r.Message != nil {
			p.assistant(&r)
		}
	case "user":
		if r.Message != nil && !r.IsMeta {
			p.user(&r)
		}
	case "system":
		p.system(&r)
	case "attachment":
		if a := r.Attachment; a != nil && a.Type == "queued_command" && a.CommandMode == "prompt" && !a.IsMeta && !r.IsMeta {
			p.queued(&r)
		}
	}
}

// queued renders a prompt queued mid-turn as a user message.
func (p *claudeParser) queued(r *claudeRecord) {
	blocks, s, isStr := decodeClaudeBlocks(r.Attachment.Prompt)
	if !isStr {
		s = claudeText(blocks)
	}
	if strings.TrimSpace(s) != "" {
		p.userText(r, s)
	}
}

func (p *claudeParser) assistant(r *claudeRecord) {
	if r.Message.ID == "" || r.Message.ID != p.curMsgID {
		p.curMsgID = r.Message.ID
		p.think = ""
	}
	model := r.Message.Model
	if model == "<synthetic>" {
		model = "" // client-side messages such as API errors
	}
	blocks, s, isStr := decodeClaudeBlocks(r.Message.Content)
	if isStr {
		blocks = []claudeBlock{{Type: "text", Text: s}}
	}
	for i, b := range blocks {
		switch b.Type {
		case "thinking":
			if b.Thinking != "" {
				if p.think != "" {
					p.think += "\n\n"
				}
				p.think += b.Thinking
			}
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			id := r.UUID
			if i > 0 {
				id = r.UUID + "#" + strconv.Itoa(i)
			}
			p.all = append(p.all, Message{ID: id, Kind: "assistant", Time: r.Timestamp, Model: model, Text: b.Text, Think: p.think})
			p.think = ""
		case "tool_use":
			p.all = append(p.all, Message{ID: b.ID, Kind: "tool", Tool: b.Name, Args: summarizeArgs(b.Input)})
			p.toolIdx[b.ID] = len(p.all) - 1
			if t, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
				p.toolStart[b.ID] = t
			}
			if b.Name == claudeAskTool {
				if a := parseAsk(b.ID, b.Input); a != nil {
					p.ask = a
				}
			}
		}
	}
}

func (p *claudeParser) user(r *claudeRecord) {
	blocks, s, isStr := decodeClaudeBlocks(r.Message.Content)
	if r.IsCompactSummary {
		text := s
		if !isStr {
			text = claudeText(blocks)
		}
		note := truncate(strings.TrimSpace(text), maxNoteChars)
		if p.compactIdx >= 0 {
			p.all[p.compactIdx].Note = note
			p.compactIdx = -1
		} else {
			p.all = append(p.all, Message{ID: r.UUID, Kind: "compaction", Time: r.Timestamp, Note: note})
		}
		return
	}
	if isStr {
		p.userText(r, s)
		return
	}
	var texts []string
	images := 0
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			p.toolResult(r, b)
		case "text":
			texts = append(texts, b.Text)
		case "image":
			images++
		}
	}
	text := strings.Join(texts, "\n")
	if strings.TrimSpace(text) == "" && images > 0 {
		text = "[image]"
		if images > 1 {
			text = "[" + strconv.Itoa(images) + " images]"
		}
	}
	if text != "" {
		p.userText(r, text)
	}
}

// userText classifies a user-authored string: Claude stores slash
// commands, ! shell commands, their output, and background task
// notifications as tagged pseudo-XML in user records.
func (p *claudeParser) userText(r *claudeRecord, s string) {
	kind, text := classifyClaudeText(s)
	switch kind {
	case "user":
		p.all = append(p.all, Message{ID: r.UUID, Kind: "user", Time: r.Timestamp, Text: text})
	case "note":
		p.all = append(p.all, Message{ID: r.UUID, Kind: "note", Time: r.Timestamp, Note: truncate(text, maxNoteChars)})
	}
}

func (p *claudeParser) toolResult(r *claudeRecord, b claudeBlock) {
	if p.ask != nil && p.ask.ToolCallID == b.ToolUseID {
		p.ask = nil
	}
	idx, ok := p.toolIdx[b.ToolUseID]
	if !ok {
		return
	}
	blocks, s, isStr := decodeClaudeBlocks(b.Content)
	if !isStr {
		s = claudeText(blocks)
		if s == "" {
			for _, x := range blocks {
				if x.Type == "image" {
					s = "[image]"
					break
				}
			}
		}
	}
	p.all[idx].Result = truncate(s, maxResultChars)
	p.all[idx].Err = b.IsError
	if start, ok := p.toolStart[b.ToolUseID]; ok {
		if end, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil && end.After(start) {
			p.all[idx].Ms = end.Sub(start).Milliseconds()
		}
		delete(p.toolStart, b.ToolUseID)
	}
}

func (p *claudeParser) system(r *claudeRecord) {
	var content string
	if len(r.Content) > 0 && r.Content[0] == '"' {
		_ = json.Unmarshal(r.Content, &content)
	}
	switch r.Subtype {
	case "compact_boundary":
		p.all = append(p.all, Message{ID: r.UUID, Kind: "compaction", Time: r.Timestamp, Note: "Conversation compacted"})
		p.compactIdx = len(p.all) - 1
	case "local_command":
		p.userText(r, content)
	case "away_summary", "informational", "scheduled_task_fire":
		if content = strings.TrimSpace(content); content != "" {
			p.all = append(p.all, Message{ID: r.UUID, Kind: "note", Time: r.Timestamp, Note: truncate(content, maxNoteChars)})
		}
	}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// classifyClaudeText maps a user-record string to ("user"|"note"|"", text).
func classifyClaudeText(s string) (string, string) {
	t := strings.TrimSpace(s)
	switch {
	case t == "":
		return "", ""
	case strings.Contains(t, "<command-name>"):
		name, _ := tagInner(t, "command-name")
		args, _ := tagInner(t, "command-args")
		return "user", strings.TrimSpace(name + " " + args)
	case strings.HasPrefix(t, "<bash-input>"):
		cmd, _ := tagInner(t, "bash-input")
		return "user", "! " + cmd
	case strings.HasPrefix(t, "<local-command-stdout>"), strings.HasPrefix(t, "<local-command-stderr>"),
		strings.HasPrefix(t, "<bash-stdout>"), strings.HasPrefix(t, "<bash-stderr>"):
		var parts []string
		for _, tag := range []string{"local-command-stdout", "local-command-stderr", "bash-stdout", "bash-stderr"} {
			if v, ok := tagInner(t, tag); ok {
				if v = strings.TrimSpace(ansiRE.ReplaceAllString(v, "")); v != "" {
					parts = append(parts, v)
				}
			}
		}
		if len(parts) == 0 {
			return "", ""
		}
		return "note", strings.Join(parts, "\n")
	case strings.HasPrefix(t, "<task-notification>"):
		if v, ok := tagInner(t, "summary"); ok && strings.TrimSpace(v) != "" {
			return "note", strings.TrimSpace(v)
		}
		return "note", "Background task update"
	case strings.HasPrefix(t, "<local-command-caveat>"), strings.HasPrefix(t, "<system-reminder>"):
		return "", ""
	case strings.HasPrefix(t, "[Request interrupted by user"):
		return "note", strings.Trim(t, "[]")
	}
	return "user", s
}

// tagInner returns the text between <tag> and </tag>.
func tagInner(s, tag string) (string, bool) {
	open, closeTag := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closeTag)
	if j < 0 {
		return rest, true
	}
	return rest[:j], true
}

func claudeText(blocks []claudeBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
