package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Source is a directory of one agent kind's session files.
type Source struct {
	Kind string // claude or pi
	Root string
	// SkipSubagents leaves out Claude's subagent transcripts (their tokens
	// are real spend; this is for comparing with a count that missed them).
	SkipSubagents bool
}

// FileState is how far a transcript has been read. Offsets move only past
// whole lines, so a line still being written is read again, complete, on
// the next scan.
type FileState struct {
	Kind string `json:"kind"`
	SID  string `json:"sid"`
	Sub  bool   `json:"sub,omitempty"` // a subagent's transcript
	Off  int64  `json:"off"`
	Size int64  `json:"size"`
	Mod  int64  `json:"mod"`
	// The last reply read: Claude writes a reply once per content block, each
	// line with the same message id and the usage so far, so a line with the
	// same id replaces the one before.
	LastID string `json:"last_id,omitempty"`
	LastH  int64  `json:"last_h,omitempty"`
	LastM  string `json:"last_m,omitempty"`
	LastT  Tokens `json:"last_t,omitempty"`
}

// ScanStats says what a scan did.
type ScanStats struct {
	Files   int // transcripts looked at
	Read    int // of those, read (new or grown)
	Bytes   int64
	Samples int
}

type fileJob struct {
	path string
	kind string
	sid  string
	sub  bool
}

// listFiles finds the transcripts under src.
func listFiles(src Source) []fileJob {
	var jobs []fileJob
	switch src.Kind {
	case "claude":
		a, _ := filepath.Glob(filepath.Join(src.Root, "*", "*.jsonl"))
		for _, p := range a {
			jobs = append(jobs, fileJob{p, "claude", strings.TrimSuffix(filepath.Base(p), ".jsonl"), false})
		}
		// A subagent's transcript is <project>/<session>/subagents/<agent>.jsonl and
		// counts toward <session>.
		var b []string
		if !src.SkipSubagents {
			b, _ = filepath.Glob(filepath.Join(src.Root, "*", "*", "subagents", "*.jsonl"))
		}
		for _, p := range b {
			sid := filepath.Base(filepath.Dir(filepath.Dir(p)))
			jobs = append(jobs, fileJob{p, "claude", sid, true})
		}
	case "pi":
		a, _ := filepath.Glob(filepath.Join(src.Root, "*", "*.jsonl"))
		for _, p := range a {
			jobs = append(jobs, fileJob{p, "pi", PiSessionID(filepath.Base(p)), false})
		}
	}
	return jobs
}

// fileResult is what reading one transcript added.
type fileResult struct {
	fs          FileState
	deltas      map[rowKey]Tokens
	first, last int64
	cwd, branch string
	samples     int
}

// readFile reads path from fs.Off and returns the new counts. It holds no
// lock: the store applies the result.
func readFile(path string, fs FileState) (*fileResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < fs.Off {
		// Shrunk: replaced by another file. What it added before stays counted.
		fs.Off, fs.LastID, fs.LastT = 0, "", Tokens{}
	}
	if _, err := f.Seek(fs.Off, io.SeekStart); err != nil {
		return nil, err
	}
	res := &fileResult{deltas: map[rowKey]Tokens{}}
	add := func(h int64, m string, d Tokens) {
		if d.IsZero() {
			return
		}
		k := rowKey{h, m}
		t := res.deltas[k]
		t.Add(d)
		res.deltas[k] = t
	}
	r := bufio.NewReaderSize(f, 256<<10)
	var carry []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			carry = append(carry, chunk...)
			continue
		}
		line := chunk
		if len(carry) > 0 {
			line = append(carry, chunk...)
			carry = carry[:0]
		}
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		if err != nil && !complete {
			// The end of the file. A last line without its newline is read only
			// when it is whole JSON; otherwise it is still being written.
			if len(line) > 0 && json.Valid(line) {
				complete = true
			} else {
				break
			}
		}
		fs.Off += int64(len(line))
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			var s *Sample
			var meta lineMeta
			var ok bool
			if fs.Kind == "pi" {
				s, meta, ok = ParsePi(line)
			} else {
				s, meta, ok = ParseClaude(line, fs.Sub)
			}
			if ok {
				if meta.Cwd != "" {
					res.cwd = meta.Cwd
				}
				if meta.Branch != "" {
					res.branch = meta.Branch
				}
				if s != nil && s.Model != "<synthetic>" && !(s.Tok.Fresh() == 0 && s.Tok.CR == 0) {
					applySample(&fs, res, s, add)
				}
			}
		}
		if err != nil {
			break
		}
	}
	fs.Size, fs.Mod = st.Size(), st.ModTime().UnixNano()
	res.fs = fs
	return res, nil
}

func applySample(fs *FileState, res *fileResult, s *Sample, add func(int64, string, Tokens)) {
	h := HourOf(s.At)
	if s.ID != "" && s.ID == fs.LastID {
		// The same reply again, with the usage so far: replace the last.
		add(fs.LastH, fs.LastM, Tokens{}.Minus(fs.LastT))
	}
	add(h, s.Model, s.Tok)
	if s.ID != "" {
		fs.LastID, fs.LastH, fs.LastM, fs.LastT = s.ID, h, s.Model, s.Tok
	}
	res.samples++
	u := s.At.Unix()
	if res.first == 0 || u < res.first {
		res.first = u
	}
	if u > res.last {
		res.last = u
	}
}

func fileKey(j fileJob) string { return j.path }

// Scan reads the transcripts under srcs from where the last scan stopped
// and adds their counts to the store's local host. Files not touched since
// the retention window are not read at all.
func (st *Store) Scan(ctx context.Context, srcs []Source) (ScanStats, error) {
	var stats ScanStats
	cutoff := st.now().Add(-SessionRetention)
	lastFlush := time.Now()
	for _, src := range srcs {
		for _, j := range listFiles(src) {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			info, err := os.Stat(j.path)
			if err != nil {
				continue
			}
			stats.Files++
			st.mu.Lock()
			prev, seen := st.files[fileKey(j)]
			var fs FileState
			if seen {
				fs = *prev
			}
			st.mu.Unlock()
			if seen && fs.Size == info.Size() && fs.Mod == info.ModTime().UnixNano() {
				continue
			}
			if !seen && info.ModTime().Before(cutoff) {
				continue
			}
			fs.Kind, fs.SID, fs.Sub = j.kind, j.sid, j.sub
			res, err := readFile(j.path, fs)
			if err != nil {
				continue
			}
			stats.Read++
			stats.Bytes += res.fs.Off - fs.Off
			stats.Samples += res.samples
			st.apply(j, res)
			if time.Since(lastFlush) > 30*time.Second {
				_ = st.Save()
				lastFlush = time.Now()
			}
		}
	}
	return stats, nil
}

// apply adds a file's result to its session.
func (st *Store) apply(j fileJob, res *fileResult) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := res.fs
	st.files[fileKey(j)] = &cp
	st.dirty = true
	key := st.host + "/" + j.kind + "/" + j.sid
	s := st.sessions[key]
	if len(res.deltas) == 0 && res.cwd == "" && s == nil {
		return
	}
	if s == nil {
		if len(res.deltas) == 0 {
			return
		}
		s = &Session{Host: st.host, Kind: j.kind, ID: j.sid}
		st.sessions[key] = s
	}
	changed := false
	for k, d := range res.deltas {
		s.add(k.h, k.m, d)
		changed = true
	}
	if res.first != 0 && (s.First == 0 || res.first < s.First) {
		s.First, changed = res.first, true
	}
	if res.last > s.Last {
		s.Last, changed = res.last, true
	}
	// The main transcript names the directory; a subagent's only fills a gap.
	if res.cwd != "" && (!j.sub || s.Cwd == "") && res.cwd != s.Cwd {
		s.Cwd, changed = res.cwd, true
	}
	if res.branch != "" && (!j.sub || s.Branch == "") && res.branch != s.Branch {
		s.Branch, changed = res.branch, true
	}
	if changed {
		s.Upd = st.nextUpd()
	}
}
