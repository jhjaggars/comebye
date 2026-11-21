package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// pushlessWorld is realHub (a hub, its provisioner "prov" with a fake
// worker tool, and the host demo1 that prov built) with a checkout of
// "comebye" under the provisioner's launch root. No VM, no ssh, no GitHub:
// the tool is a script that answers from files.
type pushlessWorld struct {
	s    *Server
	prov *provPeer
	wk   *fakeWorkerDir
	hdr  map[string]string
	repo string // the provisioner's checkout
	tip  string
}

func newPushlessWorld(t *testing.T) *pushlessWorld {
	t.Helper()
	s, prov, wk, hdr, _ := realHub(t)
	root := t.TempDir()
	repo := filepath.Join(root, "comebye")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	prov.peer.cfg.LaunchRoots = []string{root}
	w := &pushlessWorld{s: s, prov: prov, wk: wk, hdr: hdr, repo: repo, tip: strings.Repeat("ab", 20)}
	w.answer("docs/x", "pushed")
	return w
}

func (w *pushlessWorld) answer(branch, pushed string) {
	b, _ := json.Marshal(map[string]string{"name": "demo1", "branch": branch, "tip": w.tip, "local": "created", "pushed": pushed})
	os.WriteFile(filepath.Join(w.wk.dir, "fetch.json"), b, 0o644)
}

func (w *pushlessWorld) setting(t *testing.T, body string) {
	t.Helper()
	if rec := do(w.s, "PUT", "/api/settings", w.hdr, `{"pushless_fetch":`+body+`}`); rec.Code != 200 {
		t.Fatalf("put settings = %d %s", rec.Code, rec.Body)
	}
}

// reported adds a handoff on host that has just reported.
func (w *pushlessWorld) reported(t *testing.T, host, branch, report string) ledger.Entry {
	t.Helper()
	e, err := w.s.ledger.Add(ledger.Entry{Host: host, Repo: "/data/code/comebye", RepoName: "comebye", Branch: branch,
		AgentID: host + "/w1:p1", PaneID: "w1:p1", Worktree: "/data/worktrees/comebye/x", Status: ledger.Running})
	if err != nil {
		t.Fatal(err)
	}
	e, ok, err := w.s.ledger.Report(e.AgentID, report, time.Now())
	if err != nil || !ok {
		t.Fatalf("report: %v %v", ok, err)
	}
	return e
}

func (w *pushlessWorld) fetches() int { return w.wk.runs(" fetch ") }

func TestPushlessFetchIsOffByDefault(t *testing.T) {
	w := newPushlessWorld(t)
	var v settingsView
	getJSON(t, w.s, "/api/settings", userHdr, &v)
	if v.Settings.PushlessFetch != nil {
		t.Fatalf("on by default: %+v", v.Settings.PushlessFetch)
	}
	e := w.reported(t, "demo1", "docs/x", "Done. Not pushed: this host has no GitHub login.")
	if w.s.pushFetchDue(e, time.Now()) {
		t.Fatal("due with the setting off")
	}
	w.s.afterReport(e)
	got := w.s.pushFetch(context.Background(), e)
	if got.PushFetch != nil || w.fetches() != 0 || w.prov.asked("/api/provisioner/host/fetch") != 0 {
		t.Fatalf("fetched with the setting off: %+v", got.PushFetch)
	}

	// Listed but switched off, or on but another host: still nothing.
	w.setting(t, `{"enabled":false,"hosts":["demo1"]}`)
	if w.s.pushFetchDue(e, time.Now()) {
		t.Error("due while switched off")
	}
	w.setting(t, `{"enabled":true,"hosts":["other"]}`)
	if w.s.pushFetchDue(e, time.Now()) {
		t.Error("due for a host that is not listed")
	}
}

func TestPushlessFetchRunsOnReportThroughTheProvisioner(t *testing.T) {
	w := newPushlessWorld(t)
	w.setting(t, `{"enabled":true,"hosts":["demo1"]}`)
	e := w.reported(t, "demo1", "docs/x", "Done. Pushed: no, the hub fetches it.")
	w.s.afterReport(e)
	var got ledger.Entry
	waitUntil(t, "push fetch recorded", func() bool {
		got, _ = w.s.ledger.Get(e.ID)
		return got.PushFetch != nil
	})
	f := got.PushFetch
	if !f.OK || f.Tip != w.tip || f.Via != "prov" || f.Pushed != "pushed" || f.ReportSeq != got.ReportSeq {
		t.Fatalf("push fetch %+v", f)
	}
	argv := strings.Join(w.wk.argvLines(), "\n")
	if !strings.Contains(argv, "-no-prompt fetch demo1 docs/x --repo "+w.repo+" --remote-repo comebye --push --json") {
		t.Errorf("argv:\n%s", argv)
	}
	if got.Status != ledger.Finished {
		t.Errorf("status %s", got.Status)
	}

	// The same report again: no second fetch, from the hub-check or a repeat.
	w.s.afterReport(got)
	w.s.pushFetch(context.Background(), got)
	if n := w.fetches(); n != 1 {
		t.Errorf("fetched %d times for one report", n)
	}

	// The hub-check: "pushed" is settled by the hub's push, even though the
	// worker's own clone counts the commits as on no remote.
	c := &worktreeChecks{Exists: true, Tip: w.tip[:12], BaseUsed: "origin/main", Unmerged: 2, Detail: &worktreeDetail{Unpushed: 2}}
	h := hubCheckOf(got, c, time.Now())
	txt := hubText(h)
	if !strings.Contains(txt, "pushed/ok: demo1 is push-less: prov fetched the branch and pushed it to origin") || strings.Contains(txt, "on no remote") {
		t.Errorf("hub-check:\n%s", txt)
	}
	// The worker moved on since: the push no longer settles it.
	c.Tip = strings.Repeat("cd", 6)
	if txt := hubText(hubCheckOf(got, c, time.Now())); !strings.Contains(txt, "pushed/warn: the hub pushed") || !strings.Contains(txt, "on no remote") {
		t.Errorf("hub-check after the worker moved:\n%s", txt)
	}

	evs := w.s.events.query(time.Time{}, []string{evPushFetch}, 50)
	found := false
	for _, ev := range evs {
		if ev.Kind == evPushFetch && strings.Contains(ev.Text, "pushed it to origin") {
			found = true
		}
	}
	if !found {
		t.Errorf("no event: %+v", evs)
	}
}

func TestPushlessFetchFailureIsANoteNotAFailure(t *testing.T) {
	w := newPushlessWorld(t)
	w.setting(t, `{"enabled":true,"hosts":["demo1"]}`)
	w.wk.set(t, "fetch.fail", "origin docs/x (abc) is not an ancestor of the worker's (def): pushing would overwrite it; --force replaces it")
	e := w.reported(t, "demo1", "docs/x", "Done.")
	got := w.s.pushFetch(context.Background(), e)
	f := got.PushFetch
	if f == nil || f.OK || !strings.Contains(f.Note, "not an ancestor") {
		t.Fatalf("push fetch %+v", f)
	}
	if got.Status != ledger.Finished || got.LandCheck != nil {
		t.Fatalf("a failed fetch changed the handoff: %s %+v", got.Status, got.LandCheck)
	}
	c := &worktreeChecks{Exists: true, Tip: w.tip[:12], Detail: &worktreeDetail{Unpushed: 1}}
	txt := hubText(hubCheckOf(got, c, time.Now()))
	if !strings.Contains(txt, "pushed/warn: the hub could not bring the branch back from push-less demo1") || !strings.Contains(txt, "on no remote") {
		t.Errorf("hub-check:\n%s", txt)
	}
	// Not retried at once for the same report; after the retry gap it is.
	if w.s.pushFetchDue(got, time.Now()) {
		t.Error("due again at once")
	}
	if !w.s.pushFetchDue(got, time.Now().Add(pushFetchRetry+time.Second)) {
		t.Error("not due after the retry gap")
	}
}

func TestPushlessFetchSkipsWhatItMustNotFetch(t *testing.T) {
	w := newPushlessWorld(t)
	w.setting(t, `{"enabled":true,"hosts":["demo1","ghost"]}`)

	// A landing pushes the base itself: never fetched.
	land := w.reported(t, "demo1", "docs/x", "Done.")
	land.Status = ledger.Landing
	if w.s.pushFetchDue(land, time.Now()) {
		t.Error("due for a landing")
	}
	// The default branch is not a work branch.
	e := ledger.Entry{Host: "demo1", Branch: "main", Status: ledger.Finished, FinalReport: "x"}
	if w.s.pushFetchDue(e, time.Now()) {
		t.Error("due for main")
	}
	// A listed host no provisioner reports: a note, and the tool never runs.
	g := w.reported(t, "ghost", "docs/y", "Done.")
	got := w.s.pushFetch(context.Background(), g)
	if got.PushFetch == nil || got.PushFetch.OK || !strings.Contains(got.PushFetch.Note, "no provisioner reports ghost") {
		t.Fatalf("ghost: %+v", got.PushFetch)
	}
	if w.fetches() != 0 {
		t.Error("the tool ran")
	}
}

func TestPeerFetchRouteRefuses(t *testing.T) {
	w := newPushlessWorld(t)
	p := w.prov.peer
	for _, c := range []struct{ name, body, code string }{
		{"not built here", `{"name":"other","repo_name":"comebye","remote_repo":"comebye","branch":"docs/x"}`, "no_host"},
		{"no checkout", `{"name":"demo1","repo_name":"nope","remote_repo":"nope","branch":"docs/x"}`, "no_repo"},
		{"default branch", `{"name":"demo1","repo_name":"comebye","remote_repo":"comebye","branch":"main"}`, "bad_request"},
		{"option branch", `{"name":"demo1","repo_name":"comebye","remote_repo":"comebye","branch":"--upload-pack=x"}`, "bad_request"},
		{"path repo", `{"name":"demo1","repo_name":"../x","remote_repo":"comebye","branch":"docs/x"}`, "bad_request"},
		{"unknown field", `{"name":"demo1","repo_name":"comebye","remote_repo":"comebye","branch":"docs/x","force":true}`, "bad_request"},
	} {
		rec := do(p, "POST", "/api/provisioner/host/fetch", hubCall, c.body)
		if rec.Code < 400 || !strings.Contains(rec.Body.String(), c.code) {
			t.Errorf("%s = %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if w.fetches() != 0 {
		t.Fatalf("the tool ran for a refused request: %v", w.wk.argvLines())
	}
	rec := do(p, "POST", "/api/provisioner/host/fetch", hubCall, `{"name":"demo1","repo_name":"comebye","remote_repo":"comebye","branch":"docs/x"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), w.tip) {
		t.Fatalf("fetch = %d %s", rec.Code, rec.Body)
	}
}

func TestPushlessSettingIsValidated(t *testing.T) {
	w := newPushlessWorld(t)
	for _, bad := range []string{`{"enabled":true,"hosts":["Not A Host"]}`, `{"enabled":true,"via":"x;y"}`} {
		if rec := do(w.s, "PUT", "/api/settings", w.hdr, `{"pushless_fetch":`+bad+`}`); rec.Code != 400 {
			t.Errorf("%s = %d", bad, rec.Code)
		}
	}
	w.setting(t, `{"enabled":true,"hosts":["demo1","DEMO1"],"via":"prov"}`)
	// A PUT without the field keeps it.
	if rec := do(w.s, "PUT", "/api/settings", w.hdr, `{"default_kind":""}`); rec.Code != 200 {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	p := w.s.settings.get().PushlessFetch
	if p == nil || !p.Enabled || len(p.Hosts) != 1 || p.Hosts[0] != "demo1" || p.Via != "prov" {
		t.Fatalf("setting %+v", p)
	}
	// {} turns it off.
	w.setting(t, `{}`)
	if w.s.settings.get().PushlessFetch != nil {
		t.Error("{} did not turn it off")
	}
}

func TestHubCheckRouteFetchesBeforeItReadsPushed(t *testing.T) {
	w := newPushlessWorld(t)
	w.setting(t, `{"enabled":true,"hosts":["demo1"]}`)
	e := w.reported(t, "demo1", "docs/x", "Done.") // no afterReport: the hub-check must do it
	rec := do(w.s, "GET", "/api/ledger/"+e.ID+"/hubcheck", userHdr, "")
	if rec.Code != 200 {
		t.Fatalf("hubcheck = %d %s", rec.Code, rec.Body)
	}
	got, _ := w.s.ledger.Get(e.ID)
	if got.PushFetch == nil || !got.PushFetch.OK || w.fetches() != 1 {
		t.Fatalf("not fetched before the check: %+v (%d runs)", got.PushFetch, w.fetches())
	}
	do(w.s, "GET", "/api/ledger/"+e.ID+"/hubcheck", userHdr, "")
	if w.fetches() != 1 {
		t.Errorf("a second hub-check fetched again: %d", w.fetches())
	}
}
