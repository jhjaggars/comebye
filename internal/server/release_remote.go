package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/release"
)

// Tagging on a peer.
//
// With -release-host NAME the hub does not run the release script itself: it
// asks the peer NAME, which owns the dedicated clone and holds the git and gh
// credentials (-release-repo there), to check a commit, to tag and push it,
// and to report the tag's Release workflow run. Everything the user decides
// stays on the hub (the page, the CSRF token, the confirm body, the item's
// state machine, the recovery of a stuck tagging, the hub's audit log); the
// peer only does the git and GitHub work, with the same script, the same
// version and commit validation and its own audit lines in its journal.
//
// The peer's side is three routes that answer the hub only (peerRelGuard):
//
//	POST /api/release/check   {version, commit}
//	POST /api/release/tag     {version, commit, dry_run?, todo?, by?}
//	GET  /api/release/build?version=
//
// A browser, an agent token, a tagged node and a hub's own browser forward
// are all refused (see peerRelGuard and docs/security.md).

// ---- the peer's side ----

type peerRelReq struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	// DryRun asks for a dry run; a peer started with -release-dry-run does one
	// whatever the hub asks, so the hub can only make the peer more careful.
	DryRun bool `json:"dry_run,omitempty"`
	// Todo and By are what the hub says this is for, only for the peer's
	// journal line: nothing is decided from them.
	Todo string `json:"todo,omitempty"`
	By   string `json:"by,omitempty"`
}

type peerCheckResp struct {
	Check  release.Check `json:"check"`
	DryRun bool          `json:"dry_run,omitempty"`
	Error  string        `json:"error,omitempty"` // the check could not run; Check holds what was known
}

type peerTagResp struct {
	Result release.TagResult `json:"result"`
	DryRun bool              `json:"dry_run"`
}

// peerRelGuard is the start of the peer routes: the user role only (the
// route's own s.auth, repeated; an agent token, a forwarded peer agent and a
// tagged node never get this far), the hub's own call (fleet.HeaderHubCall,
// which no browser forward carries) and a host that keeps no ledger, i.e. a
// peer: a hub or a standalone instance never runs another host's tagging for
// a caller that merely adds the header. The marker is a guard, not
// authentication: whoever can present the hub device owner's login can add
// it. What bounds that is in the script (CI-green commit of main, newer than
// the last tag, origin pinned) and in docs/security.md.
func (s *Server) peerRelGuard(w http.ResponseWriter, r *http.Request) bool {
	if !s.relGuard(w, r, false) {
		return false
	}
	if s.ledger != nil || r.Header.Get(fleet.HeaderHubCall) != "1" {
		relErr(w, http.StatusForbidden, "hub_only", "this route is for a hub tagging through its peer; use the release page")
		return false
	}
	if s.cfg.Release.Tool == nil {
		relErr(w, http.StatusConflict, "release_disabled", "this host does not tag releases: start comebye with -release-repo <dedicated clone>")
		return false
	}
	return true
}

func (s *Server) peerRelBody(w http.ResponseWriter, r *http.Request) (peerRelReq, bool) {
	var b peerRelReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return b, false
	}
	b.Commit = strings.ToLower(strings.TrimSpace(b.Commit))
	b.Version = strings.TrimSpace(b.Version)
	if !release.VersionRE.MatchString(b.Version) {
		relErr(w, http.StatusBadRequest, "bad_release", "the version is vMAJOR.MINOR.PATCH")
		return b, false
	}
	if !release.CommitRE.MatchString(b.Commit) {
		relErr(w, http.StatusBadRequest, "bad_release", "the commit is the full 40-character id")
		return b, false
	}
	return b, true
}

// handlePeerCheck: POST /api/release/check. Read-only (git fetch and gh run
// list), so it only goes to the journal when it cannot run.
func (s *Server) handlePeerCheck(w http.ResponseWriter, r *http.Request) {
	if !s.peerRelGuard(w, r) {
		return
	}
	b, ok := s.peerRelBody(w, r)
	if !ok {
		return
	}
	// A check runs git fetch in the clone a tagging is using: not meanwhile.
	if s.rel.peerTagging.Load() {
		relErr(w, http.StatusConflict, "release_busy", "a release is being tagged here: wait for it to finish")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 95*time.Second)
	defer cancel()
	ck, err := s.cfg.Release.Tool.Check(ctx, b.Version, b.Commit)
	out := peerCheckResp{Check: ck, DryRun: s.cfg.Release.DryRun}
	if err != nil {
		out.Error = oneLine(err.Error(), 600)
		s.audit(r, ledger.AuditEvent{Action: "release.check_failed", Detail: b.Version + " " + b.Commit + ": " + oneLine(err.Error(), 300)})
	}
	writeJSON(w, out)
}

// handlePeerTag: POST /api/release/tag. Tags and pushes, under this host's
// credentials, one at a time. The push is not tied to the hub's connection: a
// hub that gives up waiting does not leave a half-finished push behind; the
// next try then finds the tag on origin (or none) and says so.
func (s *Server) handlePeerTag(w http.ResponseWriter, r *http.Request) {
	if !s.peerRelGuard(w, r) {
		return
	}
	b, ok := s.peerRelBody(w, r)
	if !ok {
		return
	}
	if !s.rel.mu.TryLock() {
		relErr(w, http.StatusConflict, "release_busy", "a release is being tagged here: wait for it to finish")
		return
	}
	defer s.rel.mu.Unlock()
	s.rel.peerTagging.Store(true)
	defer s.rel.peerTagging.Store(false)
	cfg := s.cfg.Release
	dry := cfg.DryRun || b.DryRun
	claim := fmt.Sprintf("(hub says: todo %s, by %s)", oneLine(b.Todo, 40), oneLine(b.By, 80))
	res, err := cfg.Tool.Tag(context.WithoutCancel(r.Context()), b.Version, b.Commit, dry)
	ev := ledger.AuditEvent{}
	switch {
	case err != nil:
		ev.Action, ev.Detail = "release.failed", b.Version+": "+oneLine(err.Error(), 300)+" "+claim
	case dry:
		ev.Action, ev.OK, ev.Detail = "release.dry_run", true, b.Version+": nothing tagged "+claim
	default:
		ev.Action, ev.OK, ev.Detail = "release.tagged", true, b.Version+" at "+b.Commit+" "+res.RunURL+" "+claim
	}
	s.audit(r, ev)
	if err != nil {
		relErr(w, http.StatusInternalServerError, "tag_failed", oneLine(err.Error(), 600))
		return
	}
	writeJSON(w, peerTagResp{Result: res, DryRun: dry})
}

// handlePeerBuild: GET /api/release/build?version=. The Release workflow run
// of a tag, as gh reports it.
func (s *Server) handlePeerBuild(w http.ResponseWriter, r *http.Request) {
	if !s.peerRelGuard(w, r) {
		return
	}
	v := r.URL.Query().Get("version")
	if !release.VersionRE.MatchString(v) {
		relErr(w, http.StatusBadRequest, "bad_release", "the version is vMAJOR.MINOR.PATCH")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	bd, err := s.cfg.Release.Tool.Build(ctx, v)
	if err != nil {
		relErr(w, http.StatusBadGateway, "build_failed", oneLine(err.Error(), 300))
		return
	}
	writeJSON(w, bd)
}

// ---- the hub's side ----

// remoteTool is the release.Tool of a hub with -release-host: each step is
// one hub-only call to the peer, which runs the script. What comes back is
// checked here (it is a peer's word): the commit must be the one asked
// about, a dry run must not have pushed, and any link must be https.
type remoteTool struct {
	s    *Server
	host string
	// dry is the peer's last word on whether it is in dry-run mode, so the
	// page can say so before the first tagging.
	dry atomic.Bool
}

// relCall carries who and what a tagging is for to the remote tool (for the
// peer's journal line only).
type relCall struct{ By, Todo string }

type relCallKey struct{}

func withRelCall(ctx context.Context, c relCall) context.Context {
	return context.WithValue(ctx, relCallKey{}, c)
}

// peerFault is a refusal or failure the peer itself reported (as opposed to
// a call that never got an answer).
type peerFault struct{ host, code, msg string }

func (e *peerFault) Error() string {
	if e.code == "" {
		return e.host + ": " + e.msg
	}
	return e.host + " refused (" + e.code + "): " + e.msg
}

// call makes one hub-only call and decodes a 200 answer into out. Any other
// status is a *peerFault carrying the peer's own code and message.
func (t *remoteTool) call(ctx context.Context, op string, q url.Values, body any, out any) error {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	code, data, err := t.s.fleet.CallHost(ctx, t.host, op, q, raw)
	if err != nil {
		return err // unreachable, no answer in time, or the peer rejected the hub
	}
	if code != http.StatusOK {
		var e struct{ Code, Message string }
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = fmt.Sprintf("answered %d", code)
		}
		return &peerFault{host: t.host, code: e.Code, msg: oneLine(e.Message, 400)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s sent an answer that is not understood: %w", t.host, err)
	}
	return nil
}

// httpsOnly keeps a link a peer sent only if it is a GitHub https URL (the
// CI and Release runs are there): the page links it, and a peer's text is
// not trusted to be a safe href or to send the user elsewhere.
func httpsOnly(u string) string {
	if strings.HasPrefix(u, "https://github.com/") && !strings.ContainsAny(u, " \t\r\n\"'<>") {
		return u
	}
	return ""
}

func (t *remoteTool) Check(ctx context.Context, version, commit string) (release.Check, error) {
	if !release.VersionRE.MatchString(version) || !release.CommitRE.MatchString(commit) {
		return release.Check{}, errors.New("the version is vMAJOR.MINOR.PATCH and the commit a full 40-character id")
	}
	var resp peerCheckResp
	if err := t.call(ctx, "release-check", nil, peerRelReq{Version: version, Commit: commit}, &resp); err != nil {
		return release.Check{}, err
	}
	t.dry.Store(resp.DryRun)
	ck := resp.Check
	ck.Version, ck.CIURL = version, httpsOnly(ck.CIURL)
	if ck.At.IsZero() {
		ck.At = time.Now().UTC()
	}
	if resp.Error != "" {
		return ck, errors.New(t.host + ": " + resp.Error)
	}
	if ck.Commit != "" && ck.Commit != commit {
		return ck, fmt.Errorf("%s checked %s, not %s", t.host, ck.Commit, commit)
	}
	ck.Commit = commit
	if !ck.OK && ck.Problem == "" {
		ck.Problem = t.host + " gave no verdict"
	}
	return ck, nil
}

func (t *remoteTool) Tag(ctx context.Context, version, commit string, dry bool) (release.TagResult, error) {
	if !release.VersionRE.MatchString(version) || !release.CommitRE.MatchString(commit) {
		return release.TagResult{}, errors.New("the version is vMAJOR.MINOR.PATCH and the commit a full 40-character id")
	}
	rc, _ := ctx.Value(relCallKey{}).(relCall)
	var resp peerTagResp
	err := t.call(ctx, "release-tag", nil, peerRelReq{Version: version, Commit: commit, DryRun: dry, Todo: rc.Todo, By: rc.By}, &resp)
	var pf *peerFault
	switch {
	case errors.As(err, &pf):
		return release.TagResult{}, err // the peer said no, or said it failed: its reason is the answer
	case err != nil:
		// No answer: the request may or may not have got through.
		return release.TagResult{}, fmt.Errorf("%w (if the request got through, %s may have pushed %s: try again, and the script says if the tag is already on origin)", err, t.host, version)
	}
	t.dry.Store(resp.DryRun)
	res := resp.Result
	res.RunURL = httpsOnly(res.RunURL)
	res.DryRun = resp.DryRun
	switch {
	case dry && res.Pushed:
		return res, fmt.Errorf("%s pushed a tag although a dry run was asked for: check %s on origin", t.host, version)
	case !resp.DryRun && !res.Pushed:
		return res, errors.New(t.host + " answered without saying it pushed the tag")
	}
	res.Pushed = res.Pushed && !resp.DryRun
	return res, nil
}

func (t *remoteTool) Build(ctx context.Context, version string) (release.Build, error) {
	if !release.VersionRE.MatchString(version) {
		return release.Build{}, errors.New("bad version")
	}
	var bd release.Build
	if err := t.call(ctx, "release-build", url.Values{"version": {version}}, nil, &bd); err != nil {
		return release.Build{}, err
	}
	switch bd.State {
	case "none", "queued", "in_progress", "success", "failure":
	default:
		bd.State = "none"
	}
	bd.URL = httpsOnly(bd.URL)
	if bd.At.IsZero() {
		bd.At = time.Now().UTC()
	}
	return bd, nil
}
