// Package idle decides whether a worker host has been idle long enough to
// stop: no herdr agent is working or waiting on the user, no hold is set
// (work in flight, an upgrade: hold.go), and nothing interactive has been
// active for a while. One implementation serves every host kind: the VM's idle
// timer and a sandbox's init loop both call `comebye idle`.
//
// The check is polled. Each poll that finds activity renews a stamp file;
// the host is idle once the stamp is older than the limit. The stamp
// carries the identity of the current boot (or container run), so a host
// that was stopped and started again begins a fresh clock instead of
// counting the time it was off.
package idle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// Options is one check.
type Options struct {
	Minutes int    // idle limit
	Stamp   string // the stamp file
	NoTouch bool   // read the stamp, never write it (a dry run)

	// Holds is the holds directory (HoldsDir); "" checks none.
	Holds string
	// BlockedMax is how long a blocked agent keeps the host up: 0 is
	// DefaultBlockedMax, negative is no limit. The clock runs from the first
	// poll that saw an agent blocked, and restarts once none is.
	BlockedMax time.Duration

	Herdr    *herdr.Client // nil: herdr's default socket
	ProcRoot string        // default /proc
	Now      func() time.Time
	// Epoch identifies this boot or container run (default: boot id and
	// the start time of process 1).
	Epoch func() string
	// ListAgents replaces the herdr call (tests).
	ListAgents func(ctx context.Context) ([]herdr.AgentInfo, error)
}

// Result is the decision and what it was based on.
type Result struct {
	Idle          bool     `json:"idle"`
	Minutes       int      `json:"minutes"`
	IdleMinutes   int      `json:"idle_minutes"`
	Reasons       []string `json:"reasons"`
	AgentsWorking int      `json:"agents_working"`
	SSHSessions   int      `json:"ssh_sessions"`
	HerdrState    string   `json:"herdr"` // ok, not_running, no_answer
	AgentsBlocked int      `json:"agents_blocked"`
	// BlockedMinutes is how long some agent has been blocked; past
	// BlockedMax blocked agents no longer keep the host up.
	BlockedMinutes int    `json:"blocked_minutes,omitempty"`
	Holds          []Hold `json:"holds,omitempty"`
}

// DefaultBlockedMax is how long a blocked agent keeps a host up by default:
// a day covers a question asked in the evening and answered next morning,
// and still stops a host whose user has gone away.
const DefaultBlockedMax = 24 * time.Hour

// Check runs one poll.
func Check(ctx context.Context, o Options) (Result, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ProcRoot == "" {
		o.ProcRoot = "/proc"
	}
	if o.Epoch == nil {
		o.Epoch = func() string { return bootEpoch(o.ProcRoot) }
	}
	if o.Minutes < 0 {
		return Result{}, errors.New("minutes must not be negative")
	}
	r := Result{Minutes: o.Minutes, Reasons: []string{}}

	// What counts as activity:
	//  - an interactive ssh session (an sshd process holding a pty). A bare
	//    ssh connection does not: herdr keeps one open for as long as a
	//    machine is saved;
	//  - a herdr agent whose status is "working";
	//  - a herdr agent that is "blocked": it waits on the user (a question,
	//    a permission), and stopping the host would throw its session away.
	//    Only for BlockedMax, so a user who has gone away does not keep it
	//    up for ever;
	//  - a hold that has not expired: a prompt or launch an agent has not
	//    picked up yet (a handoff in flight), an upgrade (hold.go);
	//  - herdr running but not answering: not a time to stop on a guess.
	// Not activity: idle, done and unknown.
	r.SSHSessions = sshPtys(o.ProcRoot)
	if r.SSHSessions > 0 {
		r.Reasons = append(r.Reasons, fmt.Sprintf("%d interactive ssh session(s)", r.SSHSessions))
	}
	list := o.ListAgents
	if list == nil {
		c := o.Herdr
		if c == nil {
			c = herdr.NewClient(expandHome(herdr.DefaultSocketPath))
		}
		list = c.ListAgents
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	agents, err := list(cctx)
	cancel()
	now := o.Now()
	epoch := o.Epoch()
	switch {
	case err == nil:
		r.HerdrState = "ok"
		for _, a := range agents {
			switch a.Status {
			case herdr.StatusWorking:
				r.AgentsWorking++
			case herdr.StatusBlocked:
				r.AgentsBlocked++
			}
		}
		if r.AgentsWorking > 0 {
			r.Reasons = append(r.Reasons, fmt.Sprintf("%d herdr agent(s) working", r.AgentsWorking))
		}
		if reason, err := blockedReason(&r, o, now, epoch); err != nil {
			return r, err
		} else if reason != "" {
			r.Reasons = append(r.Reasons, reason)
		}
	case notRunning(err):
		r.HerdrState = "not_running"
	default:
		r.HerdrState = "no_answer"
		r.Reasons = append(r.Reasons, "herdr is up but did not answer: "+oneLine(err))
	}

	if o.Holds != "" {
		r.Holds = ActiveHolds(o.Holds, now)
		for _, h := range r.Holds {
			r.Reasons = append(r.Reasons, fmt.Sprintf("held until %s: %s", h.Until.UTC().Format("15:04Z"), h.Reason))
		}
	}

	last, ok := readStamp(o.Stamp, epoch)
	active := len(r.Reasons) > 0
	if !ok || active {
		last = now
		if !o.NoTouch {
			if err := writeStamp(o.Stamp, now, epoch); err != nil {
				return r, fmt.Errorf("write %s: %w", o.Stamp, err)
			}
		}
	}
	if d := now.Sub(last); d > 0 {
		r.IdleMinutes = int(d / time.Minute)
	}
	r.Idle = !active && now.Sub(last) >= time.Duration(o.Minutes)*time.Minute
	return r, nil
}

// blockedReason keeps the blocked clock (a second stamp beside the idle
// one) and says whether blocked agents keep the host up.
func blockedReason(r *Result, o Options, now time.Time, epoch string) (string, error) {
	path := o.Stamp + ".blocked"
	if r.AgentsBlocked == 0 {
		if !o.NoTouch {
			_ = os.Remove(path)
		}
		return "", nil
	}
	since, ok := readStamp(path, epoch)
	if !ok {
		since = now
		if !o.NoTouch {
			if err := writeStamp(path, now, epoch); err != nil {
				return "", fmt.Errorf("write %s: %w", path, err)
			}
		}
	}
	max := o.BlockedMax
	if max == 0 {
		max = DefaultBlockedMax
	}
	d := now.Sub(since)
	if d > 0 {
		r.BlockedMinutes = int(d / time.Minute)
	}
	if max > 0 && d >= max {
		return "", nil
	}
	return fmt.Sprintf("%d herdr agent(s) blocked, waiting on the user", r.AgentsBlocked), nil
}

// Summary is the one-line human form.
func (r Result) Summary() string {
	switch {
	case len(r.Reasons) > 0:
		return "active: " + strings.Join(r.Reasons, ", ")
	}
	why := "no working or blocked agent, no hold, no interactive session"
	if r.AgentsBlocked > 0 {
		why = fmt.Sprintf("no working agent, %d blocked for %d min (past the limit), no hold, no interactive session", r.AgentsBlocked, r.BlockedMinutes)
	}
	if r.Idle {
		return fmt.Sprintf("idle for %d min (limit %d): %s", r.IdleMinutes, r.Minutes, why)
	}
	return fmt.Sprintf("idle for %d of %d min: %s", r.IdleMinutes, r.Minutes, why)
}

// notRunning: nothing listens on herdr's socket (no socket file, or a stale
// one nobody holds). Anything else (a timeout, a half answer) is a herdr
// that is there and not answering.
func notRunning(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

func oneLine(err error) string { return strings.Join(strings.Fields(err.Error()), " ") }

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// sshPtys counts sshd session processes that hold a pty
// ("sshd: user@pts/3").
func sshPtys(procRoot string) int {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmd := strings.TrimRight(string(b), "\x00")
		if rest, ok := strings.CutPrefix(cmd, "sshd: "); ok && strings.Contains(rest, "@pts/") {
			n++
		}
	}
	return n
}

// bootEpoch names this boot or container run: the kernel's boot id plus
// the start time of process 1 (a container's init restarts with the
// container, the boot id does not).
func bootEpoch(procRoot string) string {
	id, _ := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"))
	start := ""
	if b, err := os.ReadFile(filepath.Join(procRoot, "1", "stat")); err == nil {
		if i := strings.LastIndexByte(string(b), ')'); i >= 0 {
			if f := strings.Fields(string(b)[i+1:]); len(f) > 19 {
				start = f[19] // starttime
			}
		}
	}
	return strings.TrimSpace(string(id)) + ":" + start
}

// The stamp is "<unix seconds> <epoch>".
func readStamp(path, epoch string) (time.Time, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	secs, ep, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil || ep != epoch {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

func writeStamp(path string, t time.Time, epoch string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d %s\n", t.Unix(), epoch)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
