// Package opencode makes OpenCode sessions readable like the other kinds'.
//
// OpenCode keeps its sessions in a SQLite database, not in a file per
// session, so nothing that reads a session file (the chat view, the archive,
// recall and Search, usage) can see them. This package mirrors a session as a
// file in Pi's session format (one JSON object per line: a session header,
// then messages), from `opencode export <id>`. Everything that reads Pi
// sessions then reads these too, as kind "opencode": one mechanism, not a
// second reader for each consumer.
//
// The mirror only ever grows by whole lines (an archive copies it append-only):
// a message is written once it is complete, and nothing after an unfinished
// message is written until that one is.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Kind is the kind name of mirrored sessions (herdr's agent id for OpenCode).
const Kind = "opencode"

type exportDoc struct {
	Info struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
		Time      struct {
			Created int64 `json:"created"`
		} `json:"time"`
	} `json:"info"`
	Messages []struct {
		Info struct {
			ID         string `json:"id"`
			ParentID   string `json:"parentID"`
			Role       string `json:"role"`
			ProviderID string `json:"providerID"`
			ModelID    string `json:"modelID"`
			Model      struct {
				ProviderID string `json:"providerID"`
				ModelID    string `json:"modelID"`
			} `json:"model"`
			Time struct {
				Created   int64 `json:"created"`
				Completed int64 `json:"completed"`
			} `json:"time"`
			Finish string `json:"finish"`
			Error  any    `json:"error"`
			Tokens struct {
				Total     int64 `json:"total"`
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"info"`
		Parts []struct {
			Type   string     `json:"type"`
			Text   string     `json:"text"`
			Tool   string     `json:"tool"`
			CallID string     `json:"callID"`
			State  *toolState `json:"state"`
		} `json:"parts"`
	} `json:"messages"`
}

type toolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"`
	Error  string          `json:"error"`
	Time   struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"time"`
}

func ts(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z") }

// Convert turns the JSON of `opencode export <id>` into a Pi-format session
// file, and returns the session's id and working directory. Messages that are
// not finished yet (and everything after them) are left out.
func Convert(export []byte) (jsonl []byte, id, cwd string, err error) {
	var d exportDoc
	if err := json.Unmarshal(export, &d); err != nil {
		return nil, "", "", fmt.Errorf("opencode export: %w", err)
	}
	if d.Info.ID == "" {
		return nil, "", "", errors.New("opencode export: no session id")
	}
	var buf bytes.Buffer
	emit := func(v any) {
		b, _ := json.Marshal(v)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	type obj = map[string]any
	emit(obj{"type": "session", "version": 3, "id": d.Info.ID, "timestamp": ts(d.Info.Time.Created), "cwd": d.Info.Directory})
	parent := any(nil)
	for _, m := range d.Messages {
		in := m.Info
		switch in.Role {
		case "user":
			var blocks []obj
			for _, p := range m.Parts {
				if p.Type == "text" && p.Text != "" {
					blocks = append(blocks, obj{"type": "text", "text": p.Text})
				}
			}
			if len(blocks) == 0 {
				continue
			}
			emit(obj{"type": "message", "id": in.ID, "parentId": parent, "timestamp": ts(in.Time.Created),
				"message": obj{"role": "user", "content": blocks, "timestamp": in.Time.Created}})
			parent = in.ID
		case "assistant":
			if in.Time.Completed == 0 {
				return finish(&buf, &d)
			}
			var blocks []obj
			var results []obj
			pending := false
			for _, p := range m.Parts {
				switch p.Type {
				case "reasoning":
					if p.Text != "" {
						blocks = append(blocks, obj{"type": "thinking", "thinking": p.Text})
					}
				case "text":
					if p.Text != "" {
						blocks = append(blocks, obj{"type": "text", "text": p.Text})
					}
				case "tool":
					if p.State == nil {
						continue
					}
					args := p.State.Input
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					blocks = append(blocks, obj{"type": "toolCall", "id": p.CallID, "name": p.Tool, "arguments": args})
					switch p.State.Status {
					case "completed", "error":
						text, isErr := p.State.Output, p.State.Status == "error"
						if isErr {
							text = p.State.Error
						}
						r := obj{"role": "toolResult", "toolCallId": p.CallID, "toolName": p.Tool,
							"content": []obj{{"type": "text", "text": text}}, "isError": isErr}
						if p.State.Time.End > p.State.Time.Start && p.State.Time.Start > 0 {
							r["durationMs"] = p.State.Time.End - p.State.Time.Start
						}
						results = append(results, r)
					default:
						pending = true
					}
				}
			}
			if pending {
				return finish(&buf, &d)
			}
			model, provider := in.ModelID, in.ProviderID
			if model == "" {
				model, provider = in.Model.ModelID, in.Model.ProviderID
			}
			t := in.Tokens
			emit(obj{"type": "message", "id": in.ID, "parentId": parent, "timestamp": ts(in.Time.Completed),
				"message": obj{"role": "assistant", "content": blocks, "provider": provider, "model": model,
					"usage": obj{"input": t.Input, "output": t.Output, "cacheRead": t.Cache.Read, "cacheWrite": t.Cache.Write,
						"reasoning": t.Reasoning, "totalTokens": t.Total},
					"stopReason": in.Finish, "timestamp": in.Time.Completed}})
			parent = in.ID
			for i, r := range results {
				rid := fmt.Sprintf("%s.%d", in.ID, i)
				emit(obj{"type": "message", "id": rid, "parentId": parent, "timestamp": ts(in.Time.Completed), "message": r})
				parent = rid
			}
		}
	}
	return finish(&buf, &d)
}

func finish(buf *bytes.Buffer, d *exportDoc) ([]byte, string, string, error) {
	return buf.Bytes(), d.Info.ID, d.Info.Directory, nil
}

var slugRE = regexp.MustCompile(`[^A-Za-z0-9]`)

// Slug is the directory name for sessions of cwd, like Claude's project
// directories ("/home/me/x" -> "-home-me-x").
func Slug(cwd string) string {
	if cwd == "" {
		return "-"
	}
	return slugRE.ReplaceAllString(cwd, "-")
}

// DefaultDir is where the mirror lives when none is configured.
func DefaultDir() string {
	if d := os.Getenv("COMEBYE_STATE_DIR"); d != "" {
		return filepath.Join(d, "opencode-sessions")
	}
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "comebye", "opencode-sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "comebye", "opencode-sessions")
}

// ValidID reports whether id is an OpenCode session id ("ses_…"), safe to
// use in a file name and as an argument.
func ValidID(id string) bool {
	return regexp.MustCompile(`^ses_[A-Za-z0-9]{8,64}$`).MatchString(id)
}

// Mirror writes mirrored sessions under Dir.
type Mirror struct {
	Dir string
	// Export runs `opencode export id` and returns its JSON. Nil runs the
	// opencode program at Bin (or on PATH).
	Export func(ctx context.Context, id string) ([]byte, error)
	Bin    string
	// MinGap is the least time between two exports of one session
	// (default 5s).
	MinGap time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

func (m *Mirror) export(ctx context.Context, id string) ([]byte, error) {
	if m.Export != nil {
		return m.Export(ctx, id)
	}
	bin := m.Bin
	if bin == "" {
		bin = "opencode"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "export", id).Output()
	if err != nil {
		return nil, fmt.Errorf("opencode export: %w", err)
	}
	// The program prints a line before the JSON on some versions.
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	return out, nil
}

// Forget lets the next Sync of id export at once.
func (m *Mirror) Forget(id string) {
	m.mu.Lock()
	delete(m.last, id)
	m.mu.Unlock()
}

// Path is the mirrored file of session id when one exists.
func (m *Mirror) Path(id string) string {
	if !ValidID(id) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(m.Dir, "*", id+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// Sync refreshes the mirror of session id and returns its path. Exports of
// one session are at least MinGap apart; in between the file as it is serves.
func (m *Mirror) Sync(ctx context.Context, id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("not an opencode session id: %q", id)
	}
	m.mu.Lock()
	if m.last == nil {
		m.last = map[string]time.Time{}
	}
	gap := m.MinGap
	if gap == 0 {
		gap = 5 * time.Second
	}
	if t, ok := m.last[id]; ok && time.Since(t) < gap {
		m.mu.Unlock()
		return m.Path(id), nil
	}
	m.last[id] = time.Now()
	m.mu.Unlock()

	raw, err := m.export(ctx, id)
	if err != nil {
		return m.Path(id), err
	}
	jsonl, sid, cwd, err := Convert(raw)
	if err != nil {
		return m.Path(id), err
	}
	if sid != id {
		return m.Path(id), fmt.Errorf("opencode export gave session %s, wanted %s", sid, id)
	}
	dir := filepath.Join(m.Dir, Slug(cwd))
	path := filepath.Join(dir, id+".jsonl")
	if old, err := os.ReadFile(path); err == nil {
		if bytes.Equal(old, jsonl) {
			return path, nil
		}
		// Never shorten or rewrite what was written: the archive follows
		// the file as an append-only stream.
		if !bytes.HasPrefix(jsonl, old) {
			if len(jsonl) <= len(old) {
				return path, nil
			}
			return path, errors.New("opencode session changed under its mirror; keeping the earlier copy")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(jsonl)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}
