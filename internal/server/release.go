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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	RunURL     string     `json:"run_url,omitempty"`
	Error      string     `json:"error,omitempty"`
	// DryRun is the last dry run's output (a server run with
	// -release-dry-run never tags).
	DryRun string `json:"dry_run,omitempty"`
}

// ReleaseConfig turns the app's release and upgrade buttons on.
type ReleaseConfig struct {
	// Tool tags releases from this host's checkout; nil: this host cannot
	// (the hub's item then says so).
	Tool release.Tool
	// Upgrade lets the app upgrade this host's binary.
	Upgrade bool
	// DryRun makes every action do its checks and change nothing.
	DryRun bool
	// Bin is the installed binary `comebye upgrade` replaces.
	Bin string
	// Runner starts an upgrade (default: a transient systemd user unit,
	// which survives the restart of this service).
	Runner UpgradeRunner
}

// UpgradeRunner starts an upgrade run that outlives this server.
type UpgradeRunner interface {
	Start(ctx context.Context, rec upgrade.Record, path string, rollback, dry bool) error
}

func (c ReleaseConfig) enabled() bool { return c.Tool != nil || c.Upgrade }

// relState is the server's release bookkeeping.
type relState struct {
	mu     sync.Mutex // one tagging at a time
	upMu   sync.Mutex // one upgrade start at a time
	cmu    sync.Mutex
	checks map[string]relCached
	key    []byte
}

type relCached struct {
	at    time.Time
	check release.Check
	build release.Build
}

func newRelState() *relState {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &relState{checks: map[string]relCached{}, key: k}
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
	if b.Question == "" {
		b.Question = fmt.Sprintf("Release %s (%s)?", v, c[:7])
	}
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
}

const (
	relCheckTTL = 20 * time.Second
	relBuildTTL = 15 * time.Second
)

// cached runs fn unless a fresh answer for the key exists (or force).
func (s *Server) relCheck(ctx context.Context, it Todo, force bool) (release.Check, error) {
	k := "check:" + it.ID
	s.rel.cmu.Lock()
	c, ok := s.rel.checks[k]
	s.rel.cmu.Unlock()
	if ok && !force && time.Since(c.at) < relCheckTTL {
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

func (s *Server) relBuild(ctx context.Context, it Todo, force bool) (release.Build, error) {
	k := "build:" + it.ID
	s.rel.cmu.Lock()
	c, ok := s.rel.checks[k]
	s.rel.cmu.Unlock()
	if ok && !force && time.Since(c.at) < relBuildTTL {
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
// and the CSRF token.
func (s *Server) handleReleaseView(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) || s.todoOff(w, r) {
		return
	}
	it, ok := s.releaseItem(w, r.PathValue("id"))
	if !ok {
		return
	}
	cfg := s.cfg.Release
	force := r.URL.Query().Get("refresh") == "1"
	out := map[string]any{
		"todo": s.todoGone(it), "csrf": s.csrfToken(r),
		"can_tag": cfg.Tool != nil, "dry_run": cfg.DryRun,
		"audit": s.ledger.AuditLog(it.ID),
	}
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	if cfg.Tool != nil && it.active() {
		if it.Release.State != relTagged {
			ck, err := s.relCheck(ctx, it, force)
			if err != nil {
				out["check_error"] = err.Error()
			} else {
				out["check"] = ck
			}
		} else {
			bd, err := s.relBuild(ctx, it, force)
			if err != nil {
				out["build_error"] = err.Error()
			} else {
				out["build"] = bd
			}
		}
	}
	if it.Release.State == relTagged {
		out["hosts"] = s.relHosts(ctx)
	}
	writeJSON(w, out)
}

// relHosts lists every host with its version and what the app's last
// upgrade there did. A peer is asked (as the hub); one that cannot answer is
// marked unknown, and one too old to know the route is not enabled.
func (s *Server) relHosts(ctx context.Context) []relHost {
	hosts := s.fleet.Hosts()
	out := make([]relHost, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		out[i] = relHost{Name: h.Name, Local: h.Local, Status: h.Status, Version: h.Version}
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
		relErr(w, http.StatusConflict, "release_disabled", "this host does not tag releases: start comebye with -release-repo <checkout> (or run tools/release/tag by hand)")
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
	ck, err := s.relCheck(ctx, it, true)
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
	if _, err := s.todos.mutate(it.ID, func(t *Todo) error {
		if !t.active() || t.Release == nil || (t.Release.State != relPending && t.Release.State != relFailed) {
			return errTodoClosed
		}
		rel := *t.Release
		rel.State, rel.ApprovedBy, rel.ApprovedAt, rel.Error = relTagging, by, &now, ""
		t.Release = &rel
		return nil
	}); err != nil {
		todoErr(w, err)
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "release.approve", Todo: it.ID, OK: true, Detail: fmt.Sprintf("%s at %s (dry run %v)", it.Release.Version, it.Release.Commit, cfg.DryRun)})
	unlock = false
	go s.tagRelease(it, by)
	cur, _ := s.todos.get(it.ID)
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"todo": s.todoGone(cur)})
}

// tagRelease pushes the tag (holding relState.mu, released here) and records
// the outcome on the item.
func (s *Server) tagRelease(it Todo, by string) {
	defer s.rel.mu.Unlock()
	cfg := s.cfg.Release
	res, err := cfg.Tool.Tag(context.Background(), it.Release.Version, it.Release.Commit, cfg.DryRun)
	done := time.Now().UTC()
	ev := ledger.AuditEvent{By: by, Todo: it.ID}
	switch {
	case err != nil:
		ev.Action, ev.Detail = "release.failed", it.Release.Version+": "+oneLine(err.Error(), 300)
	case cfg.DryRun:
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
		case cfg.DryRun:
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

// handleUpgradeLocal: POST /api/release/upgrade {version, rollback?}. This
// host's own side: validate and start. The hub's handler (below) calls it
// on a peer on the user's behalf. User role only; an agent never reaches it.
func (s *Server) handleUpgradeLocal(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) {
		return
	}
	cfg := s.cfg.Release
	if !cfg.Upgrade {
		relErr(w, http.StatusConflict, "upgrade_disabled", "this host does not let the app upgrade it: start comebye with -release-upgrade")
		return
	}
	var b upgradeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !release.VersionRE.MatchString(b.Version) {
		relErr(w, http.StatusBadRequest, "bad_version", "an exact version vMAJOR.MINOR.PATCH is required (not latest)")
		return
	}
	path := s.upgradeRecordPath()
	if path == "" {
		relErr(w, http.StatusConflict, "upgrade_disabled", "this host has no state directory")
		return
	}
	s.rel.upMu.Lock()
	defer s.rel.upMu.Unlock()
	if rec, _ := upgrade.ReadRecord(path); rec.Live(time.Now()) {
		relErr(w, http.StatusConflict, "upgrade_running", "an upgrade is running on this host already")
		return
	}
	var idb [6]byte
	_, _ = rand.Read(idb[:])
	rec := upgrade.Record{ID: hex.EncodeToString(idb[:]), Version: b.Version, Rollback: b.Rollback, State: upgrade.RecRunning,
		By: s.by(r, ""), Started: time.Now().UTC(), From: s.cfg.Version}
	if err := upgrade.WriteRecord(path, rec); err != nil {
		relErr(w, http.StatusInternalServerError, "upgrade_error", err.Error())
		return
	}
	runner := cfg.Runner
	if runner == nil {
		runner = &systemdRunner{Bin: cfg.Bin, URL: "http://" + s.cfg.Addr}
	}
	if err := runner.Start(context.Background(), rec, path, b.Rollback, cfg.DryRun); err != nil {
		rec.State, rec.Error, rec.Finished = upgrade.RecFailed, "could not start: "+err.Error(), time.Now().UTC()
		_ = upgrade.WriteRecord(path, rec)
		s.auditBy(ledger.AuditEvent{By: rec.By, Action: "upgrade.start", Detail: b.Version + ": " + rec.Error})
		relErr(w, http.StatusInternalServerError, "upgrade_error", rec.Error)
		return
	}
	s.auditBy(ledger.AuditEvent{By: rec.By, Action: "upgrade.start", OK: true, Detail: fmt.Sprintf("%s (rollback %v, dry run %v) run %s", b.Version, b.Rollback, cfg.DryRun, rec.ID)})
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"record": rec})
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
	if s.cfg.Release.Tool != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		if bd, err := s.relBuild(ctx, it, true); err != nil || bd.State != "success" {
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
		r2 := r.Clone(r.Context())
		r2.Body = http.NoBody
		rr := &bufResponse{h: http.Header{}}
		r2.Body = nopCloser{strings.NewReader(string(inner))}
		s.handleUpgradeLocal(rr, r2)
		s.auditBy(ledger.AuditEvent{By: by, Action: "upgrade.request", Todo: it.ID, Host: host, OK: rr.code == 0 || rr.code == http.StatusAccepted, Detail: fmt.Sprintf("%s rollback=%v -> %d", b.Version, b.Rollback, rr.status())})
		rr.copyTo(w)
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
	bin := s.Bin
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		bin = exe
	}
	args := []string{"upgrade", "--bin", bin, "--record", path}
	switch {
	case rollback:
		args = append(args, "--rollback")
	default:
		args = append(args, "--version", rec.Version)
	}
	if dry {
		args = append(args, "--dry-run")
	}
	if dry && !rollback {
		// Nothing is installed or restarted: no need for a unit that outlives us.
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "COMEBYE_URL="+s.URL)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait() }()
		return nil
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

// ---- small plumbing for calling a handler in-process ----

type bufResponse struct {
	h    http.Header
	code int
	buf  strings.Builder
}

func (b *bufResponse) Header() http.Header         { return b.h }
func (b *bufResponse) WriteHeader(c int)           { b.code = c }
func (b *bufResponse) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *bufResponse) status() int {
	if b.code == 0 {
		return http.StatusOK
	}
	return b.code
}
func (b *bufResponse) copyTo(w http.ResponseWriter) {
	for k, v := range b.h {
		w.Header()[k] = v
	}
	w.WriteHeader(b.status())
	_, _ = w.Write([]byte(b.buf.String()))
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

// SimulatedUpgrade is an UpgradeRunner that runs nothing and writes a
// finished record at once (-release-simulate: demos and browser tests).
type SimulatedUpgrade struct{}

func (SimulatedUpgrade) Start(_ context.Context, rec upgrade.Record, path string, rollback, _ bool) error {
	rec.State, rec.Finished, rec.To = upgrade.RecDryRun, time.Now().UTC(), rec.Version
	rec.Output = "simulated: nothing was installed"
	return upgrade.WriteRecord(path, rec)
}
