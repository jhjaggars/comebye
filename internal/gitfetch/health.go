package gitfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhjaggars/comebye/internal/state"
)

// Health keeps track of the repositories a server works in, and of the ones whose
// object store git has reported as damaged: by a failed fetch, another git
// command, or the scheduled `git fsck --connectivity-only`. It only reports; it
// repairs nothing. What it knows is saved, so a damaged repository stays listed,
// and the fsck schedule holds, across a restart.
type Health struct {
	Path     string // the saved state; "" keeps it in memory
	Logf     func(format string, args ...any)
	Now      func() time.Time
	OnChange func() // the list of damaged repositories changed
	// FsckEvery is how often each repository is checked (default a day), in the
	// off-peak hours when it can; a repository not checked for twice that long
	// is checked whatever the hour.
	FsckEvery   time.Duration
	FsckTimeout time.Duration          // default 30 minutes
	OffPeak     func(t time.Time) bool // default 02:00-06:00 local time
	LogEvery    time.Duration          // the same damage is logged again after this (default a day)

	mu    sync.Mutex
	repos map[string]*repoHealth
}

type repoHealth struct {
	Seen     time.Time                    `json:"seen"`
	LastFsck time.Time                    `json:"last_fsck,omitempty"`
	FsckTook float64                      `json:"fsck_seconds,omitempty"`
	Damage   map[string]*state.RepoDamage `json:"damage,omitempty"` // by source
	Logged   time.Time                    `json:"logged,omitempty"`
	LoggedAs string                       `json:"logged_as,omitempty"`
}

// OpenHealth reads the saved state at path, if any.
func OpenHealth(path string, logf func(string, ...any)) *Health {
	h := &Health{Path: path, Logf: logf}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &h.repos)
		}
	}
	return h
}

func (h *Health) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Health) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

func (h *Health) repo(common string) *repoHealth {
	if h.repos == nil {
		h.repos = map[string]*repoHealth{}
	}
	r := h.repos[common]
	if r == nil {
		r = &repoHealth{}
		h.repos[common] = r
	}
	return r
}

// save writes the state; called with h.mu held.
func (h *Health) save() {
	if h.Path == "" {
		return
	}
	b, err := json.MarshalIndent(h.repos, "", " ")
	if err != nil {
		return
	}
	tmp := h.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, h.Path)
	}
}

// Known records a repository the server works in, so the scheduled check covers
// it. common is its common git directory.
func (h *Health) Known(common string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.repo(common)
	isNew := r.Seen.IsZero()
	r.Seen = h.now()
	if isNew {
		h.save()
	}
}

// Fetched hears a fetch's result: damage is reported, and a fetch that went
// through clears what a fetch or another git command had reported.
func (h *Health) Fetched(common string, res Result) {
	switch {
	case res.Damaged:
		h.report(common, "fetch", res.Reason, res.BrokenRef)
	case res.Fetched:
		h.clear(common, "fetch", "git")
	}
}

// GitFailed hears the output of another git command that failed in a repository;
// only damage counts.
func (h *Health) GitFailed(common, out string) {
	if IsDamage(out) {
		h.report(common, "git", "the local repository has missing or damaged objects", BrokenRef(out))
	}
}

// RepoPath is the repository's own directory: the checkout that holds the common
// git directory, or the directory itself for a bare repository.
func RepoPath(common string) string {
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common
}

// Hint says how to repair by hand what was found. It never suggests gc or prune.
func Hint(common, ref string) string {
	repo := RepoPath(common)
	if strings.HasPrefix(ref, "refs/remotes/") {
		return fmt.Sprintf("Delete the broken remote-tracking ref and fetch again: git -C %s update-ref -d %s && git -C %s fetch origin. Then check with git -C %s fsck --connectivity-only.", repo, ref, repo, repo)
	}
	return fmt.Sprintf("Restore the missing objects from a remote that has the branch: git -C %s fetch --refetch <remote> <branch> (no destination ref). Do not run gc or prune until git -C %s fsck --connectivity-only is clean.", repo, repo)
}

func worktreesOf(common string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := Command(ctx, common, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var wts []string
	for _, l := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			if e, err := filepath.EvalSymlinks(p); err == nil {
				p = e
			}
			wts = append(wts, p)
		}
	}
	sort.Strings(wts)
	return wts
}

func (h *Health) report(common, source, reason, ref string) {
	wts := worktreesOf(common)
	h.mu.Lock()
	r := h.repo(common)
	if r.Damage == nil {
		r.Damage = map[string]*state.RepoDamage{}
	}
	d := r.Damage[source]
	changed := d == nil || d.Reason != reason || d.Ref != ref
	if d == nil {
		d = &state.RepoDamage{Since: h.now()}
		r.Damage[source] = d
	}
	repo := RepoPath(common)
	d.Repo, d.Path, d.Reason, d.Ref, d.Source, d.Worktrees = filepath.Base(repo), repo, reason, ref, source, wts
	d.Hint = Hint(common, ref)
	key := source + "|" + reason + "|" + ref
	every := h.LogEvery
	if every <= 0 {
		every = 24 * time.Hour
	}
	logIt := key != r.LoggedAs || h.now().Sub(r.Logged) >= every
	if logIt {
		r.Logged, r.LoggedAs = h.now(), key
	}
	at := ""
	if ref != "" {
		at = " at " + ref
	}
	msg := fmt.Sprintf("repository %s is damaged (%s%s, found by %s). %s", repo, reason, at, source, d.Hint)
	h.save()
	h.mu.Unlock()
	if logIt {
		h.logf("%s", msg)
	}
	if changed && h.OnChange != nil {
		h.OnChange()
	}
}

func (h *Health) clear(common string, sources ...string) {
	h.mu.Lock()
	r := h.repos[common]
	changed := false
	if r != nil {
		for _, s := range sources {
			if _, ok := r.Damage[s]; ok {
				delete(r.Damage, s)
				changed = true
			}
		}
		if changed && len(r.Damage) == 0 {
			r.LoggedAs = ""
			h.logf("repository %s: no damage reported any more", RepoPath(common))
		}
	}
	if changed {
		h.save()
	}
	h.mu.Unlock()
	if changed && h.OnChange != nil {
		h.OnChange()
	}
}

// List is the damaged repositories, one entry each: fsck's finding first, as the
// fullest, then another git command's, then a fetch's.
func (h *Health) List() []state.RepoDamage {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []state.RepoDamage
	for _, r := range h.repos {
		for _, s := range []string{"fsck", "git", "fetch"} {
			if d := r.Damage[s]; d != nil {
				out = append(out, *d)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (h *Health) offPeak(t time.Time) bool {
	if h.OffPeak != nil {
		return h.OffPeak(t)
	}
	return t.Hour() >= 2 && t.Hour() < 6
}

// Due lists the repositories whose check is due now.
func (h *Health) Due() []string {
	every := h.FsckEvery
	if every <= 0 {
		every = 24 * time.Hour
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for c, r := range h.repos {
		// A repository never checked waits for the off-peak hours too, so that a
		// restart does not check everything at once.
		age := now.Sub(r.LastFsck)
		if r.LastFsck.IsZero() {
			age = every
		}
		if age >= 2*every || (age >= every && h.offPeak(now)) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// Fsck checks one repository's object store under its fetch lock, at the lowest
// CPU priority, and reports or clears what it finds. A repository that is gone
// is forgotten.
func (h *Health) Fsck(ctx context.Context, common string) error {
	if _, err := os.Stat(common); os.IsNotExist(err) {
		h.mu.Lock()
		delete(h.repos, common)
		h.save()
		h.mu.Unlock()
		return nil
	}
	timeout := h.FsckTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	unlock, err := Lock(ctx, common, timeout)
	if err != nil {
		return fmt.Errorf("fsck %s: %w", RepoPath(common), err)
	}
	defer unlock()
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := Command(fctx, common, "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 19)
	err = cmd.Wait()
	took := time.Since(t0)
	h.mu.Lock()
	r := h.repo(common)
	r.LastFsck, r.FsckTook = h.now(), took.Seconds()
	h.save()
	h.mu.Unlock()
	switch {
	case fctx.Err() == context.DeadlineExceeded:
		return fmt.Errorf("fsck %s: timed out after %s", RepoPath(common), timeout)
	case err == nil:
		h.clear(common, "fsck", "git", "fetch")
		return nil
	case IsDamage(out.String()):
		n := 0
		for _, l := range strings.Split(out.String(), "\n") {
			if damageRE.MatchString(l) {
				n++
			}
		}
		h.report(common, "fsck", fmt.Sprintf("git fsck found %d problems with missing or damaged objects", n), BrokenRef(out.String()))
		return nil
	default:
		return fmt.Errorf("fsck %s: %v", RepoPath(common), err)
	}
}

// Run checks the repositories that are due, one at a time, every interval, until
// ctx ends. known, if set, is called first each round and its repositories
// (common git directories) are recorded with Known.
func (h *Health) Run(ctx context.Context, interval time.Duration, known func() []string) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if known != nil {
			for _, c := range known() {
				h.Known(c)
			}
		}
		for _, c := range h.Due() {
			if ctx.Err() != nil {
				return
			}
			if err := h.Fsck(ctx, c); err != nil {
				h.logf("%v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
