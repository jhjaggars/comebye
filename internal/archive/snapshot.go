package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// One machine builds the index of every host and publishes a copy of it to the
// store. Other machines download the newest copy and move it into place, so
// `comebye recall` and the app keep reading a local file on every host.
//
// A snapshot is <prefix><time>.db.zst: the database, zstd-compressed, and in the
// age mode then sealed like everything else in the store. A copy is in rollback
// journal mode, never WAL, so that a reader creates no files beside it.
//
// A snapshot is large and the store's backups keep what it overwrites, so one is
// only uploaded when the index's content differs from the last one's. Every run
// also writes a small check object, <prefix><time>.check.json, saying when it
// looked and what it found. A pull copies that time into the local file, so a
// copy whose content has not changed for days still shows that its indexer is
// alive, and one whose indexer has stopped goes stale.

// SnapshotPrefix is where snapshots live in the store.
const SnapshotPrefix = "index/"

const (
	snapshotKeep    = 2
	snapshotTimeFmt = "20060102T150405Z"
	// snapshotMaxBytes bounds what a download can write to the disk.
	snapshotMaxBytes = 32 << 30
	// snapshotTempAge is when a temp file left by a crashed run gets swept.
	snapshotTempAge = time.Hour
)

// foldWait is how long foldWAL keeps trying: longer than a Reader's query may run.
var foldWait = 4 * time.Second

var (
	snapshotRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.db\.zst$`)
	checkRE    = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.check\.json$`)
)

// SnapshotCheck is what an indexing run found. Objects are never overwritten, so
// each run writes a new one and the older ones get removed.
type SnapshotCheck struct {
	CheckedAt   string `json:"checked_at"`
	Snapshot    string `json:"snapshot"`    // the key of the snapshot that holds this content
	Fingerprint string `json:"fingerprint"` // see Index.fingerprint
}

// ErrNoSnapshot means the store holds no snapshot under the prefix.
var ErrNoSnapshot = errors.New("archive: no index snapshot in the store")

// PublishResult reports Publish.
type PublishResult struct {
	Key     string   `json:"key"`
	Skipped bool     `json:"skipped,omitempty"` // the content is what Key already holds: nothing uploaded
	DBBytes int64    `json:"db_bytes,omitempty"`
	Stored  int64    `json:"stored_bytes,omitempty"`
	Pruned  []string `json:"pruned,omitempty"`
	// PruneError: the snapshot is stored, but older ones could not be removed.
	PruneError string `json:"prune_error,omitempty"`
}

// PullResult reports Pull.
type PullResult struct {
	Key      string `json:"key"`
	UpToDate bool   `json:"up_to_date"` // the local file already is this snapshot or a newer one
	// CheckedAt is when the indexer last looked, as recorded in the local file.
	CheckedAt string `json:"checked_at,omitempty"`
	BuiltAt   string `json:"built_at,omitempty"`
	Sessions  int    `json:"sessions,omitempty"`
	Stored    int64  `json:"stored_bytes,omitempty"`
}

func checkSnapshotPrefix(prefix string) error {
	if !strings.HasSuffix(prefix, "/") || validKey(prefix+"x") != nil {
		return fmt.Errorf("archive: bad snapshot prefix %q (want something like %s)", prefix, SnapshotPrefix)
	}
	for _, p := range []string{"v1/", "tomb/", "notes/", "hosts/"} {
		if strings.HasPrefix(prefix, p) {
			return fmt.Errorf("archive: the snapshot prefix %q is inside %s, which holds other objects", prefix, p)
		}
	}
	return nil
}

// snapshots lists the snapshot keys under prefix, oldest first. Keys sort by time.
func snapshots(b Backend, prefix string) ([]string, error) {
	if err := checkSnapshotPrefix(prefix); err != nil {
		return nil, err
	}
	keys, err := b.List(prefix)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range keys {
		if rest := strings.TrimPrefix(k, prefix); snapshotRE.MatchString(rest) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// LastCheck returns the newest check object, or nil when there is none.
func LastCheck(b Backend, prefix string) (*SnapshotCheck, error) {
	if err := checkSnapshotPrefix(prefix); err != nil {
		return nil, err
	}
	keys, err := b.List(prefix)
	if err != nil {
		return nil, err
	}
	newest := ""
	for _, k := range keys {
		if checkRE.MatchString(strings.TrimPrefix(k, prefix)) && k > newest {
			newest = k
		}
	}
	if newest == "" {
		return nil, nil
	}
	raw, err := b.Get(newest)
	if err != nil {
		return nil, err
	}
	var c SnapshotCheck
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("archive: reading %s: %w", newest, err)
	}
	return &c, nil
}

// writeCheck stores a check object and removes all but the newest two.
func writeCheck(b Backend, prefix string, at time.Time, c SnapshotCheck) error {
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	key := prefix + at.UTC().Format(snapshotTimeFmt) + ".check.json"
	if err := b.Put(key, body); err != nil && !errors.Is(err, ErrExists) {
		return err
	}
	keys, err := b.List(prefix)
	if err != nil {
		return err
	}
	var checks []string
	for _, k := range keys {
		if checkRE.MatchString(strings.TrimPrefix(k, prefix)) {
			checks = append(checks, k)
		}
	}
	sort.Strings(checks)
	for len(checks) > snapshotKeep {
		if err := b.Delete(checks[0]); err != nil {
			return err
		}
		checks = checks[1:]
	}
	return nil
}

// fingerprint hashes what a reader of the index can tell apart. It leaves out the
// build time, which changes on every run, and the database's bytes, which a
// rebuild of the thread tables changes without changing anything in them. A
// session's messages follow from its archived bytes and its handoff link, so
// those stand in for them.
func (ix *Index) fingerprint() (string, error) {
	h := sha256.New()
	for _, sql := range []string{
		"SELECT host,kind,id,n_bytes,link_sig,summary,summary_at FROM sessions ORDER BY host,kind,id;",
		"SELECT id,host,title,brief,started,ended,n_sessions,hosts,repos,branches,verdict,outcome,keywords,summary,lead_kind,lead_id FROM threads ORDER BY id;",
		"SELECT thread_id,host,kind,session_id FROM thread_sessions ORDER BY thread_id,host,kind,session_id;",
		"SELECT thread_id,kind,ref,text,source,at FROM outcomes ORDER BY thread_id,kind,ref,text,source,at;",
	} {
		out, err := ix.run(sql, false)
		if err != nil {
			return "", fmt.Errorf("archive: fingerprinting the index: %w", err)
		}
		h.Write(out)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NewestSnapshot returns the newest snapshot's key, or "" when there is none.
func NewestSnapshot(b Backend, prefix string) (string, error) {
	keys, err := snapshots(b, prefix)
	if err != nil || len(keys) == 0 {
		return "", err
	}
	return keys[len(keys)-1], nil
}

// tempBeside creates an empty mode-0600 file next to the index, sweeping what a
// crashed run left behind.
func (ix *Index) tempBeside(what string) (string, error) {
	dir, base := filepath.Dir(ix.Path), filepath.Base(ix.Path)
	old, _ := filepath.Glob(filepath.Join(dir, base+"."+what+"-*"))
	for _, p := range old {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > snapshotTempAge {
			removeDB(p)
		}
	}
	f, err := os.CreateTemp(dir, base+"."+what+"-*")
	if err != nil {
		return "", err
	}
	return f.Name(), f.Close()
}

func removeDB(p string) {
	for _, s := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(p + s)
	}
}

// checkSnapshotDB refuses a file that is not a complete index this binary can
// read. It reports when the index was built and how many sessions it holds.
func (ix *Index) checkSnapshotDB() (builtAt string, sessions int, err error) {
	rows, err := ix.Query("PRAGMA user_version;")
	if err != nil {
		return "", 0, fmt.Errorf("archive: not a readable database: %w", err)
	}
	if len(rows) == 0 || num(rows[0], "user_version") != schemaVersion {
		v := 0
		if len(rows) > 0 {
			v = num(rows[0], "user_version")
		}
		return "", 0, fmt.Errorf("archive: the index is in format %d and this binary reads format %d: upgrade so both ends match", v, schemaVersion)
	}
	rows, err = ix.Query("PRAGMA quick_check(1);")
	if err != nil {
		return "", 0, fmt.Errorf("archive: checking the database: %w", err)
	}
	if len(rows) != 1 || sget(rows[0], "quick_check") != "ok" {
		return "", 0, errors.New("archive: the database fails its integrity check")
	}
	rows, err = ix.Query("SELECT (SELECT value FROM meta WHERE key='built_at') AS built_at, (SELECT count(*) FROM sessions) AS sessions, (SELECT count(*) FROM threads) AS threads;")
	if err != nil {
		return "", 0, fmt.Errorf("archive: reading the database: %w", err)
	}
	if len(rows) != 1 || sget(rows[0], "built_at") == "" {
		return "", 0, errors.New("archive: the database was never finished: it has no build time")
	}
	if num(rows[0], "sessions") == 0 {
		return "", 0, errors.New("archive: the index holds no sessions")
	}
	return sget(rows[0], "built_at"), num(rows[0], "sessions"), nil
}

// Publish records that the index was checked and, if its content differs from
// the last snapshot's, first writes a consistent copy of it as a new snapshot and
// removes all but the newest two. Only one machine should do this.
func (ix *Index) Publish(a *Archiver, prefix string) (PublishResult, error) {
	var res PublishResult
	if err := checkSnapshotPrefix(prefix); err != nil {
		return res, err
	}
	fp, err := ix.fingerprint()
	if err != nil {
		return res, err
	}
	// An unreadable last check fails the run: it is not the same as "changed",
	// and uploading on every blip is what this avoids.
	last, err := LastCheck(a.B, prefix)
	if err != nil {
		return res, err
	}
	keys, err := snapshots(a.B, prefix)
	if err != nil {
		return res, err
	}
	if last != nil && last.Fingerprint == fp && slices.Contains(keys, last.Snapshot) {
		res.Key, res.Skipped = last.Snapshot, true
	} else if err := ix.upload(a, prefix, &res); err != nil {
		return res, err
	}
	now := a.now()
	if err := writeCheck(a.B, prefix, now, SnapshotCheck{CheckedAt: now.UTC().Format(time.RFC3339), Snapshot: res.Key, Fingerprint: fp}); err != nil {
		return res, fmt.Errorf("archive: recording the check: %w", err)
	}
	return res, nil
}

// upload writes a new snapshot and removes all but the newest two.
func (ix *Index) upload(a *Archiver, prefix string, res *PublishResult) error {
	tmp, err := ix.tempBeside("publish")
	if err != nil {
		return err
	}
	defer removeDB(tmp)
	key := prefix + a.now().UTC().Format(snapshotTimeFmt) + ".db.zst"
	if _, err := ix.run("VACUUM INTO "+q(tmp)+";", false); err != nil {
		return fmt.Errorf("archive: copying the index: %w", err)
	}
	c := &Index{Path: tmp, Sqlite: ix.Sqlite}
	if _, err := c.run("PRAGMA journal_mode=DELETE; INSERT OR REPLACE INTO meta VALUES('snapshot_key',"+q(path.Base(key))+");", false); err != nil {
		return fmt.Errorf("archive: preparing the copy: %w", err)
	}
	if _, _, err := c.checkSnapshotDB(); err != nil {
		return fmt.Errorf("not publishing: %w", err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	if res.DBBytes, err = io.Copy(zw, f); err != nil {
		zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	sealed, err := a.S.Seal(buf.Bytes())
	if err != nil {
		return err
	}
	if err := a.B.Put(key, sealed); err != nil {
		return err
	}
	res.Key, res.Stored = key, int64(len(sealed))
	keys, err := snapshots(a.B, prefix)
	if err != nil {
		res.PruneError = err.Error()
		return nil
	}
	for len(keys) > snapshotKeep {
		if err := a.B.Delete(keys[0]); err != nil {
			res.PruneError = err.Error()
			return nil
		}
		res.Pruned = append(res.Pruned, keys[0])
		keys = keys[1:]
	}
	return nil
}

// Pull downloads the newest snapshot if it is new, checks it, and moves it over
// the index, and records in the local file when the indexer last looked.
// A failure at any step leaves the current file as it was. A reader that is in
// the middle of a query finishes on the old file, and the next query opens the
// new one.
func (ix *Index) Pull(a *Archiver, o Opener, prefix string) (PullResult, error) {
	var res PullResult
	key, err := NewestSnapshot(a.B, prefix)
	if err != nil {
		return res, err
	}
	if key == "" {
		return res, fmt.Errorf("%w under %s: has the indexing machine run `archive index --all-hosts --publish`?", ErrNoSnapshot, prefix)
	}
	res.Key = key
	check, err := LastCheck(a.B, prefix)
	if err != nil {
		return res, err
	}
	if check != nil {
		res.CheckedAt = check.CheckedAt
	}
	dir := filepath.Dir(ix.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return res, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return res, fmt.Errorf("archive: refusing to put a plaintext index in %s (mode %o): make it mode 0700", dir, fi.Mode().Perm())
	}
	if _, err := os.Stat(ix.Path); err == nil {
		// An older index, or none of this, answers with an error or no row.
		if rows, err := ix.Query("SELECT value FROM meta WHERE key='snapshot_key';"); err == nil && len(rows) == 1 && sget(rows[0], "value") >= path.Base(key) {
			res.UpToDate = true
			if check != nil {
				if err := ix.recordCheck(check.CheckedAt); err != nil {
					return res, err
				}
			}
			return res, nil
		}
	}
	blob, err := a.B.Get(key)
	if err != nil {
		return res, err
	}
	res.Stored = int64(len(blob))
	plain, err := o.Open(blob)
	if err != nil {
		return res, fmt.Errorf("archive: opening %s: %w", key, err)
	}
	blob = nil
	tmp, err := ix.tempBeside("pull")
	if err != nil {
		return res, err
	}
	defer removeDB(tmp)
	if err := unpackSnapshot(tmp, plain); err != nil {
		return res, fmt.Errorf("archive: unpacking %s: %w", key, err)
	}
	plain = nil
	c := &Index{Path: tmp, Sqlite: ix.Sqlite}
	if res.BuiltAt, res.Sessions, err = c.checkSnapshotDB(); err != nil {
		return res, fmt.Errorf("%s: %w", key, err)
	}
	if _, err := c.run("PRAGMA journal_mode=DELETE;", false); err != nil {
		return res, err
	}
	if check != nil {
		if err := c.recordCheck(check.CheckedAt); err != nil {
			return res, err
		}
	}
	if err := ix.foldWAL(); err != nil {
		return res, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return res, err
	}
	if err := os.Rename(tmp, ix.Path); err != nil {
		return res, err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return res, nil
}

// recordCheck stores when the indexer last looked. Readers wait out the moment
// this takes.
func (ix *Index) recordCheck(at string) error {
	_, err := ix.run("INSERT OR REPLACE INTO meta VALUES('checked_at',"+q(at)+");", false)
	return err
}

// unpackSnapshot decompresses to path, checking zstd's content checksum.
func unpackSnapshot(path string, compressed []byte) error {
	zr, err := zstd.NewReader(bytes.NewReader(compressed), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer zr.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(zr, snapshotMaxBytes+1))
	if err == nil && n > snapshotMaxBytes {
		err = errors.New("larger than any index should be")
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// foldWAL folds the index's write-ahead log into it and leaves it in rollback
// mode, before something else is moved to its path. SQLite applies a -wal file
// it finds beside a database whatever that database's own mode says, so a log
// left there would be applied to the new file.
func (ix *Index) foldWAL() error {
	wal := ix.Path + "-wal"
	if _, err := os.Stat(ix.Path); os.IsNotExist(err) {
		os.Remove(wal)
		os.Remove(ix.Path + "-shm")
		return nil
	}
	// A reader in the middle of a query blocks this, and SQLite does not wait
	// for it here. Queries are short and time-limited.
	var err error
	for end := time.Now().Add(foldWait); ; time.Sleep(foldWait / 16) {
		if _, err = ix.run("PRAGMA journal_mode=DELETE;", false); err == nil {
			if _, serr := os.Stat(wal); serr != nil {
				return nil
			}
			// The pragma can answer with the old mode when it cannot change it.
			err = errors.New("the database is busy")
		}
		if time.Now().After(end) {
			break
		}
	}
	if _, serr := os.Stat(wal); serr != nil {
		return nil // no log to apply, whatever is wrong with the old file
	}
	return fmt.Errorf("archive: could not close the old index's log, so the index was not replaced: %v. If it is damaged, remove %s and its -wal and -shm files, then run this again", err, ix.Path)
}
