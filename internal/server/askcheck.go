package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// Re-checking what a session said keeps it. A session's UNSAFE answer is
// frozen when it is recorded, but the things it names move on: the PR it
// watched merges, the to-do it filed is closed. Each time the removals
// review is built the hub looks at the typed items of the answer again:
//
//	pr        gh pr view on the session's host (merged or closed: resolved)
//	todo      this hub's own to-do list (done or dropped: resolved)
//	commits   the branch checks (all in the base, or all on a remote)
//	resource  a pid alive on the host, a k3d cluster still there
//	other     free text: never resolved by the hub
//
// An item the hub cannot check ("unchecked") stays a warning, like an
// unresolved one. When every item has resolved, the UNSAFE is a note.

const (
	probeTTL = time.Minute
	// reaskGap is the least time between asks of one mark by the hub;
	// reaskCap is how old an unresolved answer gets before the hub asks
	// again; reaskMax caps the hub's automatic re-asks of one mark.
	reaskGap = 30 * time.Minute
	// openGap is the least time between asks when the review is opened.
	openGap    = 2 * time.Minute
	reaskCap   = time.Hour // an answer older than this is not trusted: the session is asked again
	reaskMax   = 12
	reaskEvery = 2 * time.Minute
	// recheckWait bounds the re-checks of one answer.
	recheckWait = 25 * time.Second

	itemResolved  = "resolved"
	itemOpen      = "unresolved"
	itemUnchecked = "unchecked"
)

// probeResult is what a host found when probed.
type probeResult struct {
	// State: merged, closed or open (a PR); alive or dead (a pid);
	// present or absent (a cluster); unknown with Detail saying why.
	State  string     `json:"state"`
	At     *time.Time `json:"at,omitempty"`
	Title  string     `json:"title,omitempty"`
	URL    string     `json:"url,omitempty"`
	Detail string     `json:"detail,omitempty"`
}

var (
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
)

// The probes, variables so tests can stand in for gh, the process table
// and k3d.
var (
	probeGHPR = func(ctx context.Context, repo string, n int) (*probeResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(n), "-R", repo, "--json", "state,mergedAt,closedAt,title,url")
		cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if msg == "" {
				msg = err.Error()
			}
			return nil, errors.New(clip(msg, 200))
		}
		var v struct {
			State, Title, URL  string
			MergedAt, ClosedAt *time.Time
		}
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			return nil, err
		}
		r := &probeResult{State: strings.ToLower(v.State), Title: v.Title, URL: v.URL}
		switch r.State {
		case "merged":
			r.At = v.MergedAt
		case "closed":
			r.At = v.ClosedAt
		}
		return r, nil
	}
	probePIDAlive = func(pid int) bool {
		err := syscall.Kill(pid, 0)
		return err == nil || errors.Is(err, syscall.EPERM)
	}
	probeK3D = func(ctx context.Context, name string) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "k3d", "cluster", "list", "-o", "json").Output()
		if err != nil {
			return false, err
		}
		var cs []struct{ Name string }
		if err := json.Unmarshal(out, &cs); err != nil {
			return false, err
		}
		for _, c := range cs {
			if c.Name == name {
				return true, nil
			}
		}
		return false, nil
	}
)

// probeLocal answers a probe on this host.
func probeLocal(ctx context.Context, q url.Values) (*probeResult, *launchError) {
	bad := func(msg string) (*probeResult, *launchError) {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_probe", message: msg}
	}
	switch q.Get("kind") {
	case "pr":
		n, err := strconv.Atoi(q.Get("number"))
		if !repoRe.MatchString(q.Get("repo")) || err != nil || n < 1 || n > 1<<30 {
			return bad("want repo OWNER/NAME and a number")
		}
		r, err := probeGHPR(ctx, q.Get("repo"), n)
		if err != nil {
			return &probeResult{State: "unknown", Detail: "gh: " + err.Error()}, nil
		}
		return r, nil
	case "pid":
		pid, err := strconv.Atoi(q.Get("pid"))
		if err != nil || pid < 2 || pid > 1<<22 {
			return bad("want a pid")
		}
		if probePIDAlive(pid) {
			return &probeResult{State: "alive"}, nil
		}
		return &probeResult{State: "dead"}, nil
	case "k3d":
		if !nameRe.MatchString(q.Get("name")) {
			return bad("want a cluster name")
		}
		ok, err := probeK3D(ctx, q.Get("name"))
		switch {
		case err != nil:
			return &probeResult{State: "unknown", Detail: "k3d: " + clip(err.Error(), 200)}, nil
		case ok:
			return &probeResult{State: "present"}, nil
		}
		return &probeResult{State: "absent"}, nil
	}
	return bad("unknown probe")
}

// handleProbe is GET /api/probe: the hub's re-check of one thing on this host.
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	res, lerr := probeLocal(ctx, r.URL.Query())
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

// probeOn probes host: here, or through the host checks path on a peer.
func (s *Server) probeOn(ctx context.Context, host string, q url.Values) (*probeResult, *launchError) {
	if host == s.fleet.LocalName() {
		return probeLocal(ctx, q)
	}
	var res probeResult
	if e := s.callHost(ctx, host, "probe", q, nil, &res); e != nil {
		return nil, e
	}
	return &res, nil
}

// probeCache holds re-check results for probeTTL, so a review rebuilt every
// few seconds does not run gh each time.
type probeCache struct {
	mu sync.Mutex
	m  map[string]probeEntry
}

type probeEntry struct {
	at  time.Time
	val any
}

func newProbeCache() *probeCache { return &probeCache{m: map[string]probeEntry{}} }

// cached returns key's value when fresh, else computes it with fn.
func (s *Server) cached(key string, fn func() any) any {
	c := s.probes
	if c == nil || s.probeTTL <= 0 {
		return fn()
	}
	c.mu.Lock()
	if e, ok := c.m[key]; ok && time.Since(e.at) < s.probeTTL {
		c.mu.Unlock()
		return e.val
	}
	c.mu.Unlock()
	v := fn()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 500 {
		for k, e := range c.m {
			if time.Since(e.at) > s.probeTTL {
				delete(c.m, k)
			}
		}
	}
	c.m[key] = probeEntry{at: time.Now(), val: v}
	return v
}

// itemView is one item of an answer with what the hub found when it looked.
type itemView struct {
	ledger.AskItem
	Label string `json:"label"`
	// State: resolved (what it named has moved on), unresolved, or
	// unchecked (the hub cannot tell). Now says so, to follow the label.
	State string `json:"state"`
	Now   string `json:"now"`
	// ChangedAt is when it resolved, when known (a PR merging, a to-do
	// closing): an event after the answer is a reason to ask again.
	ChangedAt *time.Time `json:"changed_at,omitempty"`
}

// askReview is a mark's answer with its items re-checked.
type askReview struct {
	State      string     `json:"state"`
	AnsweredAt time.Time  `json:"answered_at"`
	Items      []itemView `json:"items,omitempty"`
	// Resolved: an UNSAFE answer whose items have all resolved.
	Resolved bool `json:"resolved,omitempty"`
	// Event: some item resolved after the answer was given.
	Event     bool       `json:"event,omitempty"`
	Reasking  *time.Time `json:"reasking,omitempty"`
	ReaskNote string     `json:"reask_note,omitempty"`
	Reasks    int        `json:"reasks,omitempty"`
}

// agoText says how long ago t was.
func agoText(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d d ago", int(d.Hours()/24))
}

// askReviewOf re-checks an answer's items for the item it. It returns nil
// for a mark with no answer.
func (s *Server) askReviewOf(ctx context.Context, it *removal) *askReview {
	a := it.Mark.Ask
	if a == nil {
		return nil
	}
	ar := &askReview{State: a.State, AnsweredAt: a.At, Reasking: a.ReaskedAt, ReaskNote: a.ReaskNote, Reasks: a.Reasks}
	if a.State != ledger.AskUnsafe {
		return ar
	}
	items := a.Items
	if len(items) == 0 {
		items = []ledger.AskItem{{Type: ledger.ItemOther, Text: a.Reason}}
	}
	cctx, cancel := context.WithTimeout(ctx, recheckWait)
	defer cancel()
	ar.Items = make([]itemView, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := itemView{AskItem: item, Label: itemLabel(item)}
			v.State, v.Now, v.ChangedAt = s.recheck(cctx, it, item)
			ar.Items[i] = v
		}()
	}
	wg.Wait()
	ar.Resolved = true
	for _, v := range ar.Items {
		if v.State != itemResolved {
			ar.Resolved = false
		}
		if v.ChangedAt != nil && v.ChangedAt.After(a.At) {
			ar.Event = true
		}
	}
	return ar
}

// recheck looks at one item again.
func (s *Server) recheck(ctx context.Context, it *removal, item ledger.AskItem) (state, now string, changed *time.Time) {
	unchecked := func(why string) (string, string, *time.Time) { return itemUnchecked, "can't check: " + why, nil }
	switch item.Type {
	case ledger.ItemPR:
		if item.Repo == "" {
			return unchecked("the answer names no repository")
		}
		if !repoRe.MatchString(item.Repo) {
			return unchecked("not a repository name")
		}
		host := it.Host
		if !s.knownHost(host) {
			return unchecked("host " + host + " is not in this hub's fleet")
		}
		q := url.Values{"kind": {"pr"}, "repo": {item.Repo}, "number": {strconv.Itoa(item.Number)}}
		r, why := s.probeCached(ctx, host, q)
		if r == nil {
			return unchecked(why)
		}
		when := ""
		if r.At != nil {
			when = " " + agoText(time.Now(), *r.At)
		}
		switch r.State {
		case "merged":
			return itemResolved, "merged" + when, r.At
		case "closed":
			return itemResolved, "was closed without merging" + when, r.At
		case "open":
			return itemOpen, "is still open", nil
		}
		return unchecked(orDefault(r.Detail, "gh said "+r.State))
	case ledger.ItemTodo:
		if s.todos == nil {
			return unchecked("this hub has no to-do list")
		}
		t, ok := s.todos.get(item.ID)
		if !ok {
			return unchecked("not in this hub's to-do list (it may have been pruned)")
		}
		switch t.State {
		case TodoDone, TodoDropped:
			when := ""
			if t.ClosedAt != nil {
				when = " " + agoText(time.Now(), *t.ClosedAt)
			}
			return itemResolved, "is " + t.State + when, t.ClosedAt
		case TodoAnswered:
			return itemOpen, "is answered but not yet done", nil
		}
		return itemOpen, "is still open", nil
	case ledger.ItemCommits:
		return s.recheckCommits(ctx, it, item)
	case ledger.ItemResource:
		host := orDefault(item.Host, it.Host)
		if !s.knownHost(host) {
			return unchecked("host " + host + " is not in this hub's fleet")
		}
		switch item.Kind {
		case "pid":
			pid, err := strconv.Atoi(item.Name)
			if err != nil {
				return unchecked("not a pid")
			}
			r, why := s.probeCached(ctx, host, url.Values{"kind": {"pid"}, "pid": {strconv.Itoa(pid)}})
			if r == nil {
				return unchecked(why)
			}
			if r.State == "dead" {
				return itemResolved, "is no longer running", nil
			}
			if r.State == "alive" {
				return itemOpen, "is still running", nil
			}
			return unchecked(orDefault(r.Detail, r.State))
		case "k3d-cluster":
			if !nameRe.MatchString(item.Name) {
				return unchecked("not a cluster name")
			}
			r, why := s.probeCached(ctx, host, url.Values{"kind": {"k3d"}, "name": {item.Name}})
			if r == nil {
				return unchecked(why)
			}
			if r.State == "absent" {
				return itemResolved, "is gone", nil
			}
			if r.State == "present" {
				return itemOpen, "is still there", nil
			}
			return unchecked(orDefault(r.Detail, r.State))
		}
		return unchecked("the hub does not look for a " + orDefault(item.Kind, "resource") + " of that kind")
	}
	return unchecked("free text")
}

// probeCached probes host, remembering the answer; r is nil with why when
// the probe failed.
func (s *Server) probeCached(ctx context.Context, host string, q url.Values) (r *probeResult, why string) {
	type out struct {
		r   *probeResult
		why string
	}
	v := s.cached("probe|"+host+"|"+q.Encode(), func() any {
		res, lerr := s.probeOn(ctx, host, q)
		if lerr != nil {
			return out{why: host + ": " + lerr.message}
		}
		return out{r: res}
	}).(out)
	return v.r, v.why
}

// recheckCommits settles a "commits" item with the branch checks: every
// commit in the base, or every commit on a remote, is resolved.
func (s *Server) recheckCommits(ctx context.Context, it *removal, item ledger.AskItem) (string, string, *time.Time) {
	unchecked := func(why string) (string, string, *time.Time) { return itemUnchecked, "can't check: " + why, nil }
	c := it.Checks
	if c == nil || (item.Branch != "" && item.Branch != it.Branch && item.Branch != c.Branch) {
		if it.Worktree == "" {
			return unchecked("the session has no worktree of its own")
		}
		if !s.knownHost(it.Host) {
			return unchecked("host " + it.Host + " is not in this hub's fleet")
		}
		if item.Branch != "" && !validBranch(item.Branch) {
			return unchecked("not a branch name")
		}
		branch := orDefault(item.Branch, it.Branch)
		type out struct {
			c   *worktreeChecks
			why string
		}
		v := s.cached("commits|"+it.Host+"|"+it.Worktree+"|"+branch+"|"+it.Base, func() any {
			opts := it.opts
			opts.Detail = true
			c, lerr := s.checksOn(ctx, it.Host, it.Worktree, branch, it.Base, opts)
			if lerr != nil {
				return out{why: lerr.message}
			}
			return out{c: c}
		}).(out)
		if v.c == nil {
			return unchecked(v.why)
		}
		c = v.c
	}
	switch {
	case !c.Exists && c.Gone:
		if warn, msg := c.goneVerdict(); !warn {
			return itemResolved, "are accounted for: " + msg, nil
		} else {
			return itemOpen, "are not accounted for: " + msg, nil
		}
	case !c.Exists:
		return unchecked("the worktree is gone")
	case c.Unmerged < 0 || c.Error != "":
		return unchecked(orDefault(c.Error, "the branch could not be checked"))
	case c.Unmerged == 0:
		return itemResolved, "are all in " + orDefault(c.BaseUsed, "the base"), nil
	case c.Equivalent != "":
		return itemResolved, "have no equal in " + orDefault(c.BaseUsed, "the base") + ", but " + c.Equivalent, nil
	case c.Detail != nil && c.Detail.Unpushed == 0:
		return itemResolved, fmt.Sprintf("are all on a remote (%d not in %s)", c.Unmerged, orDefault(c.BaseUsed, "the base")), nil
	}
	n := fmt.Sprintf("%d not in %s", c.Unmerged, orDefault(c.BaseUsed, "the base"))
	if c.Detail != nil && c.Detail.Unpushed > 0 {
		n += fmt.Sprintf(", %d on no remote", c.Detail.Unpushed)
	}
	return itemOpen, "are not safe (" + n + ")", nil
}

// said is the sentence a resolved UNSAFE becomes.
func (ar *askReview) said() string {
	var parts []string
	for _, v := range ar.Items {
		parts = append(parts, v.Label+" "+v.Now)
	}
	return "the session said UNSAFE at " + ar.AnsweredAt.UTC().Format("15:04 UTC") + " (" + agoText(time.Now(), ar.AnsweredAt) + "), but " + joinAnd(parts)
}

func joinAnd(p []string) string {
	switch len(p) {
	case 0:
		return ""
	case 1:
		return p[0]
	}
	return strings.Join(p[:len(p)-1], ", ") + " and " + p[len(p)-1]
}

// reviewLines adds the answer's lines to an item's warnings and notes. A
// resolved UNSAFE (or one before the work was captured: asCaptured) is a
// note; otherwise each unresolved item is a warning of its own.
func (ar *askReview) reviewLines(it *removal, asCaptured bool) {
	age := " (answered " + agoText(time.Now(), ar.AnsweredAt) + ")"
	switch {
	case ar.Resolved:
		it.Notes = append(it.Notes, ar.said())
		return
	case asCaptured:
		var parts []string
		for _, v := range ar.Items {
			parts = append(parts, strings.TrimSpace(itemText(v)))
		}
		it.Notes = append(it.Notes, "the session said UNSAFE before its pages were recorded: "+strings.Join(parts, "; ")+age)
		return
	}
	for _, v := range ar.Items {
		if v.State == itemResolved {
			it.Notes = append(it.Notes, "the session said UNSAFE about "+v.Label+", but it "+v.Now)
			continue
		}
		it.Warnings = append(it.Warnings, "the session says UNSAFE: "+itemText(v)+age)
	}
}

// itemText is an unresolved item as one phrase: what it is, what the hub
// found, and the session's own words.
func itemText(v itemView) string {
	if v.Type == ledger.ItemOther {
		if v.Text == "" {
			return "(no reason given)"
		}
		return v.Text
	}
	s := v.Label + " " + v.Now
	if v.Text != "" {
		s += " (" + v.Text + ")"
	}
	return s
}
