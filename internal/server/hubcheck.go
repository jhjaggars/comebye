package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The hub-checked block: facts the hub can verify about a worker's work,
// shown beside its report, with the report's claims that contradict them
// flagged. A report's Verification section is the worker's own grading;
// this is the hub's, from git and CI, cheap and without reading prose beyond
// a few plain patterns (a word that looks like a SHA, "pushed", "CI green",
// "worktree clean").

// commitFact is where a commit named in a report is.
type commitFact struct {
	SHA      string `json:"sha"`
	Exists   bool   `json:"exists"`
	OnBranch bool   `json:"on_branch"`
	InBase   bool   `json:"in_base"`
}

// verifyCommits looks each of shas up in dir: whether it is a commit, and
// whether the tip (the worker's branch) and baseRef contain it.
func verifyCommits(ctx context.Context, dir, tip, baseRef string, shas []string) []commitFact {
	var out []commitFact
	for _, sha := range shas {
		f := commitFact{SHA: sha}
		full, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", sha+"^{commit}")
		if err == nil && full != "" {
			f.Exists = true
			if tip != "" {
				_, e := runGit(ctx, dir, "merge-base", "--is-ancestor", full, tip)
				f.OnBranch = e == nil
			}
			if baseRef != "" {
				_, e := runGit(ctx, dir, "merge-base", "--is-ancestor", full, baseRef)
				f.InBase = e == nil
			}
		}
		out = append(out, f)
	}
	return out
}

// addVerified fills in what opts asked the hub-check for.
func addVerified(ctx context.Context, c *worktreeChecks, dir string, opts checkOpts) {
	if len(opts.Verify) > 0 {
		base := c.baseRef
		if base == "" {
			base = c.BaseUsed
		}
		c.Verified = verifyCommits(ctx, dir, c.Tip, base, opts.Verify)
	}
	if opts.CIHead && c.Tip != "" && c.CI == nil {
		c.CI = ciOf(ctx, dir, c.Tip)
	}
}

// hubItem is one line of the block. State is ok, warn, bad (the report
// says otherwise) or info.
type hubItem struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
	Text  string `json:"text"`
}

// hubCheck is the block for one report.
type hubCheck struct {
	ID    string    `json:"id"`
	Items []hubItem `json:"items"`
	// Contradictions counts the bad items.
	Contradictions int       `json:"contradictions"`
	At             time.Time `json:"at"`
	// Note says why there is nothing to show (no report, the host could not
	// be reached).
	Note string `json:"note,omitempty"`
}

var (
	pushedRe    = regexp.MustCompile(`(?i)\bpushed\b`)
	notPushedRe = regexp.MustCompile(`(?i)(\bnot|n't|\bun|\bnever|\bwithout|\bno)[ -]?(been |yet )?pushed|\bpushed\b.*\b(not|never)\b`)
	ciGreenRe   = regexp.MustCompile(`(?i)\bCI\b.{0,40}\b(green|passing|passed|succe\w+)\b|\b(green|passing)\b.{0,20}\bCI\b`)
	ciNegRe     = regexp.MustCompile(`(?i)\b(not|n't|fail\w*|red|pending|running|unknown|no)\b`)
	cleanRe     = regexp.MustCompile(`(?i)(worktree|working tree|work tree)\b.{0,12}\bclean\b|\bclean (worktree|working tree)`)
	cleanNegRe  = regexp.MustCompile(`(?i)\b(not|n't|un\w+|dirty)\b`)
)

// claimed reports whether some line of text matches yes and none of the
// negations no on that line.
func claimed(text string, yes, no *regexp.Regexp) bool {
	for _, l := range strings.Split(text, "\n") {
		if yes.MatchString(l) && !no.MatchString(l) {
			return true
		}
	}
	return false
}

// hubCheckOf builds the block from a report and the checks run for it.
func hubCheckOf(e ledger.Entry, c *worktreeChecks, now time.Time) hubCheck {
	h := hubCheck{ID: e.ID, At: now.UTC(), Items: []hubItem{}}
	if strings.TrimSpace(e.FinalReport) == "" {
		h.Note = "no report yet"
		return h
	}
	if c == nil {
		h.Note = "the hub could not run its checks"
		return h
	}
	add := func(kind, state, f string, a ...any) {
		h.Items = append(h.Items, hubItem{Kind: kind, State: state, Text: fmt.Sprintf(f, a...)})
		if state == "bad" {
			h.Contradictions++
		}
	}
	report := e.FinalReport

	// Commits named.
	for _, f := range c.Verified {
		s := shortSHA(f.SHA)
		base := orDefault(c.BaseUsed, "the base")
		switch {
		case !f.Exists:
			add("commit", "bad", "%s: the report names it, but it is not a commit in this repository", s)
		case f.InBase && f.OnBranch:
			add("commit", "ok", "%s: on the branch and in %s", s, base)
		case f.InBase:
			add("commit", "ok", "%s: in %s", s, base)
		case f.OnBranch:
			add("commit", "ok", "%s: on the branch, not in %s", s, base)
		default:
			add("commit", "warn", "%s: exists, but is on neither the branch nor %s", s, base)
		}
	}

	// Pushed.
	said := claimed(report, pushedRe, notPushedRe)
	d := c.Detail
	switch {
	case c.Gone || d == nil || d.Unpushed < 0:
		if said {
			add("pushed", "info", "the report says pushed; the hub cannot tell (no worktree to look at)")
		}
	case d.Unpushed > 0:
		st := "warn"
		if said {
			st = "bad"
		}
		msg := fmt.Sprintf("%d commit(s) are on no remote", d.Unpushed)
		if said {
			msg = "the report says pushed, but " + msg
		}
		add("pushed", st, "%s", msg)
	case d.Upstream != "":
		add("pushed", "ok", "pushed: nothing is missing from %s", d.Upstream)
	case c.Unmerged == 0:
		add("pushed", "ok", "nothing to push: every commit is in %s", orDefault(c.BaseUsed, "the base"))
	default:
		add("pushed", "info", "no commits missing from a remote")
	}

	// CI of the head.
	green := claimed(report, ciGreenRe, ciNegRe)
	switch ci := c.CI; {
	case ci == nil:
		if green {
			add("ci", "info", "the report says CI is green; the hub has no CI result for the head")
		}
	case ci.State == ledger.CIFailure:
		st, msg := "warn", "CI failed for the head %s"
		if green {
			st, msg = "bad", "the report says CI is green, but CI failed for the head %s"
		}
		add("ci", st, msg, shortSHA(ci.SHA))
	case ci.State == ledger.CIRunning:
		add("ci", "info", "CI is still running for the head %s", shortSHA(ci.SHA))
	case ci.State == ledger.CISuccess:
		add("ci", "ok", "CI is green for the head %s", shortSHA(ci.SHA))
	default:
		add("ci", "info", "no CI runs for the head %s", shortSHA(ci.SHA))
	}

	// Worktree.
	cleanSaid := claimed(report, cleanRe, cleanNegRe)
	switch {
	case c.Gone || !c.Exists:
		add("worktree", "info", "the worktree is gone")
	case c.Dirty:
		st, msg := "warn", "the worktree has %d uncommitted file(s)"
		if cleanSaid {
			st, msg = "bad", "the report says the worktree is clean, but it has %d uncommitted file(s)"
		}
		add("worktree", st, msg, c.DirtyFiles)
	default:
		add("worktree", "ok", "the worktree is clean")
	}
	return h
}

// handleHubCheck answers GET /api/ledger/{id}/hubcheck.
func (s *Server) handleHubCheck(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	e, ok := s.ledger.Get(r.PathValue("id"))
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_handoff", "message": "no such handoff"})
		return
	}
	if strings.TrimSpace(e.FinalReport) == "" || e.Worktree == "" {
		writeJSON(w, hubCheckOf(e, nil, time.Now()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	opts := checkOpts{Detail: true, Verify: hexOnly(shasIn(e.FinalReport)), CIHead: true}.forEntry(e)
	c, lerr := s.checksOn(ctx, e.Host, e.Worktree, e.Branch, e.Base, opts)
	h := hubCheckOf(e, c, time.Now())
	if lerr != nil {
		h.Note = "the hub could not run its checks: " + lerr.message
	}
	writeJSON(w, h)
}
