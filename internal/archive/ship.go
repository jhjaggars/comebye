package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Source is one agent's session directory.
type Source struct {
	Kind string // "claude" or "pi"
	Root string // ~/.claude/projects, ~/.pi/agent/sessions
}

// Archiver copies new bytes of session files into a Backend.
type Archiver struct {
	Host string
	B    Backend
	S    Sealer
	// MaxSegment caps the raw bytes of one segment (default 32 MiB).
	MaxSegment int64
	// IdleAfter: a .jsonl file's last, unterminated line is held back until
	// the file has been quiet this long (default 2m), so a line being
	// written is not split across segments. Splitting would still restore
	// byte-exactly; this only keeps segments on line boundaries.
	IdleAfter time.Duration
	Now       func() time.Time
	// LockDir holds the per-host lock taken by Run, Flush and DeleteSession
	// (default ~/.local/state/comebye/locks).
	LockDir string
	// beforeWrite, if set, runs under the lock before the first write and
	// again until it succeeds: checks an S3 backend makes over the network.
	beforeWrite func() error
}

// Result counts what a run did.
type Result struct {
	Files       int      `json:"files"`        // files looked at
	Updated     int      `json:"updated"`      // files that got new segments
	Segments    int      `json:"segments"`     // segments written
	RawBytes    int64    `json:"raw_bytes"`    // bytes copied
	StoredBytes int64    `json:"stored_bytes"` // bytes of segments written
	Tombstoned  int      `json:"tombstoned"`   // files skipped: session deleted on request
	Shrunk      int      `json:"shrunk"`       // files smaller than what is archived (never rewritten)
	Conflicts   int      `json:"conflicts"`    // segment already existed with another length
	Errors      []string `json:"errors,omitempty"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SessionOf says which session a file belongs to, from its path relative
// to the agent's root: the project/cwd slug and the session id. ok is
// false for files that are not part of a session (Claude's per-project
// memory/ and index files).
//
// Claude: <slug>/<uuid>.jsonl and everything under <slug>/<uuid>/
// (subagents/, tool-results/, workflows/). Pi: <slug>/<file>.jsonl.
func SessionOf(kind, rel string) (slug, sid string, ok bool) {
	p := strings.Split(filepath.ToSlash(rel), "/")
	if len(p) < 2 {
		return "", "", false
	}
	switch kind {
	case "claude":
		if len(p) == 2 && strings.HasSuffix(p[1], ".jsonl") && uuidRE.MatchString(strings.TrimSuffix(p[1], ".jsonl")) {
			return p[0], strings.TrimSuffix(p[1], ".jsonl"), true
		}
		if len(p) >= 3 && uuidRE.MatchString(p[1]) {
			return p[0], p[1], true
		}
	case "pi":
		if len(p) == 2 && strings.HasSuffix(p[1], ".jsonl") {
			return p[0], strings.TrimSuffix(p[1], ".jsonl"), true
		}
	}
	return "", "", false
}

func tombKey(host, kind, slug, sid string) string {
	return "tomb/" + host + "/" + kind + "/" + slug + "/" + sid
}

func (a *Archiver) max() int64 {
	if a.MaxSegment > 0 {
		return a.MaxSegment
	}
	return 32 << 20
}

func (a *Archiver) idle() time.Duration {
	if a.IdleAfter > 0 {
		return a.IdleAfter
	}
	return 2 * time.Minute
}

func (a *Archiver) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Run archives every session file under the sources. It is safe to run
// again at any time, concurrently with the agents, and after a crash:
// what is stored is read back from the backend, each segment is written
// atomically and create-only, and the next run resumes at the end of the
// contiguous stored bytes.
func (a *Archiver) Run(sources []Source) (Result, error) {
	return a.run(sources, false, "")
}

// RunFinal is Run for sessions that have ended: it also takes each file's last,
// unterminated line, without waiting for the file to go quiet.
func (a *Archiver) RunFinal(sources []Source) (Result, error) {
	return a.run(sources, true, "")
}

// Flush archives the files of one session now, including a last line
// with no newline (the agent is going away). Used when a worker is removed.
func (a *Archiver) Flush(src Source, sessionFile string) (Result, error) {
	return a.run([]Source{src}, true, sessionFile)
}

// lock serialises everything on this machine that writes one host's
// segments. Two writers starting from the same offset can cut different
// lengths (a run holds back an unterminated last line, a flush does not),
// which leaves overlapping segments, and shipFile then refuses that stream
// for good. The timer's own flock does not cover the server's flush.
//
// It is a separate file from the timer's lock on purpose: that flock is
// held by the process that runs `comebye archive run`, so taking the same
// file here would wait forever.
func (a *Archiver) lock() (unlock func(), err error) {
	dir := a.LockDir
	if dir == "" {
		dir = expand("~/.local/state/comebye/locks")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("archive: lock: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "archive-"+url.PathEscape(a.Host)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("archive: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("archive: lock: %w", err)
	}
	return func() { f.Close() }, nil
}

// tombstones lists the sessions of this host deleted on request. A failure
// is an error, never an empty set: treating it as "none deleted" would copy
// a deleted session back in.
func (a *Archiver) tombstones() (map[string]bool, error) {
	keys, err := a.B.List("tomb/" + a.Host + "/")
	if err != nil {
		return nil, fmt.Errorf("archive: reading tombstones: %w", err)
	}
	tombs := make(map[string]bool, len(keys))
	for _, k := range keys {
		tombs[k] = true
	}
	return tombs, nil
}

func (a *Archiver) run(sources []Source, final bool, only string) (Result, error) {
	var res Result
	unlock, err := a.lock()
	if err != nil {
		return res, err
	}
	defer unlock()
	if a.beforeWrite != nil {
		if err := a.beforeWrite(); err != nil {
			return res, err
		}
		a.beforeWrite = nil
	}
	state, err := scan(a.B, a.Host)
	if err != nil {
		return res, err
	}
	tombs, err := a.tombstones()
	if err != nil {
		return res, err
	}
	for _, src := range sources {
		root := filepath.Clean(src.Root)
		walkRoot := root
		if only != "" {
			// A session file and its sibling directory.
			walkRoot = filepath.Dir(only)
		}
		_ = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			slug, sid, ok := SessionOf(src.Kind, rel)
			if !ok {
				return nil
			}
			if only != "" {
				base := strings.TrimSuffix(filepath.Base(only), ".jsonl")
				if sid != base || slug != filepath.Base(filepath.Dir(only)) {
					return nil
				}
			}
			res.Files++
			if tombs[tombKey(a.Host, src.Kind, slug, sid)] {
				res.Tombstoned++
				return nil
			}
			stream := filepath.ToSlash(rel)
			if err := a.shipFile(&res, state, src.Kind, stream, p, final); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s/%s: %v", src.Kind, stream, err))
			}
			return nil
		})
	}
	sort.Strings(res.Errors)
	return res, nil
}

func (a *Archiver) shipFile(res *Result, state map[string]*streamState, kind, stream, path string, final bool) error {
	id := kind + "/" + stream
	st := state[id]
	var have int64
	if st != nil {
		if st.Overlap {
			return errors.New("stored segments overlap; not adding to it")
		}
		have = st.Contiguous
		// A gap means a segment is missing; adding after it would hide
		// that, so stop and let someone look.
		var total int64
		for _, s := range st.Segs {
			total += s.Len
		}
		if total != have {
			return errors.New("stored segments have a gap; not adding to it")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size < have {
		res.Shrunk++
		return fmt.Errorf("file is %d bytes, %d are archived (it was rewritten or truncated); left alone", size, have)
	}
	if size == have {
		return nil
	}
	quiet := final || a.now().Sub(info.ModTime()) >= a.idle() || !strings.HasSuffix(stream, ".jsonl")
	wrote := false
	for have < size {
		n := size - have
		if n > a.max() {
			n = a.max()
		}
		buf := make([]byte, n)
		got, rerr := f.ReadAt(buf, have)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return rerr
		}
		buf = buf[:got]
		atEnd := have+int64(len(buf)) >= size
		if !(quiet && atEnd) {
			// Keep segments on line boundaries: cut at the last newline.
			// A full-size chunk with no newline is shipped whole; an
			// unterminated tail of a file still being written waits.
			if i := lastNewline(buf); i >= 0 {
				buf = buf[:i+1]
			} else if atEnd {
				buf = nil
			}
		}
		if len(buf) == 0 {
			break
		}
		blob, err := encodeSegment(a.S, a.Host, kind, stream, have, buf)
		if err != nil {
			return err
		}
		key := segKeyFor(a.Host, kind, stream, have, int64(len(buf)))
		if err := a.B.Put(key, blob); err != nil {
			if errors.Is(err, ErrExists) {
				res.Conflicts++
				return fmt.Errorf("segment %s already exists", filepath.Base(key))
			}
			return err
		}
		res.Segments++
		res.RawBytes += int64(len(buf))
		res.StoredBytes += int64(len(blob))
		wrote = true
		have += int64(len(buf))
	}
	if wrote {
		res.Updated++
	}
	return nil
}

func lastNewline(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}

// VerifyResult reports Verify.
type VerifyResult struct {
	Files      int      `json:"files"`
	Matched    int      `json:"matched"`    // archived prefix equals the file's prefix (sha256)
	Behind     int      `json:"behind"`     // matched, and the file has newer bytes not yet archived
	Missing    int      `json:"missing"`    // file has bytes and no archive at all
	Tombstoned int      `json:"tombstoned"` // session deleted on request: not archived on purpose
	Empty      int      `json:"empty"`      // zero-length file with no archive: nothing to keep
	Mismatched int      `json:"mismatched"` // archive differs from the file
	Bytes      int64    `json:"bytes"`      // bytes compared
	Failures   []string `json:"failures,omitempty"`
}

// Verify restores each file's stream from the archive and compares its
// sha256 with the same-length prefix of the file on disk.
func (a *Archiver) Verify(o Opener, sources []Source) (VerifyResult, error) {
	var vr VerifyResult
	state, err := scan(a.B, a.Host)
	if err != nil {
		return vr, err
	}
	tombs, err := a.tombstones()
	if err != nil {
		return vr, err
	}
	for _, src := range sources {
		root := filepath.Clean(src.Root)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			slug, sid, ok := SessionOf(src.Kind, rel)
			if !ok {
				return nil
			}
			stream := filepath.ToSlash(rel)
			st := state[src.Kind+"/"+stream]
			vr.Files++
			if tombs[tombKey(a.Host, src.Kind, slug, sid)] {
				vr.Tombstoned++
				return nil
			}
			if st == nil {
				if fi, err := d.Info(); err == nil && fi.Size() == 0 {
					vr.Empty++
					return nil
				}
				vr.Missing++
				return nil
			}
			fail := func(msg string) {
				vr.Mismatched++
				vr.Failures = append(vr.Failures, src.Kind+"/"+stream+": "+msg)
			}
			h := sha256.New()
			n, err := StreamTo(h, a.B, o, a.Host, src.Kind, stream)
			if err != nil {
				fail(err.Error())
				return nil
			}
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer f.Close()
			h2 := sha256.New()
			m, err := io.Copy(h2, io.LimitReader(f, n))
			if err != nil || m != n {
				fail(fmt.Sprintf("file is shorter (%d) than the archive (%d)", m, n))
				return nil
			}
			if hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(h2.Sum(nil)) {
				fail("sha256 differs")
				return nil
			}
			vr.Bytes += n
			if fi, err := f.Stat(); err == nil && fi.Size() > n {
				vr.Behind++
			}
			vr.Matched++
			return nil
		})
	}
	return vr, nil
}

// StoreCheck reports VerifyStore.
type StoreCheck struct {
	Hosts    []string `json:"hosts"`
	Streams  int      `json:"streams"`
	Segments int      `json:"segments"`
	Bytes    int64    `json:"bytes"`    // the raw bytes the segments hold
	Gaps     int      `json:"gaps"`     // missing ranges
	Overlaps int      `json:"overlaps"` // segments that cover bytes another one has
	Bad      int      `json:"bad"`      // segments that could not be read or decoded
	Unnamed  int      `json:"unnamed"`  // objects under v1/ that are not named like a segment
	Failures []string `json:"failures,omitempty"`
}

// OK says the store has no fault of any kind.
func (c StoreCheck) OK() bool { return c.Gaps+c.Overlaps+c.Bad+c.Unnamed == 0 }

// verifyStoreWorkers is how many segments are read at once.
const verifyStoreWorkers = 4

// VerifyStore checks the store by itself, with no session files to compare with:
// it decodes every segment of this host, or of all hosts (magic, header against
// key, zstd, length), and reports gaps and overlaps in each stream. It is what
// proves a restored backup, and the only check for a session whose source is gone.
func (a *Archiver) VerifyStore(o Opener, allHosts bool) (StoreCheck, error) {
	c := StoreCheck{Hosts: []string{}}
	prefix := "v1/" + a.Host + "/"
	if allHosts {
		prefix = "v1/"
	}
	keys, err := a.B.List(prefix)
	if err != nil {
		return c, err
	}
	type streamID struct{ host, kind, stream string }
	streams := map[streamID][]segName{}
	type job struct {
		key string
		n   segName
	}
	var jobs []job
	for _, k := range keys {
		n, ok := parseSegKey(k)
		if !ok {
			c.Unnamed++
			c.Failures = append(c.Failures, k+": not named like a segment")
			continue
		}
		id := streamID{n.Host, n.Kind, n.Stream}
		streams[id] = append(streams[id], n)
		jobs = append(jobs, job{k, n})
	}
	hosts := map[string]bool{}
	ids := make([]streamID, 0, len(streams))
	for id := range streams {
		ids = append(ids, id)
		if !hosts[id.host] {
			hosts[id.host] = true
			c.Hosts = append(c.Hosts, id.host)
		}
	}
	sort.Strings(c.Hosts)
	sort.Slice(ids, func(i, j int) bool {
		x, y := ids[i], ids[j]
		return x.host+"/"+x.kind+"/"+x.stream < y.host+"/"+y.kind+"/"+y.stream
	})
	c.Streams, c.Segments = len(ids), len(jobs)
	for _, id := range ids {
		segs := streams[id]
		sort.Slice(segs, func(i, j int) bool {
			if segs[i].Off != segs[j].Off {
				return segs[i].Off < segs[j].Off
			}
			return segs[i].Len < segs[j].Len
		})
		name := id.host + "/" + id.kind + "/" + id.stream
		var pos int64
		for _, s := range segs {
			switch {
			case s.Off > pos:
				c.Gaps++
				c.Failures = append(c.Failures, fmt.Sprintf("%s: gap, bytes %d to %d are missing", name, pos, s.Off))
			case s.Off < pos:
				c.Overlaps++
				c.Failures = append(c.Failures, fmt.Sprintf("%s: overlap, the segment at %d covers bytes up to %d that are already stored", name, s.Off, pos))
			}
			if e := s.Off + s.Len; e > pos {
				pos = e
			}
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan job)
	for i := 0; i < verifyStoreWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				var raw []byte
				body, err := a.B.Get(j.key)
				if err == nil {
					raw, err = decodeSegment(o, j.n, body)
				}
				mu.Lock()
				if err != nil {
					c.Bad++
					c.Failures = append(c.Failures, j.key+": "+err.Error())
				} else {
					c.Bytes += int64(len(raw))
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		next <- j
	}
	close(next)
	wg.Wait()
	sort.Strings(c.Failures)
	return c, nil
}

// DeleteSession removes a session from the archive on request: it writes a
// tombstone first (so a later run does not copy the still-present source
// files again) and then deletes the segments. It returns the number of
// segments deleted.
func (a *Archiver) DeleteSession(kind, slug, sid string) (int, error) {
	unlock, err := a.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	meta, _ := json.Marshal(map[string]string{"deleted_at": a.now().UTC().Format(time.RFC3339)})
	if err := a.B.Put(tombKey(a.Host, kind, slug, sid), meta); err != nil && !errors.Is(err, ErrExists) {
		return 0, err
	}
	keys, err := a.B.List("v1/" + a.Host + "/" + kind + "/" + slug + "/")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range keys {
		sn, ok := parseSegKey(k)
		if !ok {
			continue
		}
		_, s, ok := SessionOf(kind, slugJoin(sn.Stream))
		if !ok || s != sid {
			continue
		}
		if err := a.B.Delete(k); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// slugJoin is the stream (already "<slug>/<rest>").
func slugJoin(stream string) string { return stream }

// RewrapStats reports Rewrap.
type RewrapStats struct {
	Segments, Notes, Tombstones int
	Skipped                     int // already in the destination
}

// Dir returns the backend directory when the backend is a filesystem one.
func (a *Archiver) Dir() string {
	if fb, ok := a.B.(*FSBackend); ok {
		return fb.Dir
	}
	return ""
}

// Rewrap copies this host's archive into dst, decrypting each segment with
// o and encrypting it to the recipients of newSealer. The source is not
// changed. It is how the archive moves to a different key set (a rotated
// key, a shared homelab identity, a different set of recipients): copy, run
// `archive verify` against the new store with the new identity, then retire
// the old store. It is resumable: objects already in dst are skipped.
func (a *Archiver) Rewrap(o Opener, dst Backend, newSealer Sealer) (RewrapStats, error) {
	var st RewrapStats
	keys, err := a.B.List("v1/" + a.Host + "/")
	if err != nil {
		return st, err
	}
	for _, k := range keys {
		n, ok := parseSegKey(k)
		if !ok {
			continue
		}
		body, err := a.B.Get(k)
		if err != nil {
			return st, err
		}
		raw, err := decodeSegment(o, n, body)
		if err != nil {
			return st, fmt.Errorf("%s: %w", k, err)
		}
		blob, err := encodeSegment(newSealer, n.Host, n.Kind, n.Stream, n.Off, raw)
		if err != nil {
			return st, err
		}
		if err := dst.Put(k, blob); err != nil {
			if errors.Is(err, ErrExists) {
				st.Skipped++
				continue
			}
			return st, err
		}
		st.Segments++
	}
	notes, err := a.B.List("notes/" + a.Host + "/")
	if err != nil {
		return st, err
	}
	for _, k := range notes {
		blob, err := a.B.Get(k)
		if err != nil {
			return st, err
		}
		plain, err := o.Open(blob)
		if err != nil {
			return st, fmt.Errorf("%s: %w", k, err)
		}
		sealed, err := newSealer.Seal(plain)
		if err != nil {
			return st, err
		}
		if err := dst.Put(k, sealed); err != nil && !errors.Is(err, ErrExists) {
			return st, err
		} else if err == nil {
			st.Notes++
		}
	}
	tombs, err := a.B.List("tomb/" + a.Host + "/")
	if err != nil {
		return st, err
	}
	for _, k := range tombs {
		body, err := a.B.Get(k)
		if err != nil {
			return st, err
		}
		if err := dst.Put(k, body); err != nil && !errors.Is(err, ErrExists) {
			return st, err
		} else if err == nil {
			st.Tombstones++
		}
	}
	return st, nil
}
