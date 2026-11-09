package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/release"
)

// Deploying a release: a second, separate click.
//
// Approve release tags and nothing else; it never deploys. Once the Release
// workflow has passed, the item offers a second button that runs the
// operator's own command (-release-deploy-cmd) on the host that owns the
// release clone, with the released version as its last argument and in
// COMEBYE_RELEASE_VERSION. What the command does is the operator's business
// (comebye knows nothing about it): for example, committing the version to a
// GitOps repository and waiting for a rollout. Unset, the button is absent
// and nothing here runs.
//
// The command may restart this very hub (that is the point of one example),
// so it never runs inside the hub's request: the host that owns it starts it
// in the background, keeps the verdict in <state-dir>/release/deploy.json,
// and the page reads that record whenever it asks, before and after a hub
// restart. Only the user can start it (s.auth + CSRF, a body naming the
// version and commit the page showed), only for a tagged item whose build
// passed (checked again on the owning host), one at a time, and it is
// audited. The command line is operator-configured: nothing a request sends
// reaches it except the version, which is vMAJOR.MINOR.PATCH.

// deployTimeout bounds one run of the command.
const deployTimeout = 10 * time.Minute

// Deploy record states.
const (
	depRunning = "running"
	depOK      = "ok"
	depFailed  = "failed"
	depDryRun  = "dry_run"
)

// DeployRecord is the last run of the deploy command on a host.
type DeployRecord struct {
	Version  string     `json:"version"`
	State    string     `json:"state"`
	By       string     `json:"by,omitempty"`
	Todo     string     `json:"todo,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	Output   string     `json:"output,omitempty"` // the tail of the command's output
	Error    string     `json:"error,omitempty"`
}

type deployStatus struct {
	Enabled bool          `json:"enabled"`
	DryRun  bool          `json:"dry_run,omitempty"`
	Label   string        `json:"label,omitempty"`
	Command string        `json:"command,omitempty"` // the program only, never its arguments
	Record  *DeployRecord `json:"record,omitempty"`
}

type deployReq struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Todo    string `json:"todo,omitempty"`
	By      string `json:"by,omitempty"`
}

func (s *Server) deployRecordPath() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.StateDir, "release", "deploy.json")
}

func (s *Server) writeDeployRecord(rec DeployRecord) {
	p := s.deployRecordPath()
	if p == "" {
		return
	}
	data, _ := json.Marshal(rec)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		log.Printf("release: deploy record: %v", err)
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		err = os.Rename(tmp, p)
		if err != nil {
			log.Printf("release: deploy record: %v", err)
		}
	}
}

// deployStatusLocal is this host's own deploy command and its last run.
func (s *Server) deployStatusLocal() deployStatus {
	cfg := s.cfg.Release
	st := deployStatus{Enabled: len(cfg.DeployCmd) > 0, DryRun: cfg.DryRun, Label: cfg.DeployLabel}
	if !st.Enabled {
		return st
	}
	st.Command = filepath.Base(cfg.DeployCmd[0])
	if p := s.deployRecordPath(); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			var rec DeployRecord
			if json.Unmarshal(data, &rec) == nil && rec.Version != "" {
				if rec.State == depRunning && !s.rel.deploying.Load() {
					now := time.Now().UTC()
					rec.State, rec.Finished, rec.Error = depFailed, &now, "the deploy stopped without a verdict (this server restarted while it ran)"
				}
				st.Record = &rec
			}
		}
	}
	return st
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - 2*t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	b := t.buf.Bytes()
	if len(b) > t.max {
		b = b[len(b)-t.max:]
	}
	return string(b)
}

// startDeployLocal starts the deploy command for version in the background.
// It returns the record, or an HTTP status and {code, message}.
func (s *Server) startDeployLocal(by, todo, version string) (DeployRecord, int, [2]string) {
	cfg := s.cfg.Release
	if len(cfg.DeployCmd) == 0 {
		return DeployRecord{}, http.StatusConflict, [2]string{"deploy_disabled", "this host has no deploy command: start comebye with -release-deploy-cmd"}
	}
	if !release.VersionRE.MatchString(version) {
		return DeployRecord{}, http.StatusBadRequest, [2]string{"bad_release", "the version is vMAJOR.MINOR.PATCH"}
	}
	if !s.rel.deploying.CompareAndSwap(false, true) {
		return DeployRecord{}, http.StatusConflict, [2]string{"deploy_busy", "a deploy is already running: wait for it to finish"}
	}
	argv := append(append([]string{}, cfg.DeployCmd...), version)
	rec := DeployRecord{Version: version, State: depRunning, By: oneLine(by, 120), Todo: oneLine(todo, 40), Started: time.Now().UTC()}
	if cfg.DryRun {
		now := time.Now().UTC()
		rec.State, rec.Finished = depDryRun, &now
		rec.Output = "dry run: would run " + filepath.Base(argv[0]) + " " + strings.Join(argv[1:], " ")
		s.writeDeployRecord(rec)
		s.rel.deploying.Store(false)
		log.Printf("release: deploy %s (dry run): nothing run", version)
		return rec, http.StatusAccepted, [2]string{}
	}
	s.writeDeployRecord(rec)
	log.Printf("release: deploy %s started by %s: %s", version, rec.By, filepath.Base(argv[0]))
	go s.runDeploy(rec, argv)
	return rec, http.StatusAccepted, [2]string{}
}

func (s *Server) runDeploy(rec DeployRecord, argv []string) {
	defer s.rel.deploying.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out := &tailBuffer{max: 4000}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), "COMEBYE_RELEASE_VERSION="+rec.Version, "COMEBYE_RELEASE_TODO="+rec.Todo, "COMEBYE_RELEASE_BY="+rec.By)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	now := time.Now().UTC()
	rec.Finished, rec.Output = &now, out.String()
	switch {
	case err == nil:
		rec.State = depOK
	case ctx.Err() != nil:
		rec.State, rec.Error = depFailed, fmt.Sprintf("timed out after %s", deployTimeout)
	default:
		rec.State, rec.Error = depFailed, oneLine(err.Error(), 300)
	}
	s.writeDeployRecord(rec)
	log.Printf("release: deploy %s finished: %s", rec.Version, rec.State)
}

// ---- the peer's side (the hub's call) ----

// handlePeerDeploy: POST /api/release/deploy {version, todo?, by?}. Starts
// the deploy command, after checking for itself that the version's Release
// build passed: a forged call cannot deploy a version that was never built.
func (s *Server) handlePeerDeploy(w http.ResponseWriter, r *http.Request) {
	if !s.peerRelGuard(w, r) {
		return
	}
	var b deployReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	b.Version = strings.TrimSpace(b.Version)
	if !release.VersionRE.MatchString(b.Version) {
		relErr(w, http.StatusBadRequest, "bad_release", "the version is vMAJOR.MINOR.PATCH")
		return
	}
	if len(s.cfg.Release.DeployCmd) == 0 {
		relErr(w, http.StatusConflict, "deploy_disabled", "this host has no deploy command: start comebye with -release-deploy-cmd")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	if bd, err := s.cfg.Release.Tool.Build(ctx, b.Version); err != nil || bd.State != "success" {
		why := "the release build is " + bd.State
		if err != nil {
			why = oneLine(err.Error(), 300)
		}
		s.audit(r, ledger.AuditEvent{Action: "deploy.refused", Todo: b.Todo, Detail: b.Version + ": " + why})
		relErr(w, http.StatusConflict, "build_not_green", why)
		return
	}
	by := fmt.Sprintf("%s (hub says: by %s)", s.by(r, ""), oneLine(b.By, 80))
	rec, code, msg := s.startDeployLocal(by, b.Todo, b.Version)
	s.audit(r, ledger.AuditEvent{Action: "deploy.request", Todo: b.Todo, OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s -> %d", b.Version, code)})
	if code != http.StatusAccepted {
		relErr(w, code, msg[0], msg[1])
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"record": rec})
}

// handlePeerDeployStatus: GET /api/release/deploy.
func (s *Server) handlePeerDeployStatus(w http.ResponseWriter, r *http.Request) {
	if !s.peerRelGuard(w, r) {
		return
	}
	writeJSON(w, s.deployStatusLocal())
}

// ---- the hub's side (the user's click) ----

// deployStatusFor is the deploy status the page shows: this host's, or the
// release host's.
func (s *Server) deployStatusFor(ctx context.Context) (deployStatus, error) {
	host := s.cfg.Release.Host
	if host == "" {
		return s.deployStatusLocal(), nil
	}
	code, body, err := s.fleet.CallHost(ctx, host, "release-deploy-status", nil, nil)
	if err != nil {
		return deployStatus{}, err
	}
	var st deployStatus
	if code != http.StatusOK || json.Unmarshal(body, &st) != nil {
		return deployStatus{}, nil // too old to know the route: not enabled there
	}
	// a peer's text is shown, never trusted: bound it
	st.Label, st.Command = oneLine(st.Label, 40), oneLine(st.Command, 80)
	if st.Record != nil {
		st.Record.Output, st.Record.Error = block(st.Record.Output, 4000), oneLine(st.Record.Error, 300)
		st.Record.By, st.Record.Todo = oneLine(st.Record.By, 160), oneLine(st.Record.Todo, 40)
		switch st.Record.State {
		case depRunning, depOK, depFailed, depDryRun:
		default:
			st.Record.State = depFailed
		}
	}
	return st, nil
}

// settleDeployAudit records, once, how a deploy ended (the run outlives the
// hub that asked for it, so the end is only seen when the page asks).
func (s *Server) settleDeployAudit(st deployStatus) {
	rec := st.Record
	if s.ledger == nil || rec == nil || rec.State == depRunning || rec.Todo == "" {
		return
	}
	key := rec.Version + "@" + rec.Started.UTC().Format(time.RFC3339Nano)
	s.rel.cmu.Lock()
	done := s.rel.deployAudited[key]
	s.rel.deployAudited[key] = true
	s.rel.cmu.Unlock()
	if done {
		return
	}
	detail := rec.Version + ": " + rec.State
	if rec.Error != "" {
		detail += ": " + rec.Error
	}
	s.auditBy(ledger.AuditEvent{By: "comebye", Todo: rec.Todo, Host: s.releaseHost(), Action: "deploy.result", OK: rec.State == depOK || rec.State == depDryRun, Detail: oneLine(detail, 400)})
}

type deployBody struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// handleReleaseDeploy: POST /api/release/{id}/deploy {version, commit}.
func (s *Server) handleReleaseDeploy(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) || s.todoOff(w, r) {
		return
	}
	cfg := s.cfg.Release
	if cfg.Tool == nil {
		relErr(w, http.StatusConflict, "release_disabled", "this hub cannot check the release build: start comebye with -release-repo or -release-host")
		return
	}
	var b deployBody
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
	if it.Release.State != relTagged {
		relErr(w, http.StatusConflict, "not_released", "this release has not been tagged")
		return
	}
	if b.Version != it.Release.Version || strings.ToLower(b.Commit) != it.Release.Commit {
		relErr(w, http.StatusConflict, "confirm_mismatch", "the version and commit you confirmed are not this item's: reload the page")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if bd, err := s.relBuild(ctx, it, 0); err != nil || bd.State != "success" {
		why := "the release build is " + bd.State
		if err != nil {
			why = err.Error()
		}
		s.audit(r, ledger.AuditEvent{Action: "deploy.refused", Todo: it.ID, Detail: b.Version + ": " + why})
		relErr(w, http.StatusConflict, "build_not_green", why)
		return
	}
	by := s.by(r, "")
	if cfg.Host == "" {
		rec, code, msg := s.startDeployLocal(by, it.ID, b.Version)
		s.auditBy(ledger.AuditEvent{By: by, Action: "deploy.request", Todo: it.ID, Host: s.releaseHost(), OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s -> %d", b.Version, code)})
		if code != http.StatusAccepted {
			relErr(w, code, msg[0], msg[1])
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"record": rec})
		return
	}
	inner, _ := json.Marshal(deployReq{Version: b.Version, Todo: it.ID, By: by})
	code, body, err := s.fleet.CallHost(ctx, cfg.Host, "release-deploy", url.Values{}, inner)
	if err != nil {
		s.auditBy(ledger.AuditEvent{By: by, Action: "deploy.request", Todo: it.ID, Host: cfg.Host, Detail: b.Version + ": " + oneLine(err.Error(), 200)})
		relErr(w, http.StatusBadGateway, "peer_error", "could not reach "+cfg.Host+": "+oneLine(err.Error(), 200))
		return
	}
	s.auditBy(ledger.AuditEvent{By: by, Action: "deploy.request", Todo: it.ID, Host: cfg.Host, OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s -> %d", b.Version, code)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}
