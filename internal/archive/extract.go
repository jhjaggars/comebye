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
}

// CommitRef is a commit a session made, pushed or saw.
type CommitRef struct {
	SHA     string
	Rel     string // made | pushed | seen
	Branch  string
	Subject string
}

// Parsed is what the index learns from one session's files.
type Parsed struct {
	Cwd, Branch, Title, FirstPrompt, Version, Start, End, AgentSessionID string
	Msgs                                                                 []Msg
	Commits                                                              []CommitRef
}

const (
	maxMsgText    = 4000 // per message, indexed
	maxResultText = 1500 // per tool result, indexed
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
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"`
	Content   json.RawMessage `json:"content"`
}

// ParseClaude reads a Claude Code session file (or a subagent file; sub is
// its id) and appends to p.
func (p *Parsed) ParseClaude(r io.Reader, sub string) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
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
			p.Title = rec.AITitle
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
			s := b.Name + " " + argSummary(args)
			p.add("tool", ts, s, sub)
			p.commitsIn(argSummary(args), "")
		case "tool_result":
			t := textOf(b.Content)
			p.commitsIn(t, "")
			p.add("result", ts, clip(t, maxResultText), sub)
		}
	}
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

func (p *Parsed) add(role, ts, text, sub string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if role == "user" && sub == "" && p.FirstPrompt == "" && !strings.HasPrefix(text, "<") {
		p.FirstPrompt = clip(strings.Join(strings.Fields(text), " "), 240)
	}
	p.Msgs = append(p.Msgs, Msg{Seq: len(p.Msgs), Role: role, TS: ts, Text: clip(text, maxMsgText), Sub: sub})
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
				parts = append(parts, clip(s, 400))
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
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var rec struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Cwd       string `json:"cwd"`
				Timestamp string `json:"timestamp"`
				Version   any    `json:"version"`
				Summary   string `json:"summary"`
				Message   struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
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
						p.add(role, rec.Timestamp, s, "")
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
								p.commitsIn(sb.String(), "")
								p.add("result", rec.Timestamp, clip(sb.String(), maxResultText), "")
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
func (p *Parsed) commitsIn(text, _ string) {
	if len(text) < 10 {
		return
	}
	for _, m := range commitMadeRE.FindAllStringSubmatch(text, 8) {
		p.commit(CommitRef{SHA: m[2], Rel: "made", Branch: m[1], Subject: strings.TrimSpace(m[3])})
	}
	for _, m := range pushRE.FindAllStringSubmatch(text, 8) {
		p.commit(CommitRef{SHA: m[1], Rel: "pushed", Branch: m[2]})
	}
	for _, m := range seenRE.FindAllStringSubmatch(text, 8) {
		p.commit(CommitRef{SHA: m[1], Rel: "seen"})
	}
}

func (p *Parsed) commit(c CommitRef) {
	for i := range p.Commits {
		o := &p.Commits[i]
		if strings.HasPrefix(o.SHA, c.SHA) || strings.HasPrefix(c.SHA, o.SHA) {
			if len(c.SHA) > len(o.SHA) {
				o.SHA = c.SHA
			}
			if rank(c.Rel) > rank(o.Rel) {
				o.Rel = c.Rel
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
