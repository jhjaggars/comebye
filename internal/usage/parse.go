package usage

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// Sample is one model reply's counts. Only numbers, a model name and a
// message id are read from a transcript line; no text is kept.
type Sample struct {
	ID    string // message id (Claude); empty for Pi
	At    time.Time
	Model string
	Tok   Tokens
	Side  bool // a subagent's reply
}

// lineMeta is what a line says about where the session ran.
type lineMeta struct {
	Cwd, Branch string
}

type claudeLine struct {
	Type        string          `json:"type"`
	Timestamp   string          `json:"timestamp"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	IsSidechain bool            `json:"isSidechain"`
	UUID        string          `json:"uuid"`
	Message     json.RawMessage `json:"message"`
}

type claudeMessage struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *struct {
		In  int64 `json:"input_tokens"`
		Out int64 `json:"output_tokens"`
		CW  int64 `json:"cache_creation_input_tokens"`
		CR  int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

var usageMark = []byte(`"usage"`)

// ParseClaude reads one line of a Claude Code transcript. It returns a
// sample when the line is an assistant reply that carries usage; meta is
// where the line says the session ran (empty when it does not say). A line
// that is not JSON gives nothing.
func ParseClaude(line []byte, subagentFile bool) (s *Sample, meta lineMeta, ok bool) {
	// Cheap exit: every line that counts has a usage object.
	if !bytes.Contains(line, usageMark) {
		return nil, lineMeta{}, true
	}
	var l claudeLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil, lineMeta{}, false
	}
	meta = lineMeta{l.Cwd, l.GitBranch}
	if l.Type != "assistant" || len(l.Message) == 0 || l.Message[0] != '{' || l.Timestamp == "" {
		return nil, meta, true
	}
	var m claudeMessage
	if err := json.Unmarshal(l.Message, &m); err != nil || m.Usage == nil {
		return nil, meta, true
	}
	at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil {
		return nil, meta, true
	}
	id := m.ID
	if id == "" {
		id = l.UUID
	}
	model := m.Model
	if model == "" {
		model = "?"
	}
	t := Tokens{In: m.Usage.In, Out: m.Usage.Out, CW: m.Usage.CW, CR: m.Usage.CR, Calls: 1}
	side := l.IsSidechain || subagentFile
	if side {
		t.Sub = 1
	}
	return &Sample{ID: id, At: at, Model: model, Tok: t, Side: side}, meta, true
}

type piLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	Message   json.RawMessage `json:"message"`
}

type piMessage struct {
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Usage    *struct {
		In  int64 `json:"input"`
		Out int64 `json:"output"`
		CW  int64 `json:"cacheWrite"`
		CR  int64 `json:"cacheRead"`
	} `json:"usage"`
}

// ParsePi reads one line of a Pi session file: the session header (which
// names the directory) or a message. Pi's own cost figure is not kept.
func ParsePi(line []byte) (s *Sample, meta lineMeta, ok bool) {
	isHeader := bytes.HasPrefix(line, []byte(`{"type":"session"`))
	if !isHeader && !bytes.Contains(line, usageMark) {
		return nil, lineMeta{}, true
	}
	var l piLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil, lineMeta{}, false
	}
	if l.Type == "session" {
		return nil, lineMeta{Cwd: l.Cwd}, true
	}
	if l.Type != "message" || len(l.Message) == 0 || l.Message[0] != '{' {
		return nil, lineMeta{}, true
	}
	var m piMessage
	if err := json.Unmarshal(l.Message, &m); err != nil || m.Role != "assistant" || m.Usage == nil {
		return nil, lineMeta{}, true
	}
	at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil {
		return nil, lineMeta{}, true
	}
	model := m.Model
	if model == "" {
		model = "?"
	}
	if m.Provider != "" {
		model = m.Provider + "/" + model
	}
	return &Sample{At: at, Model: model, Tok: Tokens{In: m.Usage.In, Out: m.Usage.Out, CW: m.Usage.CW, CR: m.Usage.CR, Calls: 1}}, lineMeta{}, true
}

// PiSessionID is the id in a Pi session file name
// ("<timestamp>_<uuidv7>.jsonl"). Pi ids are UUIDv7s, whose first eight
// characters are a timestamp shared by sessions started together; the whole
// tail is the id.
func PiSessionID(base string) string {
	base = strings.TrimSuffix(base, ".jsonl")
	if i := strings.LastIndexByte(base, '_'); i >= 0 {
		return base[i+1:]
	}
	return base
}
