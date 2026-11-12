package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/release"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// Releasing and upgrading from the app.
//
// A coordinator raises a kind "release" to-do carrying a version and the
// full commit id. The user, and only the user, approves it in the app; the
// server then runs tools/release/tag (the same script a person runs by hand,
// embedded in the binary) as the server's own user, with that user's git
// credentials. Each host is then upgraded by a second click that runs
// `comebye upgrade --version` on that host.
//
// Who can do what (see docs/security.md):
//   - Every route here is s.auth: the user role only. The agent token, a peer
//     forwarding for its agents and a tagged node all get 403 agent_forbidden
//     before a handler runs, and each handler checks callerOf(r).agent()
//     again.
//   - Doing something also needs the CSRF header (below) and a body naming the
//     version and commit (or the host) that the page showed.
//   - Creating the item is the only thing an agent does, and only a recorded
//     coordinator may; the version and commit are then fixed. Nothing an
//     agent writes (context, question, links) is acted on.
//   - Every action goes to the ledger's audit log and the journal.

// Release item states.
const (
	relPending = "pending" // proposed; waiting for the user
	relTagging = "tagging" // approved; the tag is being pushed
	relTagged  = "tagged"  // the tag is pushed and the Release workflow started
	relFailed  = "failed"  // the script refused or failed; the user may try again
)

// HeaderCSRF carries the page's CSRF token on the routes that act.
const HeaderCSRF = "X-Comebye-Csrf"

// TodoRelease is the structured part of a release item. Version and Commit
// are set when the item is created and never change.
type TodoRelease struct {
	Version    string     `json:"version"`
	Commit     string     `json:"commit"`
	State      string     `json:"state"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	TaggedAt   *time.Time `json:"tagged_at,omitempty"`
	// Host is the host whose git and credentials did (or are doing) the
	// tagging: this one, or the -release-host peer.
	Host   string `json:"host,omitempty"`
	RunURL string `json:"run_url,omitempty"`
	Error  string `json:"error,omitempty"`
	// Dismissed: the user abandoned this tagged release's rollout. It is out of
	// the Releases tab, the badge and the auto-close sweep (the tag stays and
	// the release stays in the history).
	Dismissed   bool       `json:"dismissed,omitempty"`
	DismissedBy string     `json:"dismissed_by,omitempty"`
	DismissedAt *time.Time `json:"dismissed_at,omitempty"`
	// DryRun is the last dry run's output (a server run with
	// -release-dry-run never tags).
	DryRun string `json:"dry_run,omitempty"`
}

// ReleaseConfig turns the app's release and upgrade buttons on.
type ReleaseConfig struct {
	// Tool tags releases from this host's checkout; nil: this host cannot
	// (the hub's item then says so).
	Tool release.Tool
	// Host, on a hub, is the peer that owns the release clone: the hub then
	// asks it to check, tag and report the build instead of running the
	// script itself. Exclusive with Tool (New disables both if both are set).
	Host string
	// Upgrade lets the app upgrade this host's binary.
	Upgrade bool
	// DryRun makes every action do its checks and change nothing.
	DryRun bool
	// Simulated: nothing here is real (-release-simulate); the page says so.
	Simulated bool
	// Bin is the installed binary `comebye upgrade` replaces.
	Bin string
	// Runner starts an upgrade (default: a transient systemd user unit on
	// Linux, a one-shot LaunchAgent on macOS; either survives the restart of
	// this service).
	Runner UpgradeRunner
	// DeployCmd is the operator's own command (an absolute program path and
	// its arguments) that the item's second button runs on this host, with
	// the released version appended as the last argument and in
	// COMEBYE_RELEASE_VERSION. It belongs on the host that owns the release
	// clone (Tool); empty: no deploy button. See release_deploy.go.
	DeployCmd []string
	// DeployLabel names that button (default "Deploy").
	DeployLabel string
}

// UpgradeRunner starts an upgrade run that outlives this server.
type UpgradeRunner interface {
	Start(ctx context.Context, rec upgrade.Record, path string, rollback, dry bool) error
}

func (c ReleaseConfig) enabled() bool { return c.Tool != nil || c.Upgrade || c.Host != "" }

// relState is the server's release bookkeeping.
type relState struct {
	mu     sync.Mutex // one tagging at a time
	upMu   sync.Mutex // one upgrade start at a time
	cmu    sync.Mutex
	checks map[string]relCached
	// tagging holds the items whose tag is being pushed by a goroutine of
	// this process: one in state tagging that is not here was interrupted.
	tagging map[string]bool
	key     []byte
	// peerTagging: this host, as a release peer, is running a tagging.
	peerTagging atomic.Bool
	// deploying: the deploy command is running on this host.
	deploying atomic.Bool
	// deployAudited: finished deploys already written to the audit log
	// (guarded by cmu).
	deployAudited map[string]bool
}

type relCached struct {
	at    time.Time
	check release.Check
	build release.Build
}

func newRelState() *relState {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &relState{checks: map[string]relCached{}, tagging: map[string]bool{}, deployAudited: map[string]bool{}, key: k}
}

// csrf is an HMAC of the caller's login under a key made at start-up. A page
// gets it from GET /api/release/{id} (same-origin; another site cannot read
// the answer) and sends it back in X-Comebye-Csrf. It stands beside
// http.CrossOriginProtection and the JSON-only rule: three separate reasons
// for a forged cross-site request to fail.
func (s *Server) csrfToken(r *http.Request) string {
	m := hmac.New(sha256.New, s.rel.key)
	m.Write([]byte(strings.ToLower(strings.TrimSpace(r.Header.Get("Tailscale-User-Login")))))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) csrfOK(r *http.Request) bool {
	got := r.Header.Get(HeaderCSRF)
	return got != "" && hmac.Equal([]byte(got), []byte(s.csrfToken(r)))
}

// relGuard is the common start of the routes that act: user role only (the
// route's own s.auth, repeated), the CSRF header, JSON.
func (s *Server) relGuard(w http.ResponseWriter, r *http.Request, needCSRF bool) bool {
	if callerOf(r).agent() {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "agent_forbidden", "message": "agents cannot release or upgrade"})
		return false
	}
	if needCSRF && !s.csrfOK(r) {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "csrf", "message": "this page's token is missing or stale: reload the page"})
		return false
	}
	return true
}

func relErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
}

// audit records an action in the ledger and the journal. A peer has no
// ledger, so only the journal.
func (s *Server) audit(r *http.Request, e ledger.AuditEvent) {
	e.By = s.by(r, "")
	s.auditBy(e)
}

func (s *Server) auditBy(e ledger.AuditEvent) {
	e.At = time.Now().UTC()
	if e.Host == "" {
		e.Host = s.fleet.LocalName()
	}
	log.Printf("audit: %s by %s on %s (todo %s): ok=%v %s", e.Action, e.By, e.Host, e.Todo, e.OK, e.Detail)
	if s.ledger != nil {
		if err := s.ledger.AddAudit(e); err != nil {
			log.Printf("audit: ledger: %v", err)
		}
	}
}

// ---- creating the item ----

// releaseFromAdd handles the release part of POST /api/todos: who may
// raise one, and what it has to hold. It writes the refusal itself.
func (s *Server) releaseFromAdd(w http.ResponseWriter, r *http.Request, b *todoAddBody) (*TodoRelease, bool) {
	switch {
	case b.Kind != "release" && b.Release == nil:
		return nil, true
	case b.Kind != "release":
		relErr(w, http.StatusBadRequest, "bad_release", "release data goes with kind release")
		return nil, false
	case b.Release == nil:
		relErr(w, http.StatusBadRequest, "bad_release", "a release item needs release.version and release.commit")
		return nil, false
	}
	// Only the user or a recorded coordinator proposes a release; a tagged
	// worker or any other agent may not.
	if c := callerOf(r); c.agent() && (c.tagged || !s.callerIsCoordinator(c)) {
		code := "coordinator_only"
		if c.tagged {
			code = "worker_forbidden"
		}
		relErr(w, http.StatusForbidden, code, "only the user or a coordinator can propose a release")
		return nil, false
	}
	v, c := strings.TrimSpace(b.Release.Version), strings.ToLower(strings.TrimSpace(b.Release.Commit))
	if !release.VersionRE.MatchString(v) {
		relErr(w, http.StatusBadRequest, "bad_release", "the version is vMAJOR.MINOR.PATCH")
		return nil, false
	}
	if !release.CommitRE.MatchString(c) {
		relErr(w, http.StatusBadRequest, "bad_release", "the commit is the full 40-character id (git rev-parse)")
		return nil, false
	}
	// The title is the server's, not an agent's wording.
	b.Question = fmt.Sprintf("Release %s (%s)?", v, c[:7])
	for _, it := range s.todos.list("") {
		if it.Release != nil && it.Release.Version == v {
			relErr(w, http.StatusConflict, "release_exists", "there is an open item for "+v+" already: "+it.ID)
			return nil, false
		}
	}
	return &TodoRelease{Version: v, Commit: c, State: relPending}, true
}

// ---- the view ----

type relHost struct {
	Name    string          `json:"name"`
	Local   bool            `json:"local"`
	Status  string          `json:"status"`
	Version string          `json:"version,omitempty"`
	Enabled bool            `json:"enabled"`           // the host lets the app upgrade it
	Unknown bool            `json:"unknown,omitempty"` // the host could not be asked
	DryRun  bool            `json:"dry_run,omitempty"`
	Record  *upgrade.Record `json:"record,omitempty"`
	// Hub marks the local host of a hub. Offline: the host is not answering
	// (down, still connecting, or it did not answer the status call). Current:
	// it runs the release's version or a newer one. Since is when an offline
	// host went quiet. All three are set by markRollout.
	Hub     bool       `json:"hub,omitempty"`
	Offline bool       `json:"offline,omitempty"`
	Current bool       `json:"current"`
	Since   *time.Time `json:"since,omitempty"`
}

// How old an answer from git or GitHub the page may be shown. The refresh
// button asks for a newer one, but not more often than relForceGap; approving
// and upgrading always ask afresh (maxAge 0).
const (
	relCheckTTL = 20 * time.Second
	relBuildTTL = 15 * time.Second
	relForceGap = 5 * time.Second
)

// cached runs fn unless a fresh answer for the key exists (or force).
func (s *Server) relCheck(ctx context.Context, it Todo, maxAge time.Duration) (release.Check, error) {
	k := "check:" + it.ID
	s.rel.cmu.Lock()
	c, ok := s.rel.checks[k]
	s.rel.cmu.Unlock()
	if ok && time.Since(c.at) < maxAge {
		return c.check, nil
	}
	ck, err := s.cfg.Release.Tool.Check(ctx, it.Release.Version, it.Release.Commit)
	if err != nil {
		return ck, err
	}
	s.rel.cmu.Lock()
	s.rel.checks[k] = relCached{at: time.Now(), check: ck}
	s.rel.cmu.Unlock()
	return ck, nil
}

func (s *Server) relBuild(ctx context.Context, it Todo, maxAge time.Duration) (release.Build, error) {
	k := "build:" + it.ID
	s.rel.cmu.Lock()
	c, ok := s.rel.checks[k]
	s.rel.cmu.Unlock()
	if ok && time.Since(c.at) < maxAge {
		return c.build, nil
	}
	b, err := s.cfg.Release.Tool.Build(ctx, it.Release.Version)
	if err != nil {
		return b, err
	}
	s.rel.cmu.Lock()
	s.rel.checks[k] = relCached{at: time.Now(), build: b}
	s.rel.cmu.Unlock()
	return b, nil
}

func (s *Server) releaseItem(w http.ResponseWriter, id string) (Todo, bool) {
	it, ok := s.todos.get(id)
	if !ok || it.Release == nil {
		todoErr(w, errTodoNotFound)
		return it, false
	}
	return it, true
}

// handleReleaseView: GET /api/release/{id}. What the page shows: the live
// check of the commit (CI from GitHub, the changes since the last tag), the
// release build once tagged, every host with its version, the audit trail
// and the CSRF token. The Releases tab reads GET /api/releases instead, which
// returns the same view for the current release.
func (s *Server) handleReleaseView(w http.ResponseWriter, r *http.Request) {
	if !s.relReadGuard(w, r) {
		return
	}
	it, ok := s.releaseItem(w, r.PathValue("id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	writeJSON(w, s.releaseViewOf(ctx, r, it, r.URL.Query().Get("refresh") == "1", nil))
}

// relReadGuard is relGuard for the two GETs that run git and gh: a GET is not
// covered by the cross-origin rule, so a page that is not this one cannot have
// it done.
func (s *Server) relReadGuard(w http.ResponseWriter, r *http.Request) bool {
	if !s.relGuard(w, r, false) || s.todoOff(w, r) {
		return false
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
		relErr(w, http.StatusForbidden, "cross_site", "not from another site")
		return false
	}
	return true
}

// releaseViewOf builds what the page shows for one release item. hosts, when
// not nil, is a list the caller already fetched (and marked).
func (s *Server) releaseViewOf(ctx context.Context, r *http.Request, it Todo, refresh bool, hosts []relHost) map[string]any {
	cfg := s.cfg.Release
	checkAge, buildAge := relCheckTTL, relBuildTTL
	if refresh {
		checkAge, buildAge = relForceGap, relForceGap
	}
	if cfg.Tool != nil && it.active() && it.Release.State == relFailed {
		// A push that went through without being acknowledged left the item
		// failed: if the tag is on origin at this commit, record that.
		if ck, err := s.relCheck(ctx, it, checkAge); err == nil && s.reconcileTagged(it, ck) {
			it, _ = s.todos.get(it.ID)
		}
	}
	out := map[string]any{
		"todo": s.todoGone(it), "csrf": s.csrfToken(r),
		"can_tag": cfg.Tool != nil, "simulated": cfg.Simulated,
		"release_host": s.releaseHost(),
		"audit":        s.ledger.AuditLog(it.ID),
	}
	var ck *release.Check
	var bd *release.Build
	if cfg.Tool != nil {
		switch {
		case it.Release.State == relTagging:
			// While the tag is being pushed the page shows the stored state: a
			// check now would fetch in the clone the push is using.
		case it.Release.State == relTagged:
			// A tagged release is shown while it rolls out, whether or not
			// its to-do item is still open.
			b, err := s.relBuild(ctx, it, buildAge)
			if err != nil {
				out["build_error"] = err.Error()
			} else {
				out["build"], bd = b, &b
			}
		case it.active():
			c, err := s.relCheck(ctx, it, checkAge)
			if err != nil {
				out["check_error"] = err.Error()
			} else {
				out["check"], ck = c, &c
			}
		}
	}
	out["dry_run"] = cfg.DryRun || s.remoteDry() // after the check: the release host reports its mode with it
	var dep *deployStatus
	if hosts == nil {
		hosts = s.relHosts(ctx)
		if it.Release.State == relTagged {
			markRollout(hosts, it.Release.Version)
		}
	}
	// The fleet is listed whatever the release's state; only a tagged release
	// has a rollout to track.
	out["hosts"] = hosts
	if it.Release.State == relTagged {
		if u := notesURL(it.Release); u != "" {
			out["notes_url"] = u
		}
		ro := rolloutOf(hosts)
		out["rollout"] = ro
		if cfg.Tool != nil {
			// The deploy command lives on the host that owns the clone.
			dctx, dcancel := context.WithTimeout(ctx, 8*time.Second)
			if st, err := s.deployStatusFor(dctx); err != nil {
				out["deploy_error"] = oneLine(err.Error(), 200)
			} else if st.Enabled {
				out["deploy"] = st
				dep = &st
				s.settleDeployAudit(st)
			}
			dcancel()
		}
		out["needs_you"], out["needs_text"] = s.relNeeds(it, ck, bd, hosts, ro, dep)
	} else {
		out["needs_you"], out["needs_text"] = s.relNeeds(it, ck, bd, nil, relRollout{}, nil)
	}
	return out
}

// relHosts lists every host with its version and what the app's last
// upgrade there did. A peer is asked (as the hub); one that cannot answer is
// marked unknown, and one too old to know the route is not enabled.
func (s *Server) relHosts(ctx context.Context) []relHost {
	hosts := s.fleet.Hosts()
	out := make([]relHost, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		out[i] = relHost{Name: h.Name, Local: h.Local, Status: h.Status, Version: h.Version, Since: h.Since, Hub: h.Local && s.fleet.IsHub()}
		wg.Add(1)
		go func(i int, h string, local bool) {
			defer wg.Done()
			var st upgradeStatus
			if local {
				st = s.upgradeStatus()
			} else {
				cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
				defer cancel()
				code, body, err := s.fleet.CallHost(cctx, h, "release-status", nil, nil)
				switch {
				case err != nil:
					out[i].Unknown = true
					return
				case code != http.StatusOK || json.Unmarshal(body, &st) != nil:
					return // too old, or not enabled there
				}
			}
			out[i].Enabled, out[i].DryRun, out[i].Record = st.Enabled, st.DryRun, st.Record
			if st.Version != "" {
				out[i].Version = st.Version
			}
			s.settleUpgradeAudit(h, st.Record)
		}(i, h.Name, h.Local)
	}
	wg.Wait()
	return out
}

// ---- approving ----

type approveBody struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// handleReleaseApprove: POST /api/release/{id}/approve {version, commit}.
// The body must repeat what the item holds, the one the page named in its
// confirm step; the preconditions are checked again right now.
func (s *Server) handleReleaseApprove(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) || s.todoOff(w, r) {
		return
	}
	cfg := s.cfg.Release
	if cfg.Tool == nil {
		relErr(w, http.StatusConflict, "release_disabled", "this host does not tag releases: start comebye with -release-repo <checkout> or -release-host <peer> (or run tools/release/tag by hand)")
		return
	}
	var b approveBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	it, ok := s.releaseItem(w, r.PathValue("id"))
	if !ok {
		return
	}
	if !it.active() {
		todoErr(w, errTodoClosed)
		return
	}
	if b.Version != it.Release.Version || strings.ToLower(b.Commit) != it.Release.Commit {
		relErr(w, http.StatusConflict, "confirm_mismatch", "the version and commit you confirmed are not this item's: reload the page")
		return
	}
	if st := it.Release.State; st != relPending && st != relFailed {
		relErr(w, http.StatusConflict, "release_state", "this release is already "+st)
		return
	}
	if !s.rel.mu.TryLock() {
		relErr(w, http.StatusConflict, "release_busy", "a release is being tagged: wait for it to finish")
		return
	}
	unlock := true
	defer func() {
		if unlock {
			s.rel.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	ck, err := s.relCheck(ctx, it, 0)
	if err != nil || !ck.OK {
		why := ck.Problem
		if err != nil {
			why = err.Error()
		}
		s.audit(r, ledger.AuditEvent{Action: "release.refused", Todo: it.ID, Detail: it.Release.Version + " " + it.Release.Commit + ": " + why})
		relErr(w, http.StatusConflict, "precondition_failed", why)
		return
	}
	now := time.Now().UTC()
	by := s.by(r, "")
	// In flight first, so a drop cannot slip in between the state change and
	// the goroutine; cleared if the change fails.
	s.setTagging(it.ID, true)
	if _, err := s.todos.mutate(it.ID, func(t *Todo) error {
		if !t.active() || t.Release == nil || (t.Release.State != relPending && t.Release.State != relFailed) {
			return errTodoClosed
		}
		rel := *t.Release
		rel.State, rel.ApprovedBy, rel.ApprovedAt, rel.Error, rel.Host = relTagging, by, &now, "", s.releaseHost()
		t.Release = &rel
		return nil
	}); err != nil {
		s.setTagging(it.ID, false)
		todoErr(w, err)
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "release.approve", Todo: it.ID, Host: s.releaseHost(), OK: true, Detail: fmt.Sprintf("%s at %s (dry run %v)", it.Release.Version, it.Release.Commit, cfg.DryRun)})
	unlock = false
	go s.tagRelease(it, by)
	cur, _ := s.todos.get(it.ID)
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"todo": s.todoGone(cur)})
}

// tagRelease pushes the tag (holding relState.mu, released here) and records
// the outcome on the item.
func (s *Server) tagRelease(it Todo, by string) {
	defer s.rel.mu.Unlock()
	defer s.setTagging(it.ID, false)
	cfg := s.cfg.Release
	res, err := cfg.Tool.Tag(withRelCall(context.Background(), relCall{By: by, Todo: it.ID}), it.Release.Version, it.Release.Commit, cfg.DryRun)
	done := time.Now().UTC()
	ev := ledger.AuditEvent{By: by, Todo: it.ID, Host: s.releaseHost()}
	switch {
	case err != nil:
		ev.Action, ev.Detail = "release.failed", it.Release.Version+": "+oneLine(err.Error(), 300)
	case res.DryRun:
		ev.Action, ev.OK, ev.Detail = "release.dry_run", true, it.Release.Version+": nothing tagged"
	default:
		ev.Action, ev.OK, ev.Detail = "release.tagged", true, it.Release.Version+" at "+it.Release.Commit+" "+res.RunURL
	}
	s.auditBy(ev)
	_, merr := s.todos.mutate(it.ID, func(t *Todo) error {
		rel := *t.Release
		switch {
		case err != nil:
			rel.State, rel.Error = relFailed, oneLine(err.Error(), 600)
		case res.DryRun:
			rel.State, rel.Error, rel.DryRun = relPending, "", block(res.Output, 4000)
		default:
			rel.State, rel.Error, rel.TaggedAt, rel.RunURL = relTagged, "", &done, res.RunURL
		}
		t.Release = &rel
		return nil
	})
	if merr != nil {
		log.Printf("release: recording the outcome of %s: %v", it.Release.Version, merr)
	}
	s.rel.cmu.Lock()
	delete(s.rel.checks, "check:"+it.ID)
	delete(s.rel.checks, "build:"+it.ID)
	s.rel.cmu.Unlock()
}

// ---- upgrading hosts ----

type upgradeStatus struct {
	Enabled bool            `json:"enabled"`
	DryRun  bool            `json:"dry_run,omitempty"`
	Version string          `json:"version,omitempty"`
	Record  *upgrade.Record `json:"record,omitempty"`
}

func (s *Server) upgradeRecordPath() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.StateDir, "release", "upgrade.json")
}

func (s *Server) upgradeStatus() upgradeStatus {
	st := upgradeStatus{Enabled: s.cfg.Release.Upgrade, DryRun: s.cfg.Release.DryRun, Version: s.cfg.Version}
	if p := s.upgradeRecordPath(); p != "" {
		if rec, err := upgrade.ReadRecord(p); err == nil && rec.ID != "" {
			if rec.State == upgrade.RecRunning && !rec.Live(time.Now()) {
				rec.State, rec.Error = upgrade.RecFailed, "the upgrade stopped without a verdict"
			}
			st.Record = &rec
		}
	}
	return st
}

// handleUpgradeStatus: GET /api/release/upgrade, this host's side: whether
// the app may upgrade it and what the last run did. The hub asks each peer.
func (s *Server) handleUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) {
		return
	}
	writeJSON(w, s.upgradeStatus())
}

type upgradeBody struct {
	Version  string `json:"version"`
	Rollback bool   `json:"rollback,omitempty"`
	ID       string `json:"id,omitempty"`
	By       string `json:"by,omitempty"` // unused from outside: the server names the caller
}

// handleUpgradeLocal: POST /api/release/upgrade {version, rollback?}. The
// peer side of an upgrade, for the hub only: it needs the hub-call marker
// (fleet.HeaderHubCall, set by the hub's own calls and by no browser path)
// and is refused on a host that keeps the ledger, since a hub starts its own
// upgrade through handleHostUpgrade, which needs the page's CSRF token and a
// tagged release whose build passed. User role only, so an agent never
// reaches it either way.
func (s *Server) handleUpgradeLocal(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) {
		return
	}
	if s.ledger != nil || r.Header.Get(fleet.HeaderHubCall) != "1" {
		relErr(w, http.StatusForbidden, "hub_only", "this route is for a hub upgrading its peer; use the release page")
		return
	}
	var b upgradeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	rec, code, msg := s.startUpgrade(s.by(r, ""), b.Version, b.Rollback)
	if code != http.StatusAccepted {
		relErr(w, code, msg[0], msg[1])
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"record": rec})
}

// startUpgrade validates and starts an upgrade of this host: the record is
// written as running, then the runner starts the run. On refusal it returns
// the status and {code, message}.
func (s *Server) startUpgrade(by, version string, rollback bool) (upgrade.Record, int, [2]string) {
	cfg := s.cfg.Release
	if !cfg.Upgrade {
		return upgrade.Record{}, http.StatusConflict, [2]string{"upgrade_disabled", "this host does not let the app upgrade it: start comebye with -release-upgrade"}
	}
	if !release.VersionRE.MatchString(version) {
		return upgrade.Record{}, http.StatusBadRequest, [2]string{"bad_version", "an exact version vMAJOR.MINOR.PATCH is required (not latest)"}
	}
	path := s.upgradeRecordPath()
	if path == "" {
		return upgrade.Record{}, http.StatusConflict, [2]string{"upgrade_disabled", "this host has no state directory"}
	}
	s.rel.upMu.Lock()
	defer s.rel.upMu.Unlock()
	if rec, _ := upgrade.ReadRecord(path); rec.Live(time.Now()) {
		return upgrade.Record{}, http.StatusConflict, [2]string{"upgrade_running", "an upgrade is running on this host already"}
	}
	var idb [6]byte
	_, _ = rand.Read(idb[:])
	rec := upgrade.Record{ID: hex.EncodeToString(idb[:]), Version: version, Rollback: rollback, State: upgrade.RecRunning,
		By: by, Started: time.Now().UTC(), From: s.cfg.Version}
	if err := upgrade.WriteRecord(path, rec); err != nil {
		return rec, http.StatusInternalServerError, [2]string{"upgrade_error", err.Error()}
	}
	runner := cfg.Runner
	if runner == nil {
		runner = defaultUpgradeRunner(cfg.Bin, "http://"+s.cfg.Addr)
	}
	if err := runner.Start(context.Background(), rec, path, rollback, cfg.DryRun); err != nil {
		rec.State, rec.Error, rec.Finished = upgrade.RecFailed, "could not start: "+err.Error(), time.Now().UTC()
		_ = upgrade.WriteRecord(path, rec)
		s.auditBy(ledger.AuditEvent{By: by, Action: "upgrade.start", Detail: version + ": " + rec.Error})
		return rec, http.StatusInternalServerError, [2]string{"upgrade_error", rec.Error}
	}
	s.auditBy(ledger.AuditEvent{By: by, Action: "upgrade.start", OK: true, Detail: fmt.Sprintf("%s (rollback %v, dry run %v) run %s", version, rollback, cfg.DryRun, rec.ID)})
	return rec, http.StatusAccepted, [2]string{}
}

type hostUpgradeBody struct {
	Todo     string `json:"todo"`
	Version  string `json:"version"`
	Rollback bool   `json:"rollback,omitempty"`
}

// handleHostUpgrade: POST /api/hosts/{host}/release/upgrade {todo, version,
// rollback?}. The page's button. On the hub: the item must be a tagged
// release whose build is green and whose version the body names, the host
// must be known; then this host starts the run, or the peer is asked to.
func (s *Server) handleHostUpgrade(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) || s.todoOff(w, r) {
		return
	}
	host := r.PathValue("host")
	var b hostUpgradeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	it, ok := s.releaseItem(w, b.Todo)
	if !ok {
		return
	}
	if it.Release.State != relTagged {
		relErr(w, http.StatusConflict, "not_released", "this release has not been tagged")
		return
	}
	if b.Version != it.Release.Version {
		relErr(w, http.StatusConflict, "confirm_mismatch", "the version you confirmed is not this item's: reload the page")
		return
	}
	local := host == s.fleet.LocalName()
	if !local && !s.fleet.HasPeer(host) {
		relErr(w, http.StatusNotFound, "unknown_host", "no such host")
		return
	}
	// The build gate needs gh and the checkout, so a hub upgrades hosts only
	// with -release-repo set.
	if s.cfg.Release.Tool == nil {
		relErr(w, http.StatusConflict, "release_disabled", "this hub cannot check the release build: start comebye with -release-repo <checkout>")
		return
	}
	{
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		if bd, err := s.relBuild(ctx, it, 0); err != nil || bd.State != "success" {
			why := "the release build is " + bd.State
			if err != nil {
				why = err.Error()
			}
			s.audit(r, ledger.AuditEvent{Action: "upgrade.refused", Todo: it.ID, Host: host, Detail: b.Version + ": " + why})
			relErr(w, http.StatusConflict, "build_not_green", why)
			return
		}
	}
	by := s.by(r, "")
	inner, _ := json.Marshal(upgradeBody{Version: b.Version, Rollback: b.Rollback})
	if local {
		rec, code, msg := s.startUpgrade(by, b.Version, b.Rollback)
		s.auditBy(ledger.AuditEvent{By: by, Action: "upgrade.request", Todo: it.ID, Host: host, OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s rollback=%v -> %d", b.Version, b.Rollback, code)})
		if code != http.StatusAccepted {
			relErr(w, code, msg[0], msg[1])
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"record": rec})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	code, body, err := s.fleet.CallHost(ctx, host, "release-upgrade", url.Values{}, inner)
	if err != nil {
		s.auditBy(ledger.AuditEvent{By: by, Action: "upgrade.request", Todo: it.ID, Host: host, Detail: b.Version + ": " + oneLine(err.Error(), 200)})
		relErr(w, http.StatusBadGateway, "peer_error", "could not reach "+host+": "+oneLine(err.Error(), 200))
		return
	}
	s.auditBy(ledger.AuditEvent{By: by, Action: "upgrade.request", Todo: it.ID, Host: host, OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s rollback=%v -> %d", b.Version, b.Rollback, code)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// settleUpgradeAudit records, once, how an upgrade the hub started ended
// (the run outlives the server that started it, so the end is only seen
// later, when the page asks).
func (s *Server) settleUpgradeAudit(host string, rec *upgrade.Record) {
	if s.ledger == nil || rec == nil || rec.ID == "" || rec.State == upgrade.RecRunning {
		return
	}
	for _, e := range s.ledger.AuditLog("") {
		if e.Action == "upgrade.result" && e.Host == host && strings.Contains(e.Detail, "run "+rec.ID) {
			return
		}
	}
	s.auditBy(ledger.AuditEvent{By: rec.By, Action: "upgrade.result", Host: host, OK: rec.State == upgrade.RecOK || rec.State == upgrade.RecDryRun,
		Detail: fmt.Sprintf("%s: %s -> %s %s run %s %s", rec.Version, rec.From, rec.To, rec.State, rec.ID, oneLine(rec.Error, 200))})
}

// ---- starting an upgrade that survives the restart ----

// systemdRunner starts `comebye upgrade` as a transient user unit, so the
// restart of comebye.service that the upgrade performs does not kill it
// (a child of this server would be, with systemd's control-group kill).
type systemdRunner struct {
	Bin, URL string
}

func (s *systemdRunner) Start(ctx context.Context, rec upgrade.Record, path string, rollback, dry bool) error {
	bin, err := upgradeBinary(s.Bin)
	if err != nil {
		return err
	}
	args := upgradeArgs(bin, path, rec.Version, rollback, dry)
	if dry && !rollback {
		// Nothing is installed or restarted: no need for a unit that outlives us.
		return startDetachedDry(bin, s.URL, args)
	}
	sr, err := exec.LookPath("systemd-run")
	if err != nil {
		return errors.New("systemd-run is needed to upgrade from the app (run `comebye upgrade` on the host)")
	}
	full := append([]string{"--user", "--collect", "--quiet", "--unit", "comebye-upgrade-" + rec.ID, "--setenv=COMEBYE_URL=" + s.URL, "--", bin}, args...)
	out, err := exec.CommandContext(ctx, sr, full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %w: %s", err, oneLine(string(out), 200))
	}
	return nil
}

// SimulatedUpgrade is an UpgradeRunner that runs nothing and writes a
// finished record at once (-release-simulate: demos and browser tests).
type SimulatedUpgrade struct{}

func (SimulatedUpgrade) Start(_ context.Context, rec upgrade.Record, path string, rollback, _ bool) error {
	rec.State, rec.Finished, rec.To = upgrade.RecDryRun, time.Now().UTC(), rec.Version
	rec.Output = "simulated: nothing was installed"
	return upgrade.WriteRecord(path, rec)
}

func (s *Server) setTagging(id string, on bool) {
	s.rel.cmu.Lock()
	defer s.rel.cmu.Unlock()
	if on {
		s.rel.tagging[id] = true
	} else {
		delete(s.rel.tagging, id)
	}
}

// tagInFlight: a goroutine of this process is pushing the item's tag.
func (s *Server) tagInFlight(id string) bool {
	s.rel.cmu.Lock()
	defer s.rel.cmu.Unlock()
	return s.rel.tagging[id]
}

// recoverTagging runs at start-up: an item left in state tagging by a
// restart (a deploy, a crash) is moved to failed, saying the tag may or may
// not have been pushed. The user can then try again (the script refuses if
// the tag is there) or drop it.
func (s *Server) recoverTagging() {
	if s.todos == nil {
		return
	}
	for _, it := range s.todos.list("") {
		if it.Release == nil || it.Release.State != relTagging {
			continue
		}
		_, err := s.todos.mutate(it.ID, func(t *Todo) error {
			rel := *t.Release
			rel.State, rel.Error = relFailed, "interrupted by a restart: check whether the tag "+rel.Version+" exists on origin before trying again"
			t.Release = &rel
			return nil
		})
		if err != nil {
			log.Printf("release: recovering %s: %v", it.ID, err)
			continue
		}
		s.auditBy(ledger.AuditEvent{By: "comebye", Todo: it.ID, Action: "release.interrupted", Detail: it.Release.Version + ": the server restarted while tagging"})
	}
}

// releaseHost is the host that tags: the -release-host peer, else this one.
func (s *Server) releaseHost() string {
	if h := s.cfg.Release.Host; h != "" {
		return h
	}
	return s.fleet.LocalName()
}

// remoteDry: the -release-host peer last said it is in dry-run mode.
func (s *Server) remoteDry() bool {
	rt, ok := s.cfg.Release.Tool.(*remoteTool)
	return ok && rt.dry.Load()
}

// reconcileTagged moves a failed item to tagged when origin has its tag at
// exactly its commit. That is the case of a push that went through while the
// answer was lost (the peer finished after the hub gave up, or the hub
// restarted): the script now refuses the retry, so without this the item
// stays failed and no host can be upgraded from it. Only an item the user
// approved is touched, never one with a push in flight, and the commit must
// match the item's; the move is audited.
func (s *Server) reconcileTagged(it Todo, ck release.Check) bool {
	rel := it.Release
	if rel == nil || rel.State != relFailed || rel.ApprovedBy == "" || ck.TagOnOrigin == "" || ck.TagOnOrigin != rel.Commit || s.tagInFlight(it.ID) {
		return false
	}
	now := time.Now().UTC()
	moved := false
	if _, err := s.todos.mutate(it.ID, func(t *Todo) error {
		if t.Release == nil || t.Release.State != relFailed || t.Release.ApprovedBy == "" || !t.active() {
			return nil
		}
		r := *t.Release
		r.State, r.Error, r.TaggedAt = relTagged, "", &now
		t.Release, moved = &r, true
		return nil
	}); err != nil || !moved {
		return false
	}
	s.auditBy(ledger.AuditEvent{By: "comebye", Todo: it.ID, Host: s.releaseHost(), Action: "release.reconciled", OK: true,
		Detail: rel.Version + ": the tag is on origin at " + rel.Commit + "; the earlier try ended without an answer"})
	s.rel.cmu.Lock()
	delete(s.rel.checks, "check:"+it.ID)
	delete(s.rel.checks, "build:"+it.ID)
	s.rel.cmu.Unlock()
	return true
}

// releasePending names the hosts (the hub among them) that do not run a
// tagged release item's version yet, so closing the item as done cannot hide
// the Bump hub and upgrade buttons while a rollout is outstanding. An offline
// host counts: the release is not done until it has upgraded. The hub closes
// the item itself once none is left (sweepReleases); this is the refusal for a
// person who tries sooner.
func (s *Server) releasePending(ctx context.Context, it Todo) []string {
	if it.Release == nil || it.Release.State != relTagged || s.fleet == nil {
		return nil
	}
	if _, ok := parseSemver(it.Release.Version); !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hosts := s.relHosts(ctx)
	markRollout(hosts, it.Release.Version)
	return s.pendingLines(hosts)
}

// pendingLines says, for each marked host that is not current, what holds it.
func (s *Server) pendingLines(hosts []relHost) []string {
	var out []string
	for _, h := range hosts {
		if h.Current {
			continue
		}
		name := h.Name
		if h.Hub {
			name = "the hub (" + h.Name + ")"
		}
		switch {
		case h.Offline && h.Version != "":
			out = append(out, name+" is offline (last seen on "+h.Version+")")
		case h.Offline:
			out = append(out, name+" is offline")
		case h.Version == "":
			out = append(out, name+" reports no version")
		default:
			out = append(out, name+" is on "+h.Version)
		}
	}
	return out
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	if !release.VersionRE.MatchString(v) {
		return out, false
	}
	_, err := fmt.Sscanf(v, "v%d.%d.%d", &out[0], &out[1], &out[2])
	return out, err == nil
}

func semverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
