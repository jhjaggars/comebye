// Package scratch cleans up what agent sessions leave in the host's temporary
// directory. On many Linux hosts /tmp is a tmpfs, i.e. RAM: Claude Code's
// per-session scratchpads (/tmp/claude-<uid>/<project>/<session>/scratchpad),
// the mktemp directories tests leave behind (tmp.*, hl.*) and the dev servers
// and browsers that run out of them can fill it and get the desktop session
// killed by the OOM killer.
//
// Everything here is conservative: it only touches paths owned by the
// current user, never a path a live agent works in, and it can run as a dry
// run that reports and removes nothing.
package scratch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Defaults for Prune.
const (
	DefaultMinAge    = 6 * time.Hour  // a scratchpad project untouched this long may be pruned
	DefaultTmpMaxAge = 48 * time.Hour // a tmp.* directory untouched this long may be pruned
	DefaultStopWait  = 2 * time.Second
	// WarnPct is the /tmp usage above which Status warns.
	WarnPct = 80
)

// tmpPrefixes are the top-level names (<prefix>.XXXX) Prune may remove: what
// mktemp -d and the test helpers create.
var tmpPrefixes = []string{"tmp.", "hl."}

// protectedComm are processes never stopped, whatever their cwd: the user's
// shells and the tools that host the agents.
var protectedComm = map[string]bool{
	"bash": true, "fish": true, "zsh": true, "sh": true, "dash": true, "nu": true,
	"tmux": true, "screen": true, "herdr": true, "comebye": true, "sshd": true, "ssh": true,
	"claude": true, "pi": true, "systemd": true,
}

// Options says where and as whom to work. The zero value works on /tmp as
// the current user.
type Options struct {
	TmpDir  string // default /tmp
	UID     int    // the owner whose files may be touched (default os.Getuid)
	ProcDir string // default /proc
	DryRun  bool
	Now     func() time.Time
	// Kill signals a process (default syscall.Kill); tests inject a fake.
	Kill func(pid int, sig syscall.Signal) error
	// StopWait is how long a process gets after SIGTERM before SIGKILL.
	StopWait time.Duration
	Logf     func(format string, args ...any)
}

func (o *Options) fill() {
	if o.TmpDir == "" {
		o.TmpDir = "/tmp"
	}
	if o.UID == 0 && os.Getuid() != 0 {
		o.UID = os.Getuid()
	}
	if o.ProcDir == "" {
		o.ProcDir = "/proc"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Kill == nil {
		o.Kill = syscall.Kill
	}
	if o.StopWait == 0 {
		o.StopWait = DefaultStopWait
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
}

// Item is one path removed (or that would be) or kept.
type Item struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
}

// Proc is a process stopped (or that would be).
type Proc struct {
	PID    int    `json:"pid"`
	Name   string `json:"name"`
	CWD    string `json:"cwd,omitempty"`
	Reason string `json:"reason"`
}

// Report is what a cleanup did.
type Report struct {
	DryRun  bool     `json:"dry_run"`
	Removed []Item   `json:"removed"`
	Kept    []Item   `json:"kept,omitempty"`
	Stopped []Proc   `json:"stopped"`
	Bytes   int64    `json:"bytes"`
	Errors  []string `json:"errors,omitempty"`
}

// Empty says nothing was (or would be) done.
func (r Report) Empty() bool { return len(r.Removed) == 0 && len(r.Stopped) == 0 && len(r.Errors) == 0 }

// Summary is a one-line account for logs and the CLI.
func (r Report) Summary() string {
	verb := "removed"
	if r.DryRun {
		verb = "would remove"
	}
	s := verb + " " + strconv.Itoa(len(r.Removed)) + " path(s), " + HumanBytes(r.Bytes)
	if n := len(r.Stopped); n > 0 {
		sv := "stopped"
		if r.DryRun {
			sv = "would stop"
		}
		s += "; " + sv + " " + strconv.Itoa(n) + " process(es)"
	}
	return s
}

func (r *Report) merge(o Report) {
	r.Removed = append(r.Removed, o.Removed...)
	r.Kept = append(r.Kept, o.Kept...)
	r.Stopped = append(r.Stopped, o.Stopped...)
	r.Bytes += o.Bytes
	r.Errors = append(r.Errors, o.Errors...)
}

// HumanBytes formats a size (1.5 GB).
func HumanBytes(n int64) string {
	const k = 1024
	if n < k {
		return strconv.FormatInt(n, 10) + " B"
	}
	f, unit := float64(n), "KMGTP"
	i := -1
	for f >= k && i < len(unit)-1 {
		f /= k
		i++
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " " + string(unit[i]) + "B"
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// ProjectDir is the name Claude Code gives a working directory under
// /tmp/claude-<uid>: every character that is not a letter or digit becomes -
// (/home/me/.worktrees/x becomes -home-me--worktrees-x).
func ProjectDir(path string) string { return nonAlnum.ReplaceAllString(filepath.Clean(path), "-") }

// ClaudeRoot is the directory holding the user's session scratchpads.
func (o Options) ClaudeRoot() string {
	o.fill()
	return filepath.Join(o.TmpDir, "claude-"+strconv.Itoa(o.UID))
}

// Within reports whether p is dir or inside it.
func Within(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// CleanWorktree removes what the sessions of a removed worktree left in the
// temporary directory (its whole scratchpad project) and stops the processes
// still running in the worktree or the scratchpad (a forgotten dev server, a
// browser). live are the directories live agents work in; if any is inside
// the worktree nothing is touched.
func CleanWorktree(o Options, worktree string, live []string) Report {
	o.fill()
	rep := Report{DryRun: o.DryRun}
	if worktree == "" || !filepath.IsAbs(worktree) || filepath.Clean(worktree) == "/" {
		return rep
	}
	for _, l := range live {
		if Within(l, worktree) {
			rep.Kept = append(rep.Kept, Item{Path: worktree, Reason: "a live agent works there"})
			return rep
		}
	}
	dir := filepath.Join(o.ClaudeRoot(), ProjectDir(worktree))
	roots := []string{worktree}
	haveDir := ownedDir(o, dir)
	if haveDir {
		roots = append(roots, dir)
	}
	stopProcs(o, &rep, findProcs(o, roots), "in the removed worktree or its scratchpad")
	if haveDir {
		removeTree(o, &rep, dir, "scratchpad of a removed worktree")
	}
	logReport(o, "worktree "+worktree, rep)
	return rep
}

// PruneParams are the live facts Prune checks against.
type PruneParams struct {
	// Live are the directories that must keep their scratchpads: the working
	// directories of live agents and every existing worktree and repository.
	Live []string
	// MinAge: a scratchpad project is only pruned when nothing in it changed
	// for this long (default DefaultMinAge).
	MinAge time.Duration
	// TmpMaxAge: tmp.* and hl.* entries older than this are pruned (default
	// DefaultTmpMaxAge); negative turns that part off.
	TmpMaxAge time.Duration
}

// Prune removes scratchpad projects whose worktree no longer exists and the
// user's stale tmp.* / hl.* entries. It stops processes that sit in what it
// removes, except shells and the tools that host agents; a directory a
// protected process sits in is kept.
func Prune(o Options, p PruneParams) Report {
	o.fill()
	if p.MinAge <= 0 {
		p.MinAge = DefaultMinAge
	}
	if p.TmpMaxAge == 0 {
		p.TmpMaxAge = DefaultTmpMaxAge
	}
	rep := Report{DryRun: o.DryRun}
	now := o.Now()
	liveNames := map[string]bool{}
	for _, l := range p.Live {
		liveNames[ProjectDir(l)] = true
	}
	procs := findProcs(o, []string{o.TmpDir}) // one scan, reused below

	root := o.ClaudeRoot()
	if ents, err := os.ReadDir(root); err == nil {
		for _, e := range ents {
			dir := filepath.Join(root, e.Name())
			switch {
			case !ownedDir(o, dir):
				continue
			case liveNames[e.Name()]:
				rep.Kept = append(rep.Kept, Item{Path: dir, Reason: "its worktree exists"})
				continue
			}
			size, newest := scan(dir)
			if now.Sub(newest) < p.MinAge {
				rep.Kept = append(rep.Kept, Item{Path: dir, Bytes: size, Reason: "changed in the last " + p.MinAge.String()})
				continue
			}
			pruneEntry(o, &rep, procs, dir, size, "scratchpad of a worktree that is gone")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		rep.Errors = append(rep.Errors, err.Error())
	}

	if p.TmpMaxAge > 0 {
		ents, err := os.ReadDir(o.TmpDir)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		}
		for _, e := range ents {
			if !hasTmpPrefix(e.Name()) {
				continue
			}
			path := filepath.Join(o.TmpDir, e.Name())
			fi, err := os.Lstat(path)
			if err != nil || fi.Mode()&os.ModeSymlink != 0 || !ownedBy(fi, o.UID) {
				continue
			}
			size, newest := scan(path)
			if age := now.Sub(newest); age < p.TmpMaxAge {
				continue
			}
			pruneEntry(o, &rep, procs, path, size, "unused for more than "+p.TmpMaxAge.String())
		}
	}
	sort.Slice(rep.Removed, func(i, j int) bool { return rep.Removed[i].Bytes > rep.Removed[j].Bytes })
	logReport(o, "prune", rep)
	return rep
}

// pruneEntry removes path unless a protected process sits in it; other
// processes in it are stopped first.
func pruneEntry(o Options, rep *Report, procs []procInfo, path string, size int64, why string) {
	var inside []procInfo
	for _, pr := range procs {
		if pr.in(path) {
			if protectedComm[pr.name] {
				rep.Kept = append(rep.Kept, Item{Path: path, Bytes: size, Reason: "in use by " + pr.name + " (pid " + strconv.Itoa(pr.pid) + ")"})
				return
			}
			inside = append(inside, pr)
		}
	}
	stopProcs(o, rep, inside, "in "+path)
	rep.Removed = append(rep.Removed, Item{Path: path, Bytes: size, Reason: why})
	rep.Bytes += size
	if !o.DryRun {
		if err := os.RemoveAll(path); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		}
	}
}

func removeTree(o Options, rep *Report, path, why string) {
	size, _ := scan(path)
	rep.Removed = append(rep.Removed, Item{Path: path, Bytes: size, Reason: why})
	rep.Bytes += size
	if !o.DryRun {
		if err := os.RemoveAll(path); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		}
	}
}

func hasTmpPrefix(name string) bool {
	for _, p := range tmpPrefixes {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return true
		}
	}
	return false
}

func logReport(o Options, what string, r Report) {
	if r.Empty() {
		return
	}
	for _, it := range r.Removed {
		o.Logf("scratch: %s: %s %s (%s): %s", what, verbOf(r.DryRun, "removed"), it.Path, HumanBytes(it.Bytes), it.Reason)
	}
	for _, pr := range r.Stopped {
		o.Logf("scratch: %s: %s pid %d (%s): %s", what, verbOf(r.DryRun, "stopped"), pr.PID, pr.Name, pr.Reason)
	}
	for _, e := range r.Errors {
		o.Logf("scratch: %s: error: %s", what, e)
	}
}

func verbOf(dry bool, v string) string {
	if dry {
		return "would have " + v
	}
	return v
}

// ownedDir: path is a real directory (not a symlink) owned by the user.
func ownedDir(o Options, path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir() && ownedBy(fi, o.UID)
}

func ownedBy(fi fs.FileInfo, uid int) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}

// scan returns the bytes under path (not following symlinks) and the newest
// modification time of anything in it.
func scan(path string) (size int64, newest time.Time) {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			size += fi.Size()
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return nil
	})
	return size, newest
}

type procInfo struct {
	pid     int
	name    string
	cwd     string
	cmdline []string
}

// in: the process sits in dir, or has it (or --flag=dir) on its command line.
func (p procInfo) in(dir string) bool {
	if Within(p.cwd, dir) {
		return true
	}
	for _, a := range p.cmdline {
		if Within(a, dir) {
			return true
		}
		if i := strings.IndexByte(a, '='); i >= 0 && Within(a[i+1:], dir) {
			return true
		}
	}
	return false
}

// findProcs lists the user's processes whose cwd (a deleted one counts) or
// command line is inside any of roots. The caller's own process and its
// ancestors are never listed.
func findProcs(o Options, roots []string) []procInfo {
	ents, err := os.ReadDir(o.ProcDir)
	if err != nil {
		return nil
	}
	skip := map[int]bool{os.Getpid(): true}
	for pid, n := os.Getppid(), 0; pid > 1 && n < 64; n++ {
		skip[pid] = true
		pid = ppidOf(o, pid)
	}
	var out []procInfo
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || skip[pid] {
			continue
		}
		dir := filepath.Join(o.ProcDir, e.Name())
		fi, err := os.Stat(dir)
		if err != nil || !ownedBy(fi, o.UID) {
			continue
		}
		cwd, _ := os.Readlink(filepath.Join(dir, "cwd"))
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		raw, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		var args []string
		for _, a := range strings.Split(string(raw), "\x00") {
			if a != "" {
				args = append(args, a)
			}
		}
		comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
		pi := procInfo{pid: pid, name: strings.TrimSpace(string(comm)), cwd: cwd, cmdline: args}
		for _, r := range roots {
			if pi.in(r) {
				out = append(out, pi)
				break
			}
		}
	}
	return out
}

func ppidOf(o Options, pid int) int {
	b, err := os.ReadFile(filepath.Join(o.ProcDir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// stopProcs stops the processes (SIGTERM, then SIGKILL after StopWait),
// skipping the protected ones. In a dry run it only reports.
func stopProcs(o Options, rep *Report, procs []procInfo, why string) {
	var sent []procInfo
	for _, p := range procs {
		if protectedComm[p.name] {
			continue
		}
		rep.Stopped = append(rep.Stopped, Proc{PID: p.pid, Name: p.name, CWD: p.cwd, Reason: why})
		if o.DryRun {
			continue
		}
		if err := o.Kill(p.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			rep.Errors = append(rep.Errors, "stop pid "+strconv.Itoa(p.pid)+": "+err.Error())
			continue
		}
		sent = append(sent, p)
	}
	if len(sent) == 0 {
		return
	}
	deadline := time.Now().Add(o.StopWait)
	for time.Now().Before(deadline) {
		alive := false
		for _, p := range sent {
			if procAlive(o, p.pid) {
				alive = true
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range sent {
		if procAlive(o, p.pid) {
			_ = o.Kill(p.pid, syscall.SIGKILL)
		}
	}
}

func procAlive(o Options, pid int) bool {
	_, err := os.Stat(filepath.Join(o.ProcDir, strconv.Itoa(pid)))
	return err == nil
}

// Usage is how full the temporary directory's filesystem is.
type Usage struct {
	Path    string `json:"path"`
	Tmpfs   bool   `json:"tmpfs"` // RAM-backed
	Total   int64  `json:"total"`
	Used    int64  `json:"used"`
	UsedPct int    `json:"used_pct"`
	Warn    bool   `json:"warn,omitempty"` // UsedPct is at or above WarnPct
}

const tmpfsMagic = 0x01021994

// UsageOf reports the filesystem holding path; nil when it cannot be read.
func UsageOf(path string) *Usage {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil || st.Blocks == 0 {
		return nil
	}
	bs := int64(st.Bsize)
	used := int64(st.Blocks-st.Bfree) * bs
	avail := int64(st.Bavail) * bs
	pct := 0
	if used+avail > 0 {
		pct = int((used*100 + (used+avail)/2) / (used + avail))
	}
	return &Usage{Path: path, Tmpfs: int64(st.Type) == tmpfsMagic, Total: used + avail, Used: used, UsedPct: pct, Warn: pct >= WarnPct}
}
