package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Claude sessions are append-only and the phone polls them every few
// seconds, so parsed state is cached per file and only newly appended
// complete lines are parsed on each read. A file that shrinks or is
// replaced is parsed from scratch.
type claudeCacheEntry struct {
	mu      sync.Mutex
	fi      os.FileInfo
	offset  int64 // bytes consumed (always at a line boundary)
	p       *claudeParser
	lastUse time.Time
}

const claudeCacheMax = 16

var claudeCache = struct {
	sync.Mutex
	m map[string]*claudeCacheEntry
}{m: map[string]*claudeCacheEntry{}}

func claudeEntry(path string) *claudeCacheEntry {
	claudeCache.Lock()
	defer claudeCache.Unlock()
	e, ok := claudeCache.m[path]
	if !ok {
		if len(claudeCache.m) >= claudeCacheMax {
			var oldestKey string
			var oldest time.Time
			for k, v := range claudeCache.m {
				if oldestKey == "" || v.lastUse.Before(oldest) {
					oldestKey, oldest = k, v.lastUse
				}
			}
			delete(claudeCache.m, oldestKey)
		}
		e = &claudeCacheEntry{}
		claudeCache.m[path] = e
	}
	e.lastUse = time.Now()
	return e
}

// readClaude parses (incrementally) the Claude Code session open as f and
// returns the requested window.
func readClaude(f *os.File, path string, limit, skip int) (*Transcript, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	e := claudeEntry(path)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.p == nil || e.fi == nil || !os.SameFile(e.fi, fi) || fi.Size() < e.offset {
		e.p, e.offset = newClaudeParser(), 0
	}
	if fi.Size() > e.offset {
		if _, err := f.Seek(e.offset, io.SeekStart); err != nil {
			return nil, err
		}
		n, err := feedLines(bufio.NewReaderSize(io.LimitReader(f, fi.Size()-e.offset), 256*1024), e.p.feed)
		e.offset += n
		if err != nil {
			e.p, e.offset = nil, 0
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	e.fi = fi
	// Copy the window: the cached slice is patched in place by later reads
	// while the caller may still be encoding this one.
	t := window(e.p.all, limit, skip, e.p.ask, FormatClaude)
	t.Messages = append([]Message(nil), t.Messages...)
	return t, nil
}

// feedLines hands each complete newline-terminated line to fn and returns
// the bytes consumed. A trailing partial line (still being written) is not
// consumed.
func feedLines(r *bufio.Reader, fn func([]byte)) (int64, error) {
	var n int64
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n += int64(len(line))
		fn(line)
	}
}

// claudeLatestFrom is LatestOf for Claude sessions: the last assistant text,
// and the tool calls of the current assistant message still lacking a
// result (Claude's permission prompt blocks on one of those).
func claudeLatestFrom(r io.Reader, skipFirst bool) (*Latest, error) {
	st := &claudeLatest{out: &Latest{}, pending: map[string]PendingTool{}}
	br := bufio.NewReaderSize(r, 256*1024)
	first := true
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && !(first && skipFirst) { // the seek may land mid-record
			st.line(line)
		}
		first = false
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	for _, id := range st.order {
		if p, ok := st.pending[id]; ok {
			st.out.Pending = append(st.out.Pending, p)
		}
	}
	return st.out, nil
}

type claudeLatest struct {
	out      *Latest
	curMsgID string
	order    []string
	pending  map[string]PendingTool
}

func (st *claudeLatest) reset() {
	st.order = st.order[:0]
	st.pending = map[string]PendingTool{}
}

func (st *claudeLatest) line(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var rec claudeRecord
	if json.Unmarshal(line, &rec) != nil || rec.IsSidechain || rec.IsMeta || rec.Message == nil {
		return
	}
	blocks, s, isStr := decodeClaudeBlocks(rec.Message.Content)
	switch rec.Type {
	case "assistant":
		if rec.Message.ID == "" || rec.Message.ID != st.curMsgID {
			st.curMsgID = rec.Message.ID
			st.reset()
		}
		if isStr {
			blocks = []claudeBlock{{Type: "text", Text: s}}
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					st.out.Text, st.out.Time = t, rec.Timestamp
				}
			case "tool_use":
				if b.ID != "" {
					st.order = append(st.order, b.ID)
					st.pending[b.ID] = PendingTool{Tool: b.Name, Args: summarizeArgs(b.Input)}
				}
			}
		}
	case "user":
		if rec.IsCompactSummary {
			return
		}
		results := false
		for _, b := range blocks {
			if b.Type == "tool_result" {
				delete(st.pending, b.ToolUseID)
				results = true
			}
		}
		if !results {
			// A new user turn (or an interrupt) supersedes what was pending.
			st.curMsgID = ""
			st.reset()
		}
	}
}
