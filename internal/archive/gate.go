package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Each host builds its own index of every host from the store, on a timer that
// fires every few minutes. Most of what such a run would do is often not needed,
// so SyncIfChanged first compares the store and the ledger with what the index
// was last built from. It compares state, not events: nothing has to notify it,
// so a missed event cannot lose anything, and the next tick sees the difference.
//
// The git check fetches over the network and FTS optimize rewrites the whole
// full-text index (about 170 MB of disk writes for a 500 MB index), so those run
// on their own, slower schedules. Between git passes, a commit keeps the verdict
// the last pass gave it.

// Default schedules for SyncIfChanged.
const (
	DefaultGitEvery = time.Hour
	// A day of ordinary activity grows a 500 MB index by a few tens of MB without
	// optimize, and does not slow searches measurably.
	DefaultOptimizeEvery = 24 * time.Hour
)

const (
	metaBuiltAt    = "built_at"
	metaCheckedAt  = "checked_at"
	metaGateStore  = "gate_store"
	metaGateLedger = "gate_ledger"
	metaGitAt      = "git_at"
	metaOptimizeAt = "optimize_at"
)

// GateOptions configure SyncIfChanged.
type GateOptions struct {
	Git, GH bool
	// LedgerOK says the links are what the ledger holds. When it could not be
	// read, sessions are still indexed, but threads wait: their outcomes come
	// from the ledger, and rebuilding them without it would drop those outcomes.
	LedgerOK      bool
	GitEvery      time.Duration // 0 is DefaultGitEvery
	OptimizeEvery time.Duration // 0 is DefaultOptimizeEvery
}

// GateResult reports SyncIfChanged.
type GateResult struct {
	Ran       bool      `json:"ran"` // false: nothing had changed, and only the check time moved
	GitPass   bool      `json:"git_pass,omitempty"`
	Optimized bool      `json:"optimized,omitempty"`
	CheckedAt string    `json:"checked_at"`
	Stats     SyncStats `json:"stats"`
}

// storeState hashes what the store holds that an index is built from. Listing is
// enough: segments are named by offset and length, and notes, tombstones and host
// claims are new objects when they change.
func storeState(b Backend) (string, error) {
	h := sha256.New()
	for _, prefix := range []string{"v1/", "notes/", "tomb/", "hosts/"} {
		keys, err := b.List(prefix)
		if err != nil {
			return "", fmt.Errorf("archive: listing %s: %w", prefix, err)
		}
		fmt.Fprintf(h, "%s %d\n", prefix, len(keys))
		for _, k := range keys {
			fmt.Fprintln(h, k)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func ledgerState(links []Link) (string, error) {
	b, err := json.Marshal(links)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// metaValues reads the meta table; a new or unreadable index has nothing in it,
// which reads as "everything is due".
func (ix *Index) metaValues() map[string]string {
	out := map[string]string{}
	if _, err := os.Stat(ix.Path); err != nil || ix.Outdated() {
		return out
	}
	rows, err := ix.Query("SELECT key, value FROM meta;")
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[sget(r, "key")] = sget(r, "value")
	}
	return out
}

func (ix *Index) setMeta(kv map[string]string) error {
	sql := "BEGIN;"
	for k, v := range kv {
		sql += "INSERT OR REPLACE INTO meta VALUES(" + q(k) + "," + q(v) + ");"
	}
	_, err := ix.run(sql+"COMMIT;", false)
	return err
}

func due(last string, every time.Duration, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, last)
	return err != nil || now.Sub(t) >= every
}

// SyncIfChanged brings an all-hosts index up to date if the store or the ledger
// changed since it was last built, or the git check is due. Either way it records
// that it looked, which is what keeps the index from reading as stale.
func (ix *Index) SyncIfChanged(a *Archiver, o Opener, links []Link, g GateOptions) (GateResult, error) {
	var res GateResult
	if g.GitEvery <= 0 {
		g.GitEvery = DefaultGitEvery
	}
	if g.OptimizeEvery <= 0 {
		g.OptimizeEvery = DefaultOptimizeEvery
	}
	now := a.now().UTC()
	res.CheckedAt = now.Format(time.RFC3339)
	meta := ix.metaValues()

	store, err := storeState(a.B)
	if err != nil {
		return res, err
	}
	ledger := ""
	if g.LedgerOK {
		if ledger, err = ledgerState(links); err != nil {
			return res, err
		}
	}
	// A git pass rebuilds threads, which needs the ledger.
	gitDue := g.Git && g.LedgerOK && due(meta[metaGitAt], g.GitEvery, now)
	changed := store != meta[metaGateStore] || (g.LedgerOK && ledger != meta[metaGateLedger])
	if !changed && !gitDue {
		return res, ix.setMeta(map[string]string{metaCheckedAt: res.CheckedAt})
	}

	res.Ran, res.GitPass = true, gitDue
	optimize := due(meta[metaOptimizeAt], g.OptimizeEvery, now)
	res.Stats, err = ix.Sync(a, o, links, SyncOptions{
		AllHosts:     true,
		Git:          g.Git,
		GitCached:    g.Git && !gitDue,
		GH:           g.GH && gitDue,
		SkipOptimize: !optimize,
		NoThreads:    !g.LedgerOK,
	})
	if err != nil {
		return res, err
	}
	res.Optimized = optimize
	// A host that could not be read gets retried on the next tick, and threads
	// that were skipped get built on the first tick that can read the ledger.
	if len(res.Stats.FailedHosts) > 0 {
		store = ""
	}
	kv := map[string]string{metaGateStore: store, metaGateLedger: ledger, metaCheckedAt: res.CheckedAt}
	if gitDue {
		kv[metaGitAt] = res.CheckedAt
	}
	if err := ix.setMeta(kv); err != nil {
		return res, err
	}
	if len(res.Stats.FailedHosts) > 0 {
		return res, fmt.Errorf("archive: could not read %v; their earlier rows stay", res.Stats.FailedHosts)
	}
	return res, nil
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

// foldWait is how long foldWAL keeps trying: longer than a Reader's query may run.
var foldWait = 4 * time.Second
