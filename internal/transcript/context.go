package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// Context is how much a Claude session's last request carried: the size of
// the context window the model was given, which is what grows until the
// session is cleared or compacted.
type Context struct {
	// Tokens is the last request's input_tokens plus its cache creation and
	// cache read tokens.
	Tokens int `json:"tokens"`
	// Model is the model that answered it; Provider is Pi's (and
	// OpenCode's) provider for it, empty for Claude.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	// At is when that request was answered (RFC 3339), if the line says.
	At string `json:"at,omitempty"`
	// Compacted is when the session was last compacted, if that is within
	// the part of the file read (RFC 3339).
	Compacted string `json:"compacted,omitempty"`
	// Window is the model's context window in tokens, 0 when not known.
	// The reader does not know it; the server fills it in.
	Window int `json:"window,omitempty"`
}

// contextTail is how much of the file's end is looked at first, and
// contextMaxTail the most (a turn of huge tool output can push the last
// assistant line a long way from the end).
const (
	contextTail    = 256 << 10
	contextMaxTail = 8 << 20
)

// LastContext reads the context size of the Claude session at path from
// its last assistant line that has usage. Lines of subagents (sidechains)
// and synthetic ones (an error the client made up, with zero usage) are
// skipped. It reports false when the file has no such line. Only the end of
// the file is read.
func LastContext(path string) (Context, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Context{}, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Context{}, false, err
	}
	size := fi.Size()
	for tail := int64(contextTail); ; tail *= 4 {
		if tail > contextMaxTail {
			tail = contextMaxTail
		}
		start := size - tail
		if start < 0 {
			start = 0
		}
		buf := make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return Context{}, false, err
		}
		if start > 0 { // the first line is cut: drop it
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				buf = nil
			}
		}
		c, ok, stale := lastContextIn(buf)
		if ok {
			return c, true, nil
		}
		if stale {
			return Context{}, false, nil
		}
		if start == 0 || tail >= contextMaxTail {
			return Context{}, false, nil
		}
	}
}

// lastContextIn finds the last usable assistant line in buf, in either
// session format: Claude's (input_tokens and the cache counts under
// message.usage) or Pi's (input, cacheRead and cacheWrite; OpenCode's mirror
// is Pi's format). A compaction after that line means the figure is from
// before it, so nothing is reported rather than a stale number; a compaction
// before it is reported as when the context last shrank.
func lastContextIn(buf []byte) (c Context, found, stale bool) {
	for len(buf) > 0 {
		end := len(buf)
		if buf[end-1] == '\n' {
			end--
		}
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		line := buf[start:end]
		buf = buf[:start]
		if len(line) == 0 {
			continue
		}
		if at, ok := compactionAt(line); ok {
			if !found {
				return Context{}, false, true // the newest figure predates this compaction
			}
			c.Compacted = at
			return c, true, false
		}
		if found || !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var l struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Timestamp   string `json:"timestamp"`
			Message     struct {
				Role     string `json:"role"`
				Provider string `json:"provider"`
				Model    string `json:"model"`
				Usage    struct {
					Input         int `json:"input_tokens"`
					CacheCreation int `json:"cache_creation_input_tokens"`
					CacheRead     int `json:"cache_read_input_tokens"`
					PiInput       int `json:"input"`
					PiCacheRead   int `json:"cacheRead"`
					PiCacheWrite  int `json:"cacheWrite"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil || l.IsSidechain {
			continue
		}
		m := l.Message
		if !(l.Type == "assistant" || (l.Type == "message" && m.Role == "assistant")) {
			continue
		}
		u := m.Usage
		n := u.Input + u.CacheCreation + u.CacheRead + u.PiInput + u.PiCacheRead + u.PiCacheWrite
		if n == 0 || m.Model == "<synthetic>" {
			continue
		}
		c = Context{Tokens: n, Model: m.Model, Provider: m.Provider, At: l.Timestamp}
		found = true
	}
	return c, found, false
}

// compactionAt reports whether line marks a compaction (Claude's
// compact_boundary system line, Pi's compaction record) and when.
func compactionAt(line []byte) (string, bool) {
	if !bytes.Contains(line, []byte(`"compact`)) {
		return "", false
	}
	var l struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(line, &l) != nil {
		return "", false
	}
	if l.Type == "compaction" || (l.Type == "system" && l.Subtype == "compact_boundary") {
		return l.Timestamp, true
	}
	return "", false
}

// FileContains reports whether the file at path contains needle (a streamed
// scan; the marker a hook left in a session is looked for with it).
func FileContains(path string, needle []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	keep := len(needle) - 1
	have := 0
	for {
		n, err := f.Read(buf[have:])
		have += n
		if bytes.Contains(buf[:have], needle) {
			return true, nil
		}
		if err != nil {
			if err == io.EOF {
				return false, nil
			}
			return false, err
		}
		if have > keep {
			copy(buf, buf[have-keep:have])
			have = keep
		}
	}
}
