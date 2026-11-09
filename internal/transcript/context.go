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
	// Model is the model that answered it.
	Model string `json:"model,omitempty"`
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
		if c, ok := lastContextIn(buf); ok {
			return c, true, nil
		}
		if start == 0 || tail >= contextMaxTail {
			return Context{}, false, nil
		}
	}
}

// lastContextIn finds the last usable assistant line in buf.
func lastContextIn(buf []byte) (Context, bool) {
	for len(buf) > 0 {
		end := len(buf)
		if buf[end-1] == '\n' {
			end--
		}
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		line := buf[start:end]
		buf = buf[:start]
		if len(line) == 0 || !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var l struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Model string `json:"model"`
				Usage struct {
					Input         int `json:"input_tokens"`
					CacheCreation int `json:"cache_creation_input_tokens"`
					CacheRead     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.IsSidechain {
			continue
		}
		u := l.Message.Usage
		n := u.Input + u.CacheCreation + u.CacheRead
		if n == 0 || l.Message.Model == "<synthetic>" {
			continue
		}
		return Context{Tokens: n, Model: l.Message.Model}, true
	}
	return Context{}, false
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
