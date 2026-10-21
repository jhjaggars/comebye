// Package transcript parses agent session JSONL files (Pi, and Claude Code;
// see claude.go) into a chat-friendly message stream. Pi writes sessions as newline-delimited JSON records
// (session version 3):
//
//	{"type":"session",...}
//	{"type":"model_change"|"thinking_level_change"|"custom"|...}
//	{"type":"message","message":{"role":"user"|"assistant"|"toolResult","content":[blocks]}}
//	{"type":"custom_message","customType":"...","content":"...","display":bool}
//	{"type":"compaction","summary":"..."}
//
// Content blocks are {type:"text"|"thinking"|"toolCall", ...}. A toolResult
// record carries toolCallId, toolName, content, isError, and durationMs,
// and is paired back to the toolCall block that produced it.
package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Message is one renderable item in the chat view.
type Message struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // user | assistant | tool | note | compaction
	Time   string `json:"time,omitempty"`
	Text   string `json:"text,omitempty"`   // user / assistant text
	Think  string `json:"think,omitempty"`  // assistant thinking
	Model  string `json:"model,omitempty"`  // assistant model id
	Note   string `json:"note,omitempty"`   // note / compaction summary
	Tool   string `json:"tool,omitempty"`   // tool name
	Args   string `json:"args,omitempty"`   // compact argument summary
	Result string `json:"result,omitempty"` // truncated tool result
	Err    bool   `json:"err,omitempty"`    // tool result errored
	Ms     int64  `json:"ms,omitempty"`     // tool duration
}

// Transcript is a parsed window of a session file.
type Transcript struct {
	Messages []Message `json:"messages"`
	Total    int       `json:"total"` // total parseable messages in the file
	More     bool      `json:"more"`  // older messages exist beyond this window
	// Ask is the agent's open ask_user_question call, if the latest one has
	// no tool result yet: the questionnaire is (or was, if Pi died) waiting
	// on the user. Computed over the whole file, independent of the window.
	Ask *Ask `json:"ask,omitempty"`
	// Format is the session file format: "pi" or "claude".
	Format string `json:"format,omitempty"`
}

// askToolName is the tool registered by the @juicesharp/rpiv-ask-user-question
// Pi extension. Pi writes the assistant message carrying the toolCall before
// the tool runs, so the question and its options are on disk while the
// dialog is open; the toolResult (the answer) is appended afterwards.
const askToolName = "ask_user_question"

// Ask is a pending structured questionnaire.
type Ask struct {
	ToolCallID string        `json:"tool_call_id"`
	Questions  []AskQuestion `json:"questions"`
}

// AskQuestion is one question of a questionnaire (the tool allows 1-4).
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	MultiSelect bool        `json:"multi_select,omitempty"`
	Options     []AskOption `json:"options"`
}

// AskOption is one authored choice. The dialog also appends its own
// "Type something." row (and "Next" for multi-select), which are not listed.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	HasPreview  bool   `json:"has_preview,omitempty"`
}

// limits keep phone payloads small.
const (
	maxResultChars = 4000
	maxArgsChars   = 500
	maxNoteChars   = 2000
)

type rawRecord struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Summary string      `json:"summary"`
	Message *rawMessage `json:"message"`
	// custom_message fields
	CustomType string `json:"customType"`
	Content    string `json:"content"`
	Display    *bool  `json:"display"`
	Timestamp  string `json:"timestamp"`
}

type rawMessage struct {
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"` // array of blocks or plain string
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
	DurationMs int64           `json:"durationMs"`
}

type block struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	ID       string          `json:"id"`
	Args     json.RawMessage `json:"arguments"`
}

// Read parses the session at path and returns the last `limit` messages,
// skipping `skip` messages counting back from the end (for "load earlier").
// A missing file is returned as os.ErrNotExist so callers can fall back to
// raw pane output.
func Read(path string, limit, skip int) (*Transcript, error) {
	if limit <= 0 {
		limit = 100
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if sniffFormat(f) == FormatClaude {
		return readClaude(f, path, limit, skip)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var all []Message
	// toolCallId -> index into all; tool cards are patched in place when
	// their toolResult record arrives (indices, not pointers, because the
	// slice keeps growing).
	toolIdx := map[string]int{}
	// The latest ask_user_question call, cleared when its result arrives.
	var pendingAsk *Ask

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r rawRecord
		if err := json.Unmarshal(line, &r); err != nil {
			continue // tolerate partially-written trailing lines
		}
		switch r.Type {
		case "message":
			if r.Message == nil {
				continue
			}
			switch r.Message.Role {
			case "user":
				text := firstText(decodeBlocks(r.Message.Content))
				if text != "" {
					all = append(all, Message{ID: r.ID, Kind: "user", Time: r.Timestamp, Text: text})
				}
			case "assistant":
				appendAssistant(&all, toolIdx, r)
				if ask := findAsk(r.Message.Content); ask != nil {
					pendingAsk = ask
				}
			case "toolResult":
				if pendingAsk != nil && pendingAsk.ToolCallID == r.Message.ToolCallID {
					pendingAsk = nil
				}
				if idx, ok := toolIdx[r.Message.ToolCallID]; ok {
					all[idx].Result = truncate(firstText(decodeBlocks(r.Message.Content)), maxResultChars)
					all[idx].Err = r.Message.IsError
					all[idx].Ms = r.Message.DurationMs
				}
			}
		case "custom_message":
			if r.Display != nil && !*r.Display {
				continue
			}
			all = append(all, Message{ID: r.ID, Kind: "note", Note: truncate(r.Content, maxNoteChars)})
		case "compaction":
			all = append(all, Message{ID: r.ID, Kind: "compaction", Note: truncate(r.Summary, maxNoteChars)})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}

	return window(all, limit, skip, pendingAsk, FormatPi), nil
}

// window cuts the last `limit` messages, `skip` back from the end.
func window(all []Message, limit, skip int, ask *Ask, format string) *Transcript {
	total := len(all)
	start := total - skip - limit
	if start < 0 {
		start = 0
	}
	end := total - skip
	if end < 0 {
		end = 0
	}
	return &Transcript{
		Messages: all[start:end],
		Total:    total,
		More:     start > 0,
		Ask:      ask,
		Format:   format,
	}
}

// findAsk returns the last ask_user_question call in an assistant message's
// content, parsed into questions, or nil.
func findAsk(content json.RawMessage) *Ask {
	var ask *Ask
	for _, b := range decodeBlocks(content) {
		if b.Type != "toolCall" || b.Name != askToolName {
			continue
		}
		if a := parseAsk(b.ID, b.Args); a != nil {
			ask = a
		}
	}
	return ask
}

// parseAsk decodes an ask-user-question tool input ({questions:[{question,
// header, multiSelect, options:[{label, description, preview}]}]}), the
// shape shared by Pi's ask_user_question and Claude Code's AskUserQuestion.
// It returns nil when there are no questions.
func parseAsk(id string, raw json.RawMessage) *Ask {
	var args struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
				Preview     string `json:"preview"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || len(args.Questions) == 0 {
		return nil
	}
	a := &Ask{ToolCallID: id}
	for _, q := range args.Questions {
		aq := AskQuestion{
			Question:    truncate(q.Question, maxNoteChars),
			Header:      truncate(q.Header, maxArgsChars),
			MultiSelect: q.MultiSelect,
		}
		for _, o := range q.Options {
			aq.Options = append(aq.Options, AskOption{
				Label:       truncate(o.Label, maxArgsChars),
				Description: truncate(o.Description, maxArgsChars),
				HasPreview:  o.Preview != "",
			})
		}
		a.Questions = append(a.Questions, aq)
	}
	return a
}

// appendAssistant emits the assistant bubble (text/thinking, if any) and a
// standalone card per toolCall block, registering cards for later result
// pairing.
func appendAssistant(all *[]Message, toolIdx map[string]int, r rawRecord) {
	blocks := decodeBlocks(r.Message.Content)
	am := Message{ID: r.ID, Kind: "assistant", Model: r.Message.Model, Time: r.Timestamp}
	hasText := false
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if hasText {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Text)
			hasText = true
		}
	}
	am.Text = sb.String()
	for _, b := range blocks {
		if b.Type == "thinking" && b.Thinking != "" {
			if am.Think != "" {
				am.Think += "\n\n"
			}
			am.Think += b.Thinking
		}
	}
	if hasText {
		*all = append(*all, am)
	}
	for _, b := range blocks {
		if b.Type != "toolCall" {
			continue
		}
		*all = append(*all, Message{
			ID:   b.ID,
			Kind: "tool",
			Tool: b.Name,
			Args: summarizeArgs(b.Args),
		})
		toolIdx[b.ID] = len(*all) - 1
	}
}

// decodeBlocks accepts either a JSON array of blocks or a plain string.
func decodeBlocks(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

func firstText(blocks []block) string {
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

// summarizeArgs renders tool arguments as a compact preview, preferring the
// most informative scalar field.
func summarizeArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return truncate(string(raw), maxArgsChars)
	}
	for _, key := range []string{"command", "prompt", "path", "file_path", "query", "description", "pattern", "url", "name", "task"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return truncate(strings.TrimSpace(s), maxArgsChars)
			}
		}
	}
	compact, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return truncate(string(compact), maxArgsChars)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
