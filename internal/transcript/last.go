package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// Latest summarizes the tail of a session for notifications: the most
// recent assistant text, and any tool calls from the final assistant turn
// that have no result yet (what a permission prompt is usually about).
type Latest struct {
	Text    string        `json:"text,omitempty"`
	Time    string        `json:"time,omitempty"`
	Pending []PendingTool `json:"pending,omitempty"`
}

// PendingTool is a tool call still waiting for its result.
type PendingTool struct {
	Tool string `json:"tool"`
	Args string `json:"args,omitempty"`
}

// tailWindow bounds how much of a session file LatestOf reads. Sessions
// grow to many megabytes; the last assistant message is near the end.
const tailWindow = 1 << 20

// LatestOf reads the last tailWindow bytes of the session at path. A
// missing file returns os.ErrNotExist.
func LatestOf(path string) (*Latest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	format := sniffFormat(f)
	var r io.Reader = f
	partial := false
	offset := int64(0)
	if fi.Size() > tailWindow {
		offset = fi.Size() - tailWindow
		partial = true
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	if format == FormatClaude {
		return claudeLatestFrom(r, partial)
	}
	return latestFrom(r, partial)
}

func latestFrom(r io.Reader, skipFirst bool) (*Latest, error) {
	out := &Latest{}
	// Tool calls of the most recent assistant record, by id; resolved ones
	// are deleted as their toolResult records arrive.
	var pendingOrder []string
	pending := map[string]PendingTool{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if first && skipFirst {
			first = false
			continue // the seek landed mid-record
		}
		first = false
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec rawRecord
		if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "message" || rec.Message == nil {
			continue
		}
		switch rec.Message.Role {
		case "assistant":
			blocks := decodeBlocks(rec.Message.Content)
			if text := strings.TrimSpace(firstText(blocks)); text != "" {
				out.Text = text
				out.Time = rec.Timestamp
			}
			pendingOrder = pendingOrder[:0]
			pending = map[string]PendingTool{}
			for _, b := range blocks {
				if b.Type == "toolCall" && b.ID != "" {
					pendingOrder = append(pendingOrder, b.ID)
					pending[b.ID] = PendingTool{Tool: b.Name, Args: summarizeArgs(b.Name, b.Args)}
				}
			}
		case "toolResult":
			delete(pending, rec.Message.ToolCallID)
		case "user":
			// A new user turn supersedes whatever was pending.
			pendingOrder = pendingOrder[:0]
			pending = map[string]PendingTool{}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, id := range pendingOrder {
		if p, ok := pending[id]; ok {
			out.Pending = append(out.Pending, p)
		}
	}
	return out, nil
}

// LatestFromMessages approximates LatestOf from an already rendered window
// of messages (what a peer's transcript endpoint returns, since the hub
// cannot read the peer's session file): the last assistant text, and the
// trailing tool cards that have no result yet.
func LatestFromMessages(msgs []Message) *Latest {
	out := &Latest{}
	var pending []PendingTool
	for _, m := range msgs {
		switch m.Kind {
		case "assistant":
			if t := strings.TrimSpace(m.Text); t != "" {
				out.Text, out.Time = t, m.Time
			}
			pending = pending[:0]
		case "user":
			pending = pending[:0]
		case "tool":
			if m.Result == "" && !m.Err && m.Ms == 0 {
				pending = append(pending, PendingTool{Tool: m.Tool, Args: m.Args})
			} else {
				pending = pending[:0]
			}
		}
	}
	if len(pending) > 0 {
		out.Pending = append([]PendingTool(nil), pending...)
	}
	return out
}

// Excerpt flattens s to one line of at most n runes (plus an ellipsis),
// for notification bodies. n <= 0 returns "".
func Excerpt(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n])) + "…"
}
