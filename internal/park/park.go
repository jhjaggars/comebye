// Package park keeps what is needed to bring a closed agent back: the park
// record (session reference, launch arguments, worktree, branch) and the
// arguments that make each agent kind resume its own session.
//
// Parking closes an agent's pane and keeps its worktree, branch and session.
// The host's comebye writes the record before it closes the pane, so a park
// is recorded even when the hub is down; the hub only mirrors it.
package park

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record is one parked agent.
type Record struct {
	ID   string `json:"id"` // pk_ + 10 hex
	Name string `json:"name"`
	Kind string `json:"kind"` // claude, pi or opencode
	Host string `json:"host,omitempty"`
	// SessionKind is "id" (claude, opencode) or "path" (pi); Session is its value.
	SessionKind string    `json:"session_kind"`
	Session     string    `json:"session"`
	CWD         string    `json:"cwd"`
	Worktree    string    `json:"worktree"`
	Branch      string    `json:"branch,omitempty"`
	LaunchArgs  []string  `json:"launch_args,omitempty"`
	PaneID      string    `json:"pane_id,omitempty"` // the pane it had when parked
	Why         string    `json:"why,omitempty"`
	By          string    `json:"by,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"`
	Dirty       bool      `json:"dirty,omitempty"`
	DirtyFiles  int       `json:"dirty_files,omitempty"`
	ParkedAt    time.Time `json:"parked_at"`
	// Scratch: the agent's /tmp scratchpad was moved from ScratchFrom to
	// ScratchTo (on disk) at park, and goes back at resume.
	ScratchFrom  string `json:"scratch_from,omitempty"`
	ScratchTo    string `json:"scratch_to,omitempty"`
	ScratchBytes int64  `json:"scratch_bytes,omitempty"`
	ScratchNote  string `json:"scratch_note,omitempty"`
	// What parking keeps on disk, measured at park (a worktree walk is cut off
	// after a few seconds, so the figure may be a floor): for the "disk cost" line.
	WorktreeBytes int64 `json:"worktree_bytes,omitempty"`
	SessionBytes  int64 `json:"session_bytes,omitempty"`
	// Resume history: the record is kept after a resume as history.
	ResumedAt   *time.Time `json:"resumed_at,omitempty"`
	ResumedPane string     `json:"resumed_pane,omitempty"`
	Pinned      bool       `json:"pinned,omitempty"`
}

// Parked reports whether the agent is still closed.
func (r Record) Parked() bool { return r.ResumedAt == nil }

// Launch is how an agent was started: its kind and arguments (without the
// first prompt), kept so a resume can start it the same way.
type Launch struct {
	Kind string    `json:"kind"`
	Args []string  `json:"args,omitempty"`
	At   time.Time `json:"at"`
}

type file struct {
	Records  []Record          `json:"records"`
	Launches map[string]Launch `json:"launches,omitempty"` // by agent name
}

// Store is the host's park state: parked.json in the state directory.
type Store struct {
	mu   sync.Mutex
	path string
	f    file
}

// Open reads (or starts) the store in dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("no state directory")
	}
	s := &Store{path: filepath.Join(dir, "parked.json")}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.f); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
	}
	return s, nil
}

// save writes the file atomically (caller holds mu).
func (s *Store) save() error {
	b, err := json.MarshalIndent(&s.f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".parked-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// NewID makes a record id.
func NewID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "pk_" + hex.EncodeToString(b)
}

// Add stores a new record.
func (s *Store) Add(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Records = append(s.f.Records, r)
	return s.save()
}

// Update changes the record with id in place.
func (s *Store) Update(id string, fn func(*Record)) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.f.Records {
		if s.f.Records[i].ID == id {
			fn(&s.f.Records[i])
			return s.f.Records[i], s.save()
		}
	}
	return Record{}, ErrNotFound
}

// Delete drops a record.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.f.Records {
		if s.f.Records[i].ID == id {
			s.f.Records = append(s.f.Records[:i], s.f.Records[i+1:]...)
			return s.save()
		}
	}
	return ErrNotFound
}

// ErrNotFound means no record has that id.
var ErrNotFound = errors.New("no such park record")

// Get returns the record with id.
func (s *Store) Get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.f.Records {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// List returns the records, newest park first. With parkedOnly the ones
// already resumed are left out.
func (s *Store) List(parkedOnly bool) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.f.Records {
		if parkedOnly && !r.Parked() {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ParkedAt.After(out[j].ParkedAt) })
	return out
}

// Prune drops resumed records older than keep (the history window).
func (s *Store) Prune(keep time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.f.Records[:0]
	for _, r := range s.f.Records {
		if r.ResumedAt != nil && now.Sub(*r.ResumedAt) > keep {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) != len(s.f.Records) {
		s.f.Records = kept
		_ = s.save()
	}
}

// NoteLaunch remembers how the agent called name was started.
func (s *Store) NoteLaunch(name, kind string, args []string, at time.Time) {
	if name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f.Launches == nil {
		s.f.Launches = map[string]Launch{}
	}
	s.f.Launches[name] = Launch{Kind: kind, Args: LaunchArgs(args), At: at}
	// Keep the table small: forget the oldest beyond 500 names.
	if len(s.f.Launches) > 500 {
		var oldest string
		for n, l := range s.f.Launches {
			if oldest == "" || l.At.Before(s.f.Launches[oldest].At) {
				oldest = n
			}
		}
		delete(s.f.Launches, oldest)
	}
	_ = s.save()
}

// LaunchOf returns how the agent called name was last started here.
func (s *Store) LaunchOf(name string) (Launch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.f.Launches[name]
	return l, ok
}

// LaunchArgs drops the first prompt from a launch's arguments: everything from
// the "--" that ends option parsing.
func LaunchArgs(args []string) []string {
	for i, a := range args {
		if a == "--" {
			args = args[:i]
			break
		}
	}
	return append([]string(nil), args...)
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Kinds are the agent kinds that can be parked.
var Kinds = []string{"claude", "pi", "opencode"}

// Supported reports whether kind can be parked and resumed.
func Supported(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ResumeArgs are the arguments that start kind on its own session, then the
// arguments it was launched with. The resume flag goes first: claude's
// --allowedTools takes everything after it.
func ResumeArgs(kind, sessionKind, session string, launch []string) ([]string, error) {
	var flag string
	switch kind {
	case "claude":
		if sessionKind != "id" || !idRe.MatchString(session) {
			return nil, fmt.Errorf("claude resumes by session id; %q is not one", session)
		}
		flag = "--resume"
	case "opencode":
		if sessionKind != "id" || !idRe.MatchString(session) {
			return nil, fmt.Errorf("opencode resumes by session id; %q is not one", session)
		}
		flag = "-s"
	case "pi":
		if sessionKind != "path" || !filepath.IsAbs(session) || strings.ContainsRune(session, 0) {
			return nil, fmt.Errorf("pi resumes by session file path; %q is not one", session)
		}
		flag = "--session"
	default:
		return nil, fmt.Errorf("agent kind %q cannot be parked", kind)
	}
	return append([]string{flag, session}, LaunchArgs(launch)...), nil
}

// MoveTree moves directory src to dst, across file systems if need be (a
// RAM-backed /tmp to disk): a rename when it works, else a copy and a remove.
// It returns the bytes moved. dst must not exist.
func MoveTree(src, dst string) (int64, error) {
	if _, err := os.Lstat(dst); err == nil {
		return 0, fmt.Errorf("%s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	if err := os.Rename(src, dst); err == nil {
		return treeSize(dst), nil
	}
	n, err := copyTree(src, dst)
	if err != nil {
		os.RemoveAll(dst)
		return 0, err
	}
	if err := os.RemoveAll(src); err != nil {
		return n, fmt.Errorf("copied, but could not remove %s: %w", src, err)
	}
	return n, nil
}

// TreeSize is the bytes of the regular files under p, counting for at most limit
// (the figure is a floor when the walk was cut off).
func TreeSize(p string, limit time.Duration) int64 {
	var n int64
	stop := time.Now().Add(limit)
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if time.Now().After(stop) {
			return filepath.SkipAll
		}
		if err == nil && d.Type().IsRegular() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// DeleteForWorktree drops the parked records whose worktree is wt (it was removed).
func (s *Store) DeleteForWorktree(wt string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.f.Records[:0]
	var gone []Record
	for _, r := range s.f.Records {
		if r.Parked() && r.Worktree == wt {
			gone = append(gone, r)
			continue
		}
		kept = append(kept, r)
	}
	if len(gone) > 0 {
		s.f.Records = kept
		_ = s.save()
	}
	return gone
}

func treeSize(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func copyTree(src, dst string) (int64, error) {
	var n int64
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(to, fi.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, to)
		case d.Type().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
			if err != nil {
				return err
			}
			c, err := io.Copy(out, in)
			n += c
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			return err
		}
		return nil // sockets, devices: not copied
	})
	return n, err
}
