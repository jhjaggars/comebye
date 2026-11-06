package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
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

var snapshotRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.db\.zst$`)

// ErrNoSnapshot means the store holds no snapshot under the prefix.
var ErrNoSnapshot = errors.New("archive: no index snapshot in the store")

// PublishResult reports Publish.
type PublishResult struct {
	Key     string   `json:"key"`
	DBBytes int64    `json:"db_bytes"`
	Stored  int64    `json:"stored_bytes"`
	Pruned  []string `json:"pruned,omitempty"`
	// PruneError: the snapshot is stored, but older ones could not be removed.
	PruneError string `json:"prune_error,omitempty"`
}

// PullResult reports Pull.
type PullResult struct {
	Key      string `json:"key"`
	UpToDate bool   `json:"up_to_date"` // the local file already is this snapshot or a newer one
	BuiltAt  string `json:"built_at,omitempty"`
	Sessions int    `json:"sessions,omitempty"`
	Stored   int64  `json:"stored_bytes,omitempty"`
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

// Publish writes a consistent copy of the index to the store as a new snapshot
// and removes all but the newest two. Only one machine should do this.
func (ix *Index) Publish(a *Archiver, prefix string) (PublishResult, error) {
	var res PublishResult
	if err := checkSnapshotPrefix(prefix); err != nil {
		return res, err
	}
	tmp, err := ix.tempBeside("publish")
	if err != nil {
		return res, err
	}
	defer removeDB(tmp)
	key := prefix + a.now().UTC().Format(snapshotTimeFmt) + ".db.zst"
	if _, err := ix.run("VACUUM INTO "+q(tmp)+";", false); err != nil {
		return res, fmt.Errorf("archive: copying the index: %w", err)
	}
	c := &Index{Path: tmp, Sqlite: ix.Sqlite}
	if _, err := c.run("PRAGMA journal_mode=DELETE; INSERT OR REPLACE INTO meta VALUES('snapshot_key',"+q(path.Base(key))+");", false); err != nil {
		return res, fmt.Errorf("archive: preparing the copy: %w", err)
	}
	if _, _, err := c.checkSnapshotDB(); err != nil {
		return res, fmt.Errorf("not publishing: %w", err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return res, err
	}
	defer f.Close()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return res, err
	}
	if res.DBBytes, err = io.Copy(zw, f); err != nil {
		zw.Close()
		return res, err
	}
	if err := zw.Close(); err != nil {
		return res, err
	}
	sealed, err := a.S.Seal(buf.Bytes())
	if err != nil {
		return res, err
	}
	if err := a.B.Put(key, sealed); err != nil {
		return res, err
	}
	res.Key, res.Stored = key, int64(len(sealed))
	keys, err := snapshots(a.B, prefix)
	if err != nil {
		res.PruneError = err.Error()
		return res, nil
	}
	for len(keys) > snapshotKeep {
		if err := a.B.Delete(keys[0]); err != nil {
			res.PruneError = err.Error()
			return res, nil
		}
		res.Pruned = append(res.Pruned, keys[0])
		keys = keys[1:]
	}
	return res, nil
}

// Pull downloads the newest snapshot, checks it, and moves it over the index.
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
