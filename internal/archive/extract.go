package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Msg is one indexed piece of a conversation.
type Msg struct {
	Seq  int
	Role string // user | assistant | tool | result
	TS   string
	Text string
	Sub  string // subagent id, "" for the main thread
	// Where the record is in the archive, so that a later reader can fetch the
	// message's full text from the segments that hold it.
	Stream string
	Off    int64  // byte offset of the record's line in Stream
	ToolID string // ties a tool call and its result together
	IsErr  bool   // a tool result that reported failure
	Secret bool   // something in Text was masked
	// FullLen is the length of the original text when Text holds only part of
	// it; 0 when Text is all of it.
	FullLen int
}

// CommitRef is a commit a session made, pushed or saw.
type CommitRef struct {
	SHA     string
	Rel     string // made | pushed | seen
	Branch  string
	Subject string
	// The message the Rel came from (see Produced).
	Seq    int
	Sub    string
	ToolID string
	TS     string
}

// Parsed is what the index learns from one session's files.
type Parsed struct {
	Cwd, Branch, Title, FirstPrompt, Version, Start, End, AgentSessionID string
	Msgs                                                                 []Msg
	Commits                                                              []CommitRef
	// Produced is everything else the session made that the tool results show:
	// Pages, PRs, issues, comments, releases, pushed images (see produced.go).
	Produced []Produced
	// Docs are markdown files under a docs/ directory that the session wrote
	// or edited (from the tool call, so: what it did, not what it claimed).
	Docs []string
	// Secrets counts the messages with masked text (see mask.go); stream and
	// off say where the record being parsed is.
	Secrets int
	stream  string
	off     int64
	calls   map[string]call // tool calls so far, by id: a result is read with its command
}

// call is a tool call: what it ran and which message holds it.
type call struct {
	cmd string
	at  at
}

const (
	maxMsgText    = 4000 // per message, indexed
	maxResultText = 1500 // per tool result, indexed: this much from each end
)

type claudeRec struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Timestamp string `json:"timestamp"`
	Version   string `json:"version"`
	AITitle   string `json:"aiTitle"`
	IsMeta    bool   `json:"isMeta"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"`
	Content   json.RawMessage `json:"content"`
}

// ParseClaude reads a Claude Code session file (or a subagent file; sub is
// its id) and appends to p.
func (p *Parsed) ParseClaude(r io.Reader, sub string) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var pos int64
	for {
		line, err := br.ReadBytes('\n')
		p.off = pos
		pos += int64(len(line))
		if len(line) > 0 && line[len(line)-1] == '\n' { // an unterminated tail is not parsed yet
			var rec claudeRec
			if json.Unmarshal(line, &rec) == nil {
				p.claudeRec(&rec, sub)
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (p *Parsed) claudeRec(rec *claudeRec, sub string) {
	if sub == "" {
		if rec.Cwd != "" && p.Cwd == "" {
			p.Cwd = rec.Cwd
		}
		if rec.GitBranch != "" && rec.GitBranch != "HEAD" {
			p.Branch = rec.GitBranch // the last seen: the branch it ended on
		}
		if rec.Version != "" {
			p.Version = rec.Version
		}
		if rec.SessionID != "" && p.AgentSessionID == "" {
			p.AgentSessionID = rec.SessionID
		}
		if rec.AITitle != "" {
			p.Title = maskPlain(rec.AITitle)
		}
		p.stamp(rec.Timestamp)
	}
	switch rec.Type {
	case "user", "assistant":
	default:
		return
	}
	role := rec.Message.Role
	if role == "" {
		role = rec.Type
	}
	c := bytes.TrimSpace(rec.Message.Content)
	if len(c) == 0 {
		return
	}
	if c[0] == '"' {
		var s string
		_ = json.Unmarshal(c, &s)
		if !rec.IsMeta {
			p.add(role, rec.Timestamp, s, sub)
		}
		return
	}
	var bs []block
	if json.Unmarshal(c, &bs) != nil {
		return
	}
	p.blocks(role, rec.Timestamp, bs, sub, "input")
}

func (p *Parsed) blocks(role, ts string, bs []block, sub, argField string) {
	for _, b := range bs {
		switch b.Type {
		case "text":
			p.add(role, ts, b.Text, sub)
		case "tool_use", "toolCall":
			args := b.Input
			if len(args) == 0 {
				args = b.Arguments
			}
			if sub == "" {
				p.doc(b.Name, args)
			}
			sum := argSummary(args)
			a := at{seq: -1}
			if m := p.push("tool", ts, b.Name+" "+sum, sub); m != nil {
				m.ToolID = b.ID
				a = at{seq: m.Seq, sub: sub, id: b.ID, ts: ts}
			}
			p.noteCall(b.ID, sum, a)
			p.commitsIn(sum, a)
		case "tool_result":
			p.result(ts, textOf(b.Content), sub, b.ToolUseID, b.IsError)
		}
	}
}

func (p *Parsed) noteCall(id, cmd string, a at) {
	if id == "" {
		return
	}
	if p.calls == nil {
		p.calls = map[string]call{}
	}
	p.calls[id] = call{cmd, a}
}

// result adds a tool's output and reads what it made. A result with no text is
// not kept as a message, so what the call made is tied to the call's message.
func (p *Parsed) result(ts, text, sub, toolID string, isErr bool) {
	a := at{seq: -1}
	if m := p.addResult(ts, text, sub, toolID, isErr); m != nil {
		a = at{seq: m.Seq, sub: sub, id: toolID, ts: ts}
	}
	p.commitsIn(text, a)
	if isErr {
		return
	}
	c := p.calls[toolID]
	if a.none() {
		a = c.at
	}
	p.producedIn(c.cmd, text, a)
}

func (p *Parsed) stamp(ts string) {
	if ts == "" {
		return
	}
	if p.Start == "" || ts < p.Start {
		p.Start = ts
	}
	if ts > p.End {
		p.End = ts
	}
}

func (p *Parsed) add(role, ts, text, sub string) { p.push(role, ts, text, sub) }

// push adds a message, masked and clipped to its head, and returns it (nil
// when there was no text) so that the caller can fill in the rest.
func (p *Parsed) push(role, ts, text, sub string) *Msg {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	full := len(text)
	masked, secret := maskHead(text, maxMsgText)
	secret = secret || strings.Contains(masked, MaskMark) // argSummary masks what it clips
	if full <= maxMsgText {
		full = 0
	}
	if role == "user" && sub == "" && p.FirstPrompt == "" && !strings.HasPrefix(masked, "<") {
		p.FirstPrompt = clip(strings.Join(strings.Fields(masked), " "), 240)
	}
	return p.append(Msg{Role: role, TS: ts, Text: masked, Sub: sub, Secret: secret, FullLen: full})
}

// addResult adds a tool's output: both of its ends, masked.
func (p *Parsed) addResult(ts, text, sub, toolID string, isErr bool) *Msg {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	full := len(text)
	masked, secret, clipped := maskEnds(text, maxResultText)
	if !clipped {
		full = 0
	}
	return p.append(Msg{Role: "result", TS: ts, Text: masked, Sub: sub, ToolID: toolID, IsErr: isErr, Secret: secret, FullLen: full})
}

// highlightMarks are what the API and its queries use to mark a match inside a
// snippet (see apiMarks).
// They are taken out of stored text, so a transcript cannot fake a highlight.
var highlightMarks = strings.NewReplacer("\x02", "", "\x03", "", "\ue000", "", "\ue001", "")

func (p *Parsed) append(m Msg) *Msg {
	m.Seq, m.Stream, m.Off = len(p.Msgs), p.stream, p.off
	m.Text = highlightMarks.Replace(m.Text)
	if m.Secret {
		p.Secrets++
	}
	p.Msgs = append(p.Msgs, m)
	return &p.Msgs[len(p.Msgs)-1]
}

// textOf flattens a tool result: a string or a list of text blocks.
func textOf(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var bs []block
	if json.Unmarshal(raw, &bs) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range bs {
		if b.Type == "text" {
			sb.WriteString(b.Text)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// argSummary picks the human-meaningful string arguments of a tool call
// (command, file path, pattern, prompt...) and leaves out big payloads.
func argSummary(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var parts []string
	for _, k := range []string{"command", "file_path", "path", "pattern", "description", "prompt", "query", "url", "subagent_type"} {
		if v, ok := m[k]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				// Masked before clipping, so a clip cannot leave half a value.
				m, _ := maskHead(s, 400)
				parts = append(parts, m)
			}
		}
	}
	return strings.Join(parts, " ")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// ParsePi reads a pi session file (session v3).
func (p *Parsed) ParsePi(r io.Reader) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var pos int64
	for {
		line, err := br.ReadBytes('\n')
		p.off = pos
		pos += int64(len(line))
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var rec struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Cwd       string `json:"cwd"`
				Timestamp string `json:"timestamp"`
				Version   any    `json:"version"`
				Summary   string `json:"summary"`
				Message   struct {
					Role       string          `json:"role"`
					Content    json.RawMessage `json:"content"`
					ToolCallID string          `json:"toolCallId"`
					IsError    bool            `json:"isError"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &rec) == nil {
				p.stamp(rec.Timestamp)
				switch rec.Type {
				case "session":
					p.Cwd, p.AgentSessionID = rec.Cwd, rec.ID
				case "compaction":
					p.add("assistant", rec.Timestamp, "[compaction summary] "+rec.Summary, "")
				case "message":
					role := rec.Message.Role
					if role == "toolResult" {
						role = "result"
					}
					c := bytes.TrimSpace(rec.Message.Content)
					if len(c) > 0 && c[0] == '"' {
						var s string
						_ = json.Unmarshal(c, &s)
						if role == "result" {
							p.result(rec.Timestamp, s, "", rec.Message.ToolCallID, rec.Message.IsError)
						} else {
							p.add(role, rec.Timestamp, s, "")
						}
					} else {
						var bs []block
						if json.Unmarshal(c, &bs) == nil {
							if role == "result" {
								var sb strings.Builder
								for _, b := range bs {
									if b.Type == "text" {
										sb.WriteString(b.Text + "\n")
									}
								}
								p.result(rec.Timestamp, sb.String(), "", rec.Message.ToolCallID, rec.Message.IsError)
							} else {
								p.blocks(role, rec.Timestamp, bs, "", "arguments")
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				if p.Title == "" {
					p.Title = clip(p.FirstPrompt, 80)
				}
				return nil
			}
			return err
		}
	}
}

var (
	commitMadeRE = regexp.MustCompile(`\[([A-Za-z0-9._/#-]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] ([^\n]{0,120})`)
	pushRE       = regexp.MustCompile(`(?m)^\s*(?:[0-9a-f]{7,40}\.\.|\+\s)?([0-9a-f]{7,40})\s+\S+\s+->\s+(\S+)`)
	seenRE       = regexp.MustCompile(`(?m)^commit ([0-9a-f]{40})\b`)
)

// commitsIn looks for commits in git's own output: "[branch sha] subject"
// after a commit, "old..new branch -> branch" after a push, and
// "commit <sha>" in git log/show.
func (p *Parsed) commitsIn(text string, a at) {
	if len(text) < 10 {
		return
	}
	for _, m := range commitMadeRE.FindAllStringSubmatch(text, 8) {
		p.commit(CommitRef{SHA: m[2], Rel: "made", Branch: m[1], Subject: strings.TrimSpace(m[3]), Seq: a.seq, Sub: a.sub, ToolID: a.id, TS: a.ts})
	}
	for _, m := range pushRE.FindAllStringSubmatch(text, 8) {
		p.commit(CommitRef{SHA: m[1], Rel: "pushed", Branch: m[2], Seq: a.seq, Sub: a.sub, ToolID: a.id, TS: a.ts})
	}
	for _, m := range seenRE.FindAllStringSubmatch(text, 8) {
		p.commit(CommitRef{SHA: m[1], Rel: "seen", Seq: a.seq, Sub: a.sub, ToolID: a.id, TS: a.ts})
	}
}

func (p *Parsed) commit(c CommitRef) {
	c.Subject, c.Branch = maskPlain(c.Subject), maskPlain(c.Branch)
	for i := range p.Commits {
		o := &p.Commits[i]
		if strings.HasPrefix(o.SHA, c.SHA) || strings.HasPrefix(c.SHA, o.SHA) {
			if len(c.SHA) > len(o.SHA) {
				o.SHA = c.SHA
			}
			if rank(c.Rel) > rank(o.Rel) || o.Seq < 0 && c.Seq >= 0 {
				if rank(c.Rel) > rank(o.Rel) {
					o.Rel = c.Rel
				}
				o.Seq, o.Sub, o.ToolID, o.TS = c.Seq, c.Sub, c.ToolID, c.TS
			}
			if o.Branch == "" {
				o.Branch = c.Branch
			}
			if o.Subject == "" {
				o.Subject = c.Subject
			}
			return
		}
	}
	p.Commits = append(p.Commits, c)
}

func rank(r string) int {
	switch r {
	case "made":
		return 3
	case "pushed":
		return 2
	}
	return 1
}

// RepoFromCwd guesses the repository name from a working directory:
// ~/.worktrees/<repo>/<branch> gives <repo>, otherwise the directory's
// last element.
func RepoFromCwd(cwd string) string {
	parts := strings.Split(filepath.ToSlash(cwd), "/")
	for i, p := range parts {
		if p == ".worktrees" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

func (p *Parsed) doc(tool string, raw json.RawMessage) {
	switch tool {
	case "Write", "Edit", "MultiEdit", "write", "edit":
	default:
		return
	}
	var m struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	f := m.FilePath
	if f == "" {
		f = m.Path
	}
	if !strings.HasSuffix(f, ".md") || !strings.Contains(f, "/docs/") {
		return
	}
	for _, d := range p.Docs {
		if d == f {
			return
		}
	}
	if len(p.Docs) < 20 {
		p.Docs = append(p.Docs, f)
	}
}
