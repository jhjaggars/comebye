package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// Push-less hosts: a worker VM that keeps no GitHub login (a KubeVirt VM
// made by tools/worker). Its agents commit locally; the provisioner that
// built it holds the push credentials and its ssh config, so the hub has the
// provisioner bring the branch back:
//
//	POST /api/provisioner/host/fetch   {name, repo_name, remote_repo, branch}   hub to provisioner
//
// The provisioner runs `comebye-worker fetch NAME BRANCH --repo DIR --push`
// with DIR its own checkout of the repository (under its launch roots). The
// tool never pushes the default branch and never overwrites a tip that is
// not an ancestor.
//
// On the hub it is off by default, behind the pushless_fetch setting: a list
// of hosts (the push-less flag) and a switch. When a handoff on a listed host
// reports, the hub runs the fetch-and-push, and the hub-check waits for it
// before it reads "pushed". The result is recorded on the entry
// (ledger.PushFetch); a failure is a note there and an event, never a failed
// handoff or landing.

// pushlessFetchSettings is the hub's setting.
type pushlessFetchSettings struct {
	Enabled bool `json:"enabled"`
	// Hosts are the push-less hosts.
	Hosts []string `json:"hosts,omitempty"`
	// Via is the provisioner node to ask; empty: the one that reports the
	// host (refused when two do).
	Via string `json:"via,omitempty"`
}

func normalizePushless(in *pushlessFetchSettings) *pushlessFetchSettings {
	if in == nil {
		return nil
	}
	out := &pushlessFetchSettings{Enabled: in.Enabled, Via: strings.TrimSpace(in.Via)}
	for _, h := range in.Hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && !slices.Contains(out.Hosts, h) {
			out.Hosts = append(out.Hosts, h)
		}
	}
	if !out.Enabled && len(out.Hosts) == 0 && out.Via == "" {
		return nil
	}
	return out
}

// checkPushless says what is wrong with a setting a PUT sends ("" when fine).
func checkPushless(p *pushlessFetchSettings) string {
	if p == nil {
		return ""
	}
	for _, h := range p.Hosts {
		if !provision.ValidName(h) {
			return "pushless_fetch.hosts: " + plainText(h, 40) + " is not a host name"
		}
	}
	if p.Via != "" && !provision.ValidName(p.Via) {
		return "pushless_fetch.via: not a host name"
	}
	if len(p.Hosts) > 50 {
		return "pushless_fetch.hosts: at most 50"
	}
	return ""
}

// pushlessOf is the setting in force for host: nil when the hub does not
// fetch from it.
func (s *Server) pushlessOf(host string) *pushlessFetchSettings {
	p := s.settings.get().PushlessFetch
	if p == nil || !p.Enabled || !slices.Contains(p.Hosts, host) {
		return nil
	}
	return p
}

const (
	evPushFetch = "push-fetch" // the hub brought a push-less host's branch back, or could not

	// pushFetchRetry is how long a failed fetch waits before the hub-check
	// may try it again for the same report.
	pushFetchRetry = 5 * time.Minute
	pushFetchWait  = 5 * time.Minute
)

// pushlessState is the fetches under way, one per entry.
type pushlessState struct {
	mu      sync.Mutex
	running map[string]chan struct{}
}

// pushFetchRunning says whether a fetch for the entry is under way.
func (s *Server) pushFetchRunning(id string) bool {
	s.pushless.mu.Lock()
	defer s.pushless.mu.Unlock()
	_, ok := s.pushless.running[id]
	return ok
}

// pushFetchDue says whether e wants a fetch now.
func (s *Server) pushFetchDue(e ledger.Entry, now time.Time) bool {
	if s.ledger == nil || s.pushlessOf(e.Host) == nil {
		return false
	}
	if e.Status != ledger.Finished && e.Status != ledger.ReadyToRemove {
		return false // running, landing (that pushes the base, never fetched), landed
	}
	if strings.TrimSpace(e.FinalReport) == "" || !provision.ValidBranch(e.Branch) {
		return false
	}
	f := e.PushFetch
	return f == nil || f.ReportSeq != e.ReportSeq || (!f.OK && now.Sub(f.At) > pushFetchRetry)
}

// afterReport starts the fetch of a push-less host's branch, in the
// background, when a report arrives.
func (s *Server) afterReport(e ledger.Entry) {
	if !s.pushFetchDue(e, time.Now()) {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), pushFetchWait)
		defer cancel()
		s.pushFetch(ctx, e)
	}()
}

// pushFetch runs the fetch-and-push for e if it is due, or waits for the
// one under way, and returns the entry as it is then.
func (s *Server) pushFetch(ctx context.Context, e ledger.Entry) ledger.Entry {
	s.pushless.mu.Lock()
	if ch, ok := s.pushless.running[e.ID]; ok {
		s.pushless.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		if cur, ok := s.ledger.Get(e.ID); ok {
			return cur
		}
		return e
	}
	if cur, ok := s.ledger.Get(e.ID); ok {
		e = cur
	}
	if !s.pushFetchDue(e, time.Now()) {
		s.pushless.mu.Unlock()
		return e
	}
	if s.pushless.running == nil {
		s.pushless.running = map[string]chan struct{}{}
	}
	ch := make(chan struct{})
	s.pushless.running[e.ID] = ch
	s.pushless.mu.Unlock()
	defer func() {
		s.pushless.mu.Lock()
		delete(s.pushless.running, e.ID)
		close(ch)
		s.pushless.mu.Unlock()
	}()

	f := s.runPushFetch(ctx, e)
	cur, ok, err := s.ledger.SetPushFetch(e.ID, f)
	if err != nil || !ok {
		return e
	}
	who := fmt.Sprintf("handoff %s (branch %s on %s)", safeText(e.ID), safeText(e.Branch), safeText(e.Host))
	if f.OK {
		s.logEvent(evPushFetch, e.ID, "", fmt.Sprintf("%s/%d", e.ID, e.ReportSeq),
			fmt.Sprintf("%s: %s is push-less, so %s fetched the branch and pushed it to origin (%s).", who, safeText(e.Host), safeText(f.Via), shortSHA(f.Tip)))
	} else {
		s.logEvent(evPushFetch, e.ID, "", "",
			fmt.Sprintf("%s: the hub could not bring the branch back from push-less %s: %s", who, safeText(e.Host), safeText(f.Note)))
	}
	return cur
}

// runPushFetch asks the provisioner. A failure comes back as a note.
func (s *Server) runPushFetch(ctx context.Context, e ledger.Entry) ledger.PushFetch {
	f := ledger.PushFetch{At: time.Now().UTC(), ReportSeq: e.ReportSeq, Branch: e.Branch}
	note := func(msg string) ledger.PushFetch {
		f.Note = oneLine(msg, 300)
		return f
	}
	p := s.pushlessOf(e.Host)
	if p == nil {
		return note("the pushless_fetch setting no longer lists " + e.Host)
	}
	owner, _, amb := s.ownerOf(ctx, e.Host, p.Via)
	switch {
	case amb:
		return note("more than one provisioner reports " + e.Host + ": name the one to ask in pushless_fetch.via")
	case owner == "":
		return note("no provisioner reports " + e.Host + ", so none can fetch from it")
	}
	f.Via = owner
	remote := e.RepoName
	if e.Repo != "" {
		remote = filepath.Base(e.Repo)
	}
	if !provision.ValidRepoName(e.RepoName) || !provision.ValidRepoName(remote) {
		return note("the repository name " + plainText(e.RepoName, 60) + " cannot be fetched")
	}
	code, body, err := s.callBody(ctx, owner, "provision-host-fetch", peerFetchReq{Name: e.Host, RepoName: e.RepoName, RemoteRepo: remote, Branch: e.Branch})
	if err != nil {
		return note("could not ask " + owner + ": " + errMessage(err))
	}
	if code != http.StatusOK {
		return note(owner + ": " + peerMessage(body, code))
	}
	var r provision.FetchResult
	if json.Unmarshal(body, &r) != nil || r.Tip == "" || r.Branch != e.Branch {
		return note(owner + " gave an answer that is not a fetch result")
	}
	f.OK, f.Tip, f.Pushed = true, r.Tip, r.Pushed
	return f
}

// peerFetchReq is the body of POST /api/provisioner/host/fetch.
type peerFetchReq struct {
	Name       string `json:"name"`
	RepoName   string `json:"repo_name"`
	RemoteRepo string `json:"remote_repo"`
	Branch     string `json:"branch"`
}

// handlePeerHostFetch: POST /api/provisioner/host/fetch, on the provisioner.
func (s *Server) handlePeerHostFetch(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	var q peerFetchReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil || !provision.ValidName(q.Name) || !provision.ValidBranch(q.Branch) ||
		!provision.ValidRepoName(q.RepoName) || !provision.ValidRepoName(q.RemoteRepo) {
		relErr(w, http.StatusBadRequest, "bad_request", "give the host, a work branch and the repository's names")
		return
	}
	tool, terr := s.toolOfHost(q.Name)
	if terr != nil {
		relErr(w, http.StatusNotFound, "no_host", "this provisioner did not build "+q.Name+", so it will not fetch from it")
		return
	}
	repo := s.localRepoNamed(q.RepoName)
	if repo == "" {
		relErr(w, http.StatusNotFound, "no_repo", "this provisioner has no checkout of "+q.RepoName+" under its launch roots")
		return
	}
	res, err := tool.Fetch(r.Context(), provision.FetchReq{Name: q.Name, Branch: q.Branch, Repo: repo, RemoteRepo: q.RemoteRepo})
	if err != nil {
		s.audit(r, ledger.AuditEvent{Action: "provision.fetch", Host: q.Name, Detail: q.Branch + ": " + oneLine(provision.Text(err.Error(), 160), 160)})
		relErr(w, http.StatusConflict, "fetch_failed", provision.Text(err.Error(), 300))
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "provision.fetch", OK: true, Host: q.Name, Detail: q.Branch + " " + shortSHA(res.Tip) + " " + res.Pushed})
	writeJSON(w, res)
}

// localRepoNamed is this node's checkout of the repository called name: the
// first launch root holding a git repository of that name ("" when none).
func (s *Server) localRepoNamed(name string) string {
	if !provision.ValidRepoName(name) {
		return ""
	}
	for _, root := range s.launchRoots() {
		p := filepath.Join(root, name)
		if fi, err := os.Stat(filepath.Join(p, ".git")); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return p
		}
		if fi, err := os.Stat(filepath.Join(p, "objects")); err == nil && fi.IsDir() {
			if _, err := os.Stat(filepath.Join(p, "HEAD")); err == nil {
				return p // a bare repository
			}
		}
	}
	return ""
}

// pushFetchItem is the hub-check's line about a fetch-and-push, and whether
// it settles "pushed" for the head the checks saw.
func pushFetchItem(e ledger.Entry, tip string) (state, text string, settles bool) {
	f := e.PushFetch
	if f == nil || f.ReportSeq != e.ReportSeq {
		return "", "", false
	}
	if !f.OK {
		return "warn", "the hub could not bring the branch back from push-less " + e.Host + ": " + f.Note, false
	}
	if tip != "" && !strings.HasPrefix(f.Tip, tip) && !strings.HasPrefix(tip, f.Tip) {
		return "warn", fmt.Sprintf("the hub pushed %s from push-less %s, but the branch's head is now %s", shortSHA(f.Tip), e.Host, shortSHA(tip)), false
	}
	how := "pushed it to origin"
	if f.Pushed == "same" {
		how = "found it already on origin"
	}
	return "ok", fmt.Sprintf("%s is push-less: %s fetched the branch and %s at %s", e.Host, f.Via, how, shortSHA(f.Tip)), true
}
