package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/release"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// The Releases tab on the Peers screen.
//
// A release is no longer tied to a to-do item the user can close. The item
// is a one-line ping with "Open in Peers"; the release itself (its card, the
// fleet table with the Bump hub and Upgrade buttons, the history) lives in
// the tab, which is fed by GET /api/releases:
//
//   - the current release is the newest release item that still needs
//     something: one waiting for the user, being tagged, failed, or tagged
//     with a host that has not upgraded. A tagged release stays here after
//     its item was closed or dropped. When the last host upgrades the item is
//     closed by the hub itself (sweepReleases) and the release shows as done
//     for a day.
//   - an offline host blocks "all upgraded": the release is not done, and the
//     item stays open, until that host runs the version.
//   - the history is the last releaseHistoryMax tagged releases.
//
// Items are kept for todoRetention (14 days) after they close; that bounds
// how far back the history reaches.

const (
	releaseHistoryMax = 5
	// A finished release stays in the tab as "all upgraded" for this long.
	releaseDoneShown = 24 * time.Hour
	// How often the hub looks for rollouts that finished on their own.
	releaseSweepEvery = 30 * time.Second
)

// relRollout counts how far a tagged release has got across the fleet.
type relRollout struct {
	Total    int  `json:"total"`
	Current  int  `json:"current"`  // hosts on the version or newer
	Behind   int  `json:"behind"`   // reachable, not there yet
	Offline  int  `json:"offline"`  // not answering and not there yet
	Complete bool `json:"complete"` // every host is current
}

// markRollout sets Offline and Current on each host for a release version.
// A host is offline when its status is not ok or the hub could not ask it
// for its release status. A host is current when its version parses and is
// not older than the release's: an unknown or unparsable version never is,
// so a host that cannot say what it runs holds the release open.
func markRollout(hosts []relHost, version string) {
	want, wok := parseSemver(version)
	for i := range hosts {
		h := &hosts[i]
		h.Offline = h.Status != "ok" || h.Unknown
		have, ok := parseSemver(h.Version)
		h.Current = wok && ok && !semverLess(have, want)
	}
}

func rolloutOf(hosts []relHost) relRollout {
	ro := relRollout{Total: len(hosts)}
	for _, h := range hosts {
		switch {
		case h.Current:
			ro.Current++
		case h.Offline:
			ro.Offline++
		default:
			ro.Behind++
		}
	}
	ro.Complete = ro.Total > 0 && ro.Current == ro.Total
	return ro
}

// relNeeds says whether the release needs the user right now, and in a few
// words what for. A rollout that waits only on an offline host needs nobody:
// nothing can be clicked until the host is back.
func (s *Server) relNeeds(it Todo, ck *release.Check, bd *release.Build, hosts []relHost, ro relRollout, dep *deployStatus) (bool, string) {
	rl := it.Release
	v := rl.Version
	switch rl.State {
	case relPending:
		if ck == nil || ck.CIStatus != "completed" {
			return false, v + " waits for CI"
		}
		if ck.OK {
			return true, "Approve " + v
		}
		return true, v + " is blocked: " + oneLine(ck.Problem, 120)
	case relFailed:
		return true, "Tagging " + v + " failed"
	case relTagging:
		return false, "Tagging " + v + "…"
	case relTagged:
		if bd != nil {
			switch bd.State {
			case "failure":
				return true, "The build of " + v + " failed"
			case "success":
			default:
				return false, "The build of " + v + " is running"
			}
		}
		if ro.Complete {
			return false, "All upgraded to " + v
		}
		for _, h := range hosts {
			if h.Current {
				continue
			}
			if h.Record != nil && h.Record.Version == v && (h.Record.State == upgrade.RecFailed || h.Record.State == upgrade.RecRolledBack) {
				return true, "Upgrade failed on " + h.Name
			}
		}
		if dep != nil && dep.Record != nil && dep.Record.Version == v && dep.Record.State == depFailed {
			return true, "Deploy of " + v + " failed"
		}
		if ro.Behind > 0 {
			return true, fmt.Sprintf("Roll out %s: %d host%s behind", v, ro.Behind, relS(ro.Behind))
		}
		return false, fmt.Sprintf("%s: %d of %d, %d offline", v, ro.Current, ro.Total, ro.Offline)
	}
	return false, ""
}

// relHistory is one line of the tab's history.
type relHistory struct {
	Version  string     `json:"version"`
	Commit   string     `json:"commit"`
	TaggedAt *time.Time `json:"tagged_at,omitempty"`
	NotesURL string     `json:"notes_url,omitempty"`
	Todo     string     `json:"todo"`
}

var ghRunRE = regexp.MustCompile(`^(https://github\.com/[^/]+/[^/]+)/actions/runs/\d+`)

// notesURL is where a release's notes are: its GitHub release page when the
// recorded workflow run is a GitHub one, else the run itself.
func notesURL(rl *TodoRelease) string {
	if m := ghRunRE.FindStringSubmatch(rl.RunURL); m != nil {
		return m[1] + "/releases/tag/" + rl.Version
	}
	return rl.RunURL
}

func taggedAt(it Todo) time.Time {
	if it.Release != nil && it.Release.TaggedAt != nil {
		return *it.Release.TaggedAt
	}
	return it.CreatedAt
}

// releaseHistory lists the newest tagged releases, newest first, leaving out
// the one the tab shows as current.
func (s *Server) releaseHistory(items []Todo, skip string) []relHistory {
	var tagged []Todo
	for _, it := range items {
		if it.Release != nil && it.Release.State == relTagged && it.ID != skip {
			tagged = append(tagged, it)
		}
	}
	sort.SliceStable(tagged, func(i, j int) bool { return taggedAt(tagged[i]).After(taggedAt(tagged[j])) })
	if len(tagged) > releaseHistoryMax {
		tagged = tagged[:releaseHistoryMax]
	}
	out := make([]relHistory, 0, len(tagged))
	for _, it := range tagged {
		h := relHistory{Version: it.Release.Version, Commit: it.Release.Commit, TaggedAt: it.Release.TaggedAt, NotesURL: notesURL(it.Release), Todo: it.ID}
		out = append(out, h)
	}
	return out
}

func (s *Server) releaseItems() []Todo {
	var out []Todo
	for _, it := range s.todos.list("all") {
		if it.Release != nil {
			out = append(out, it)
		}
	}
	return out
}

// pickCurrent chooses the release the tab shows.
func (s *Server) pickCurrent(items []Todo, hosts []relHost, now time.Time) (Todo, bool) {
	var open, tagged *Todo
	for i := range items {
		it := &items[i]
		switch {
		case it.Release.State == relTagged:
			if tagged == nil || taggedAt(*it).After(taggedAt(*tagged)) {
				tagged = it
			}
		case it.active():
			if open == nil || it.CreatedAt.After(open.CreatedAt) {
				open = it
			}
		}
	}
	var cand *Todo
	if tagged != nil {
		cp := *tagged
		markRollout(hosts, cp.Release.Version)
		ro := rolloutOf(hosts)
		if !ro.Complete || now.Sub(cp.UpdatedAt) < releaseDoneShown {
			cand = tagged
		}
	}
	// A newer proposal replaces the last release in the tab.
	if open != nil && (cand == nil || open.CreatedAt.After(cand.CreatedAt)) {
		cand = open
	}
	if cand == nil {
		return Todo{}, false
	}
	return *cand, true
}

// handleReleases: GET /api/releases[?refresh=1]. The Releases tab: the current
// release in the shape of GET /api/release/{id} (or none), the fleet's
// versions, the history, and whether the user is needed. It also closes any
// release item whose rollout has finished.
func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	if !s.relReadGuard(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	hosts := s.relHosts(ctx)
	s.sweepReleasesWith(hosts)
	items := s.releaseItems()
	cur, ok := s.pickCurrent(items, hosts, time.Now())
	var out map[string]any
	if ok {
		// pickCurrent marked the hosts for the newest tagged release; the view
		// needs them marked for its own version.
		if cur.Release.State == relTagged {
			markRollout(hosts, cur.Release.Version)
		}
		out = s.releaseViewOf(ctx, r, cur, r.URL.Query().Get("refresh") == "1", hostsIf(cur, hosts))
		out["current"] = true
	} else {
		cfg := s.cfg.Release
		out = map[string]any{
			"current": false, "csrf": s.csrfToken(r), "can_tag": cfg.Tool != nil, "simulated": cfg.Simulated,
			"release_host": s.releaseHost(), "dry_run": cfg.DryRun || s.remoteDry(), "hosts": hosts,
			"needs_you": false, "needs_text": "",
		}
	}
	out["history"] = s.releaseHistory(items, cur.ID)
	writeJSON(w, out)
}

// hostsIf passes the fetched hosts on only for a tagged release, which is the
// one whose view lists them.
func hostsIf(it Todo, hosts []relHost) []relHost {
	if it.Release.State == relTagged {
		return hosts
	}
	return nil
}

// sweepReleasesWith closes every open tagged release item whose version the
// whole fleet runs. The hosts must be a fresh relHosts answer.
func (s *Server) sweepReleasesWith(hosts []relHost) {
	if s.todos == nil || len(hosts) == 0 {
		return
	}
	for _, it := range s.todos.list("") {
		if it.Release == nil || it.Release.State != relTagged {
			continue
		}
		cp := append([]relHost(nil), hosts...)
		markRollout(cp, it.Release.Version)
		if !rolloutOf(cp).Complete {
			continue
		}
		closed, err := s.todos.mutate(it.ID, func(t *Todo) error {
			if !t.active() {
				return errTodoClosed
			}
			now := time.Now().UTC()
			t.State, t.ClosedBy, t.ClosedAt, t.Notice = TodoDone, "comebye", &now, nil
			return nil
		})
		if err != nil {
			continue
		}
		s.auditBy(ledger.AuditEvent{By: "comebye", Todo: closed.ID, Action: "release.complete", OK: true,
			Detail: fmt.Sprintf("%s runs on all %d hosts: the item was closed", it.Release.Version, len(hosts))})
	}
}

// sweepReleases is sweepReleasesWith for a caller with no hosts at hand. It
// asks the fleet only when a tagged release item is open.
func (s *Server) sweepReleases(ctx context.Context) {
	if s.todos == nil || s.fleet == nil {
		return
	}
	any := false
	for _, it := range s.todos.list("") {
		if it.Release != nil && it.Release.State == relTagged {
			any = true
			break
		}
	}
	if !any {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s.sweepReleasesWith(s.relHosts(ctx))
}

// RunReleaseSweep closes finished release items on a clock, so a rollout that
// ends while nobody has the Releases tab open still closes its item.
func (s *Server) RunReleaseSweep(ctx context.Context) {
	if s.todos == nil || s.fleet == nil {
		return
	}
	t := time.NewTicker(releaseSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if e := recover(); e != nil {
						log.Printf("release sweep: %v", e)
					}
				}()
				s.sweepReleases(ctx)
			}()
		}
	}
}

func relS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
