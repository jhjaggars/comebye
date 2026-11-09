package usage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// archKey identifies an hourly rollup: what is kept of a session once its
// per-session rows have expired.
type archKey struct {
	H                               int64
	Host, Kind, Model               string
	Project, Role, Purpose, Handoff string
}

type archRow struct {
	H       int64  `json:"h"`
	Host    string `json:"host"`
	Kind    string `json:"kind"`
	Model   string `json:"m"`
	Project string `json:"project,omitempty"`
	Role    string `json:"role,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Handoff string `json:"handoff,omitempty"`
	T       Tokens `json:"t"`
}

// PeerState is what the hub knows about gathering from one peer.
type PeerState struct {
	Epoch  string `json:"epoch,omitempty"`  // the peer store's identity; a new one resets the cursor
	Cursor int64  `json:"cursor,omitempty"` // the largest Upd gathered
	Home   string `json:"home,omitempty"`   // the peer's home directory
	OK     int64  `json:"ok,omitempty"`     // unix seconds of the last good gather
	Err    string `json:"err,omitempty"`
}

// Store holds every host's counts: the local host's from scanning, the
// peers' from gathering. It is a single JSON file, written atomically.
type Store struct {
	mu       sync.Mutex
	path     string
	host     string
	home     string
	epoch    string
	sessions map[string]*Session
	files    map[string]*FileState
	archive  map[archKey]Tokens
	peers    map[string]*PeerState
	seq      int64
	dirty    bool
	// now is the clock (tests replace it).
	now func() time.Time
}

type diskFile struct {
	V        int                   `json:"v"`
	Epoch    string                `json:"epoch"`
	Host     string                `json:"host"`
	Sessions []*Session            `json:"sessions"`
	Files    map[string]*FileState `json:"files"`
	Archive  []archRow             `json:"archive"`
	Peers    map[string]*PeerState `json:"peers,omitempty"`
}

// Open reads the store at path (it need not exist) for the local host. A
// file that cannot be parsed is an error: it is never overwritten.
func Open(path, host string) (*Store, error) {
	home, _ := os.UserHomeDir()
	st := &Store{path: path, host: host, home: home, now: time.Now,
		sessions: map[string]*Session{}, files: map[string]*FileState{}, archive: map[archKey]Tokens{}, peers: map[string]*PeerState{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		st.epoch = newEpoch()
		st.dirty = true
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	var d diskFile
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	st.epoch = d.Epoch
	if st.epoch == "" {
		st.epoch = newEpoch()
	}
	for _, s := range d.Sessions {
		s.index()
		st.sessions[s.Key()] = s
		if s.Upd > st.seq {
			st.seq = s.Upd
		}
	}
	if d.Files != nil && d.Host == host {
		st.files = d.Files
	}
	for _, a := range d.Archive {
		st.archive[archKey{a.H, a.Host, a.Kind, a.Model, a.Project, a.Role, a.Purpose, a.Handoff}] = a.T
	}
	if d.Peers != nil {
		st.peers = d.Peers
	}
	return st, nil
}

func newEpoch() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Host is the local host's name.
func (st *Store) Host() string { return st.host }

// SetClock replaces the clock.
func (st *Store) SetClock(now func() time.Time) { st.now = now }

// nextUpd returns a cursor value larger than any before. Callers hold mu.
func (st *Store) nextUpd() int64 {
	n := st.now().UnixNano()
	if n <= st.seq {
		n = st.seq + 1
	}
	st.seq = n
	return n
}

// Save writes the store if it changed.
func (st *Store) Save() error {
	st.mu.Lock()
	if !st.dirty || st.path == "" {
		st.mu.Unlock()
		return nil
	}
	d := diskFile{V: 1, Epoch: st.epoch, Host: st.host, Files: st.files, Peers: st.peers}
	for _, s := range st.sessions {
		d.Sessions = append(d.Sessions, s)
	}
	sort.Slice(d.Sessions, func(i, j int) bool { return d.Sessions[i].Key() < d.Sessions[j].Key() })
	for k, t := range st.archive {
		d.Archive = append(d.Archive, archRow{k.H, k.Host, k.Kind, k.Model, k.Project, k.Role, k.Purpose, k.Handoff, t})
	}
	sort.Slice(d.Archive, func(i, j int) bool {
		a, b := d.Archive[i], d.Archive[j]
		if a.H != b.H {
			return a.H < b.H
		}
		return a.Host+a.Kind+a.Model+a.Project+a.Handoff < b.Host+b.Kind+b.Model+b.Project+b.Handoff
	})
	b, err := json.Marshal(d)
	st.dirty = false
	st.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".usage-*.tmp")
	if err != nil {
		st.markDirty()
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), st.path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		st.markDirty()
		return werr
	}
	return nil
}

func (st *Store) markDirty() {
	st.mu.Lock()
	st.dirty = true
	st.mu.Unlock()
}

// Snapshot is what a host gives the hub: its own sessions changed after a
// cursor.
type Snapshot struct {
	Epoch    string     `json:"epoch"`
	Host     string     `json:"host"`
	Home     string     `json:"home,omitempty"`
	Cursor   int64      `json:"cursor"` // pass back as since
	More     bool       `json:"more,omitempty"`
	Sessions []*Session `json:"sessions"`
}

// SnapshotSince returns the local host's sessions changed after since,
// oldest change first, at most limit of them (and about maxRows rows). More
// says there are further ones: ask again from Cursor.
func (st *Store) SnapshotSince(since int64, limit int) Snapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := Snapshot{Epoch: st.epoch, Host: st.host, Home: st.home, Cursor: since}
	var all []*Session
	for _, s := range st.sessions {
		if s.Host == st.host && s.Upd > since {
			all = append(all, s)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Upd < all[j].Upd })
	rows := 0
	for i, s := range all {
		if (limit > 0 && len(out.Sessions) >= limit) || (rows > maxSnapshotRows && len(out.Sessions) > 0) {
			out.More = len(all) > i
			break
		}
		cp := *s
		cp.Rows = append([]Row(nil), s.Rows...)
		cp.idx = nil
		cp.L = Labels{} // labels are the hub's
		out.Sessions = append(out.Sessions, &cp)
		rows += len(s.Rows)
		out.Cursor = s.Upd
	}
	return out
}

// maxSnapshotRows bounds one answer to a gathering hub (a peer's answer is
// read up to a few MiB).
const maxSnapshotRows = 20000

// PeerCursor is where to resume gathering from host, and the epoch that
// cursor belongs to.
func (st *Store) PeerCursor(host string) (since int64, epoch string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if p := st.peers[host]; p != nil {
		return p.Cursor, p.Epoch
	}
	return 0, ""
}

// Merge replaces host's sessions with those in snap and moves its cursor.
// A peer whose store was replaced (a new epoch) answers a stale cursor with
// everything, so the cursor simply follows. Sessions already past the
// retention window are refused: they may have been rolled up, and would
// be counted twice.
func (st *Store) Merge(host string, snap Snapshot) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := st.now().Add(-SessionRetention).Unix()
	p := st.peers[host]
	if p == nil {
		p = &PeerState{}
		st.peers[host] = p
	}
	if snap.Epoch != p.Epoch || snap.Cursor > p.Cursor {
		p.Cursor = snap.Cursor
	}
	p.Epoch, p.Home, p.OK, p.Err = snap.Epoch, snap.Home, st.now().Unix(), ""
	n := 0
	for _, in := range snap.Sessions {
		if in.Last < cutoff {
			continue
		}
		s := *in
		s.Host = host
		s.Rows = append([]Row(nil), in.Rows...)
		s.idx = nil
		if old := st.sessions[s.Key()]; old != nil && old.L.Src >= s.L.Src {
			s.L = old.L // keep what the hub labelled
		}
		st.sessions[s.Key()] = &s
		n++
	}
	st.dirty = true
	return n
}

// PeerFailed records that gathering from host failed.
func (st *Store) PeerFailed(host, msg string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p := st.peers[host]
	if p == nil {
		p = &PeerState{}
		st.peers[host] = p
	}
	if p.Err != msg {
		p.Err = msg
		st.dirty = true
	}
}

// Peers returns what is known about each peer.
func (st *Store) Peers() map[string]PeerState {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]PeerState, len(st.peers))
	for k, v := range st.peers {
		out[k] = *v
	}
	return out
}

// HomeOf is host's home directory as far as known ("" if not).
func (st *Store) HomeOf(host string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.homeLocked(host)
}

func (st *Store) homeLocked(host string) string {
	if host == st.host {
		return st.home
	}
	if p := st.peers[host]; p != nil {
		return p.Home
	}
	return ""
}

// SetHome overrides the local home directory (tests).
func (st *Store) SetHome(h string) { st.mu.Lock(); st.home = h; st.mu.Unlock() }

// Relabel gives each session of the last 30 days the labels fn computes,
// except that a ledger-derived label is never replaced by a guess.
func (st *Store) Relabel(fn func(host, home string, s *Session) Labels) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := st.now().Add(-30 * 24 * time.Hour).Unix()
	for _, s := range st.sessions {
		if s.Last < cutoff {
			continue
		}
		n := fn(s.Host, st.homeLocked(s.Host), s)
		if n.Src < s.L.Src || n == s.L {
			continue
		}
		s.L = n
		st.dirty = true
	}
}

// Prune rolls the sessions that ended before the retention window up into
// the hourly rollups and drops their per-session rows.
func (st *Store) Prune() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := st.now().Add(-SessionRetention).Unix()
	n := 0
	for key, s := range st.sessions {
		if s.Last >= cutoff {
			continue
		}
		for _, r := range s.Rows {
			k := archKey{r.H, s.Host, s.Kind, r.M, s.L.Project, s.L.Role, s.L.Purpose, s.L.Handoff}
			t := st.archive[k]
			t.Add(r.T)
			st.archive[k] = t
		}
		delete(st.sessions, key)
		n++
	}
	if n > 0 {
		st.dirty = true
	}
	return n
}

// Epoch identifies this store; a hub resets its cursor when it changes.
func (st *Store) Epoch() string { return st.epoch }
