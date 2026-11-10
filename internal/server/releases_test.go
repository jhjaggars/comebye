package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/release"
)

// releasesView is what GET /api/releases returns, as much as these tests read.
type releasesView struct {
	Current   bool         `json:"current"`
	Todo      Todo         `json:"todo"`
	Hosts     []relHost    `json:"hosts"`
	Rollout   relRollout   `json:"rollout"`
	History   []relHistory `json:"history"`
	NeedsYou  bool         `json:"needs_you"`
	NeedsText string       `json:"needs_text"`
	CSRF      string       `json:"csrf"`
}

func getReleases(t *testing.T, s *Server) releasesView {
	t.Helper()
	w := do(s, "GET", "/api/releases", userHdr, "")
	var v releasesView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("GET /api/releases = %d %s", w.Code, w.Body)
	}
	return v
}

// relHubWithPeer is a hub named desk with one peer, kube, that answers the
// release status call with peerVersion (down: the peer is not running).
func relHubWithPeer(t *testing.T, hubVersion, peerVersion string, down bool) (*Server, *fakeTool) {
	t.Helper()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(fleet.VersionHeader, peerVersion)
		switch {
		case r.URL.Path == "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: agents\ndata: {\"type\":\"agents\",\"version\":%q,\"agents\":[]}\n\n", peerVersion)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == "GET" && r.URL.Path == "/api/release/upgrade":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"enabled":true,"version":%q}`, peerVersion)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(peer.Close)
	store, err := fleet.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fl := fleet.New(nil, "desk", nil, fleet.Options{HubMode: true, Version: hubVersion, Logf: func(string, ...any) {}})
	fl.AttachStore(store, fleet.AddrOptions{AllowHTTPLoopback: true, Domains: []string{".ts.net"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go fl.Run(ctx)
	tool := &fakeTool{check: okCheck(), build: release.Build{State: "success"}}
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken, StateDir: t.TempDir(), Version: hubVersion,
		Release: ReleaseConfig{Tool: tool, Upgrade: true, Runner: &fakeRunner{}}}, nil, nil)
	b, _ := json.Marshal(map[string]any{"name": "kube", "urls": []string{peer.URL}})
	if w := do(s, "POST", "/api/peers", userHdr, string(b)); w.Code != 201 {
		t.Fatalf("add peer = %d %s", w.Code, w.Body)
	}
	waitUntil(t, "peer known", func() bool { return fl.HasPeer("kube") })
	if down {
		peer.Close()
	} else {
		waitUntil(t, "peer up", func() bool {
			for _, h := range fl.Hosts() {
				if h.Name == "kube" && h.Status == fleet.StatusOK && h.Version == peerVersion {
					return true
				}
			}
			return false
		})
	}
	return s, tool
}

func tagOnServer(t *testing.T, s *Server, version, sha string) Todo {
	t.Helper()
	it := todoOf(t, do(s, "POST", "/api/todos", userHdr, relBody(version, sha)).Body.Bytes())
	tok := token(t, s, it.ID)
	if code, body := approve(s, it.ID, tok, version, sha); code != 202 {
		t.Fatalf("approve %s = %d %s", version, code, body)
	}
	got := waitRel(t, s, it.ID, relTagged)
	waitUntil(t, "tagging flag cleared", func() bool { return !s.tagInFlight(it.ID) })
	return got
}

func TestReleasesNothingPending(t *testing.T) {
	s, _, _ := relServer(t)
	v := getReleases(t, s)
	if v.Current || v.NeedsYou || len(v.History) != 0 || v.CSRF == "" {
		t.Fatalf("view = %+v", v)
	}
	if len(v.Hosts) == 0 {
		t.Fatal("the fleet's versions are shown when nothing is pending")
	}
}

func TestReleasesShowsAProposedReleaseAndNeedsYouWhenCIIsDone(t *testing.T) {
	s, tool, _ := relServer(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	v := getReleases(t, s)
	if !v.Current || v.Todo.ID != it.ID || !v.NeedsYou || v.NeedsText != "Approve v0.5.9" {
		t.Fatalf("green = %+v", v)
	}
	// CI still running: shown, but nobody is needed yet.
	tool.mu.Lock()
	tool.check = release.Check{OK: false, Problem: "CI running", CIStatus: "in_progress", CIURL: "https://ci/1"}
	tool.mu.Unlock()
	s.rel.cmu.Lock()
	s.rel.checks = map[string]relCached{}
	s.rel.cmu.Unlock()
	v = getReleases(t, s)
	if !v.Current || v.NeedsYou {
		t.Fatalf("CI running = %+v", v)
	}
	// CI red: needs the user to look.
	tool.mu.Lock()
	tool.check = release.Check{OK: false, Problem: "CI finished with failure", CIStatus: "completed", CIConclusion: "failure"}
	tool.mu.Unlock()
	s.rel.cmu.Lock()
	s.rel.checks = map[string]relCached{}
	s.rel.cmu.Unlock()
	v = getReleases(t, s)
	if !v.NeedsYou || !strings.Contains(v.NeedsText, "blocked") {
		t.Fatalf("CI red = %+v", v)
	}
}

func TestReleasesTaggedHubBehind(t *testing.T) {
	s, _ := relHubWithPeer(t, "v0.5.8", "v0.5.8", false)
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	v := getReleases(t, s)
	if !v.Current || v.Todo.ID != it.ID || !v.NeedsYou || v.Rollout.Behind != 2 || v.Rollout.Complete {
		t.Fatalf("view = %+v", v)
	}
	if !strings.HasPrefix(v.NeedsText, "Roll out v0.5.9: 2 hosts behind") {
		t.Fatalf("text = %q", v.NeedsText)
	}
	hub := v.Hosts[0]
	if !hub.Hub || !hub.Local || hub.Current || hub.Version != "v0.5.8" {
		t.Fatalf("hub row = %+v", hub)
	}
}

func TestReleasesAnOfflineHostBlocksAllUpgraded(t *testing.T) {
	s, _ := relHubWithPeer(t, "v0.5.9", "v0.5.8", true)
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	// The hub runs it; the peer is down on an older version.
	v := getReleases(t, s)
	if v.Rollout.Current != 1 || v.Rollout.Offline != 1 || v.Rollout.Complete {
		t.Fatalf("rollout = %+v", v.Rollout)
	}
	if v.NeedsYou || v.NeedsText != "v0.5.9: 1 of 2, 1 offline" {
		t.Fatalf("needs = %v %q", v.NeedsYou, v.NeedsText)
	}
	var kube relHost
	for _, h := range v.Hosts {
		if h.Name == "kube" {
			kube = h
		}
	}
	if !kube.Offline || kube.Current {
		t.Fatalf("kube = %+v", kube)
	}
	// The ping stays open, and a person closing it is refused, naming the host.
	if got, _ := s.todos.get(it.ID); !got.active() {
		t.Fatal("the item closed with a host offline")
	}
	w := do(s, "POST", "/api/todos/"+it.ID+"/done", userHdr, `{}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "kube is offline") {
		t.Fatalf("done = %d %s", w.Code, w.Body)
	}
}

func TestReleasesAutoCloseWhenTheFleetMatches(t *testing.T) {
	s, _ := relHubWithPeer(t, "v0.5.9", "v0.5.9", false)
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	v := getReleases(t, s) // closes the item
	got, _ := s.todos.get(it.ID)
	if got.State != TodoDone || got.ClosedBy != "comebye" || got.ClosedAt == nil {
		t.Fatalf("item = %+v", got)
	}
	if !v.Rollout.Complete || v.Rollout.Current != 2 || v.NeedsYou {
		t.Fatalf("view = %+v", v)
	}
	found := false
	for _, a := range actions(s) {
		found = found || a == "release.complete"
	}
	if !found {
		t.Fatalf("no release.complete in the record: %v", actions(s))
	}
	// It shows as done in the tab, and not in the history.
	if !v.Current || v.Todo.ID != it.ID || len(v.History) != 0 {
		t.Fatalf("done view = %+v", v)
	}
	// A day later the tab is back to nothing pending, and the release is history.
	got, _ = s.todos.get(it.ID)
	if _, ok := s.pickCurrent(s.releaseItems(), v.Hosts, got.UpdatedAt.Add(releaseDoneShown+time.Minute)); ok {
		t.Fatal("a finished release is still the current one after a day")
	}
}

func TestReleasesAutoCloseSweepWithoutAPageOpen(t *testing.T) {
	s, _ := relHubWithPeer(t, "v0.5.9", "v0.5.9", false)
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	s.sweepReleases(context.Background())
	if got, _ := s.todos.get(it.ID); got.State != TodoDone {
		t.Fatalf("state = %s", got.State)
	}
}

func TestReleasesTaggedStaysAfterItsItemIsDropped(t *testing.T) {
	s, _ := relHubWithPeer(t, "v0.5.8", "v0.5.8", false)
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	if w := do(s, "POST", "/api/todos/"+it.ID+"/drop", userHdr, `{}`); w.Code != 200 {
		t.Fatalf("drop = %d %s", w.Code, w.Body)
	}
	v := getReleases(t, s)
	if !v.Current || v.Todo.ID != it.ID || v.Todo.State != TodoDropped || v.Rollout.Behind != 2 || !v.NeedsYou {
		t.Fatalf("dropped but unfinished = %+v", v)
	}
	// Its buttons still work: the upgrade is accepted for a closed item.
	tok := v.CSRF
	w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), `{"todo":"`+it.ID+`","version":"v0.5.9"}`)
	if w.Code != 202 {
		t.Fatalf("upgrade for a dropped item = %d %s", w.Code, w.Body)
	}
}

func TestReleasesAFailedUpgradeNeedsYou(t *testing.T) {
	s, _ := relHubWithPeer(t, "v0.5.8", "v0.5.9", false)
	run := s.cfg.Release.Runner.(*fakeRunner)
	run.done.State, run.done.Error = "failed", "health check failed"
	tagOnServer(t, s, "v0.5.9", relSHA)
	v := getReleases(t, s)
	tok := v.CSRF
	if w := do(s, "POST", "/api/hosts/desk/release/upgrade", withCSRF(tok), `{"todo":"`+v.Todo.ID+`","version":"v0.5.9"}`); w.Code != 202 {
		t.Fatalf("upgrade = %d %s", w.Code, w.Body)
	}
	v = getReleases(t, s)
	if !v.NeedsYou || v.NeedsText != "Upgrade failed on desk" {
		t.Fatalf("needs = %v %q", v.NeedsYou, v.NeedsText)
	}
}

func TestReleasesHistoryIsTheLastFive(t *testing.T) {
	s, _, _ := relServer(t)
	base := time.Now().Add(-48 * time.Hour).UTC()
	for i := 1; i <= 7; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		it := Todo{Question: fmt.Sprintf("r%d", i), Kind: "release", CreatedBy: "me", Release: &TodoRelease{
			Version: fmt.Sprintf("v0.5.%d", i), Commit: relSHA, State: relTagged, TaggedAt: &at, RunURL: "https://github.com/o/r/actions/runs/" + fmt.Sprint(100+i)}}
		added, _, err := s.todos.add(it)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.todos.close(added.ID, TodoDone, "comebye", ""); err != nil {
			t.Fatal(err)
		}
	}
	// A proposal that was never tagged is not history.
	newRelease(t, s, "v0.5.50", relSHA2)
	v := getReleases(t, s)
	if len(v.History) != 5 {
		t.Fatalf("history = %d entries: %+v", len(v.History), v.History)
	}
	if v.History[0].Version != "v0.5.7" || v.History[4].Version != "v0.5.3" {
		t.Fatalf("order = %+v", v.History)
	}
	if v.History[0].NotesURL != "https://github.com/o/r/releases/tag/v0.5.7" {
		t.Fatalf("notes url = %q", v.History[0].NotesURL)
	}
}

func TestReleasesAreReadOnlyFromAnotherSite(t *testing.T) {
	s, _, _ := relServer(t)
	r := httptestReq("GET", "/api/releases", "")
	r.Header.Set("Tailscale-User-Login", "me@example.com")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := serveTo(s.base(), r); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site GET = %d", w.Code)
	}
}

func TestReleaseDismissAbandonsATaggedRollout(t *testing.T) {
	s, _ := relHubWithPeer(t, "v0.5.8", "v0.5.8", false)
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	body := `{"version":"v0.5.9","commit":"` + relSHA + `"}`
	tok := getReleases(t, s).CSRF
	// Needs the page's token and the right version/commit.
	if w := do(s, "POST", "/api/release/"+it.ID+"/dismiss", userHdr, body); w.Code != 403 {
		t.Fatalf("no token = %d", w.Code)
	}
	if w := do(s, "POST", "/api/release/"+it.ID+"/dismiss", withCSRF(tok), `{"version":"v0.5.1","commit":"`+relSHA+`"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "confirm_mismatch") {
		t.Fatalf("mismatch = %d %s", w.Code, w.Body)
	}
	if v := getReleases(t, s); !v.Current || !v.NeedsYou {
		t.Fatalf("before = %+v", v)
	}
	if w := do(s, "POST", "/api/release/"+it.ID+"/dismiss", withCSRF(tok), body); w.Code != 200 {
		t.Fatalf("dismiss = %d %s", w.Code, w.Body)
	}
	got, _ := s.todos.get(it.ID)
	if !got.Release.Dismissed || got.Release.DismissedBy == "" || got.State != TodoDropped || got.DropReason != "release dismissed" {
		t.Fatalf("item = %+v %+v", got, got.Release)
	}
	found := false
	for _, a := range actions(s) {
		found = found || a == "release.dismiss"
	}
	if !found {
		t.Fatalf("not audited: %v", actions(s))
	}
	// Out of the tab and the badge; still in the history.
	v := getReleases(t, s)
	if v.Current || v.NeedsYou || len(v.History) != 1 || v.History[0].Version != "v0.5.9" {
		t.Fatalf("after = %+v", v)
	}
	if w := do(s, "POST", "/api/release/"+it.ID+"/dismiss", withCSRF(tok), body); w.Code != 409 {
		t.Fatalf("twice = %d", w.Code)
	}
}

func TestReleaseDismissIsOutOfTheSweepAndOnlyForTagged(t *testing.T) {
	s, _ := relHubWithPeer(t, "v0.5.9", "v0.5.9", false)
	pend := todoOf(t, do(s, "POST", "/api/todos", userHdr, relBody("v0.5.10", relSHA2)).Body.Bytes())
	tok := token(t, s, pend.ID)
	if w := do(s, "POST", "/api/release/"+pend.ID+"/dismiss", withCSRF(tok), `{"version":"v0.5.10","commit":"`+relSHA2+`"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "not_released") {
		t.Fatalf("pending = %d %s", w.Code, w.Body)
	}
	// A dismissed release the fleet runs is not closed or reported by the sweep (its item is already closed).
	it := tagOnServer(t, s, "v0.5.9", relSHA)
	do(s, "POST", "/api/release/"+it.ID+"/dismiss", withCSRF(tok), `{"version":"v0.5.9","commit":"`+relSHA+`"}`)
	s.sweepReleases(context.Background())
	for _, a := range actions(s) {
		if a == "release.complete" {
			t.Fatal("the sweep completed a dismissed release")
		}
	}
	if v := getReleases(t, s); v.Todo.ID == it.ID {
		t.Fatal("a dismissed release is current")
	}
}
