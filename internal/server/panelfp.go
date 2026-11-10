package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A poll that finds nothing new should not redo the work. The Diff panel asks
// every 3 s; most polls come back "unchanged", and answering one used to cost
// the full answer (three diffs, secret redaction, JSON) before the signatures
// were compared. Now the host keeps the last answer per checkout with a cheap
// fingerprint of the checkout and, when the fingerprint is the same, answers
// from it without running the diffs.
//
// The fingerprint is read-only (git status never writes the index with
// GIT_OPTIONAL_LOCKS=0): the branch and HEAD, the merge base, git's status
// lines (which carry the staged blob ids), and the size and mtime of every
// path status lists, so a second edit to an already-modified file shows.
// A file changed within panelRacy of the check is not trusted: its size and
// mtime may not move for the next edit (git's own "racily clean" rule), so
// that answer is not kept.

const (
	// panelRacy is how recent a change must be to be distrusted.
	panelRacy = 2 * time.Second
	// panelFPPaths is the most paths whose stat goes into a fingerprint; more
	// than that and the checkout is always read in full.
	panelFPPaths = 2000
	// panelFPStatus is the most status output read for a fingerprint.
	panelFPStatus = 1 << 20
	// panelCacheMax is how many checkouts keep an answer.
	panelCacheMax = 24
)

type panelCacheEntry struct {
	fp   string
	d    panelDiff
	used time.Time
}

var panelCache = struct {
	sync.Mutex
	m map[string]panelCacheEntry
}{m: map[string]panelCacheEntry{}}

func panelCacheGet(top, fp string) (panelDiff, bool) {
	panelCache.Lock()
	defer panelCache.Unlock()
	e, ok := panelCache.m[top]
	if !ok || e.fp != fp {
		return panelDiff{}, false
	}
	e.used = time.Now()
	panelCache.m[top] = e
	return e.d, true
}

func panelCachePut(top, fp string, d panelDiff) {
	panelCache.Lock()
	defer panelCache.Unlock()
	if len(panelCache.m) >= panelCacheMax {
		if _, have := panelCache.m[top]; !have {
			oldest, at := "", time.Time{}
			for k, e := range panelCache.m {
				if oldest == "" || e.used.Before(at) {
					oldest, at = k, e.used
				}
			}
			delete(panelCache.m, oldest)
		}
	}
	panelCache.m[top] = panelCacheEntry{fp: fp, d: d, used: time.Now()}
}

func panelCacheDrop(top string) {
	panelCache.Lock()
	delete(panelCache.m, top)
	panelCache.Unlock()
}

// statusPaths lists the paths in `git status --porcelain=v2 -z` output.
func statusPaths(out []byte) []string {
	var paths []string
	recs := strings.Split(string(out), "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if r == "" || r[0] == '#' || r[0] == '!' {
			continue
		}
		var skip int // fields before the path
		switch r[0] {
		case '1':
			skip = 8
		case '2':
			skip = 9
			i++ // the original path follows as its own record
		case 'u':
			skip = 10
		case '?':
			paths = append(paths, strings.TrimPrefix(r, "? "))
			continue
		default:
			continue
		}
		f := strings.SplitN(r, " ", skip+1)
		if len(f) == skip+1 {
			paths = append(paths, f[skip])
		}
	}
	return paths
}

// panelFingerprint says "this checkout, as of now". ok is false when it cannot
// be trusted as a key (a huge change set, a file changed a moment ago, or git
// failed); the caller then reads in full and keeps nothing.
func panelFingerprint(ctx context.Context, top, base, mergeBase string) (fp string, ok bool) {
	out, trunc, err := panelGit(ctx, top, panelFPStatus, "status", "--porcelain=v2", "--branch", "--untracked-files=all", "-z")
	if err != nil || trunc {
		return "", false
	}
	paths := statusPaths(out)
	if len(paths) > panelFPPaths {
		return "", false
	}
	h := sha256.New()
	h.Write([]byte(base + "\x00" + mergeBase + "\x00"))
	h.Write(out)
	now := time.Now()
	var buf [24]byte
	for _, p := range paths {
		fi, err := os.Lstat(filepath.Join(top, filepath.FromSlash(p)))
		if err != nil { // deleted: status says so; nothing more to see
			h.Write([]byte{0})
			continue
		}
		if now.Sub(fi.ModTime()) < panelRacy {
			return "", false
		}
		binary.LittleEndian.PutUint64(buf[0:], uint64(fi.ModTime().UnixNano()))
		binary.LittleEndian.PutUint64(buf[8:], uint64(fi.Size()))
		binary.LittleEndian.PutUint64(buf[16:], uint64(fi.Mode()))
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil)[:12]), true
}
