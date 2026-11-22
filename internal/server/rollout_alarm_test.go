package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// rolloutItems is every to-do the rollout alarm filed, in any state.
func rolloutItems(s *Server) []Todo {
	var out []Todo
	for _, it := range s.todos.list("all") {
		if it.Source == SourceRollout {
			out = append(out, it)
		}
	}
	return out
}

func activeRollout(items []Todo) []Todo {
	var out []Todo
	for _, it := range items {
		if it.active() {
			out = append(out, it)
		}
	}
	return out
}

// A failed hub bump files one action item; a second failure adds nothing;
// a later success closes it.
func TestRolloutAlarmDeployFailsOnceThenCloses(t *testing.T) {
	okFile := filepath.Join(t.TempDir(), "flux-installed")
	// the bump's own failure path: no flux on PATH, plus a secret it must not leak
	script, _ := deployScript(t, `if [ -e `+okFile+` ]; then echo "bump: ok version=$1 total=2s"; exit 0; fi
echo "bump: edit      +0s attempt 1"
echo "bump: push      +1s token=hunter2-not-for-the-todo"
echo "flux: command not found" >&2
echo "bump: FAILED phase=reconcile reason=flux: command not found"
exit 1`)
	s, _ := deployHub(t, script)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approve(s, it.ID, tok, "v0.5.9", relSHA)
	waitRel(t, s, it.ID, relTagged)

	deployTo := func(state string) {
		t.Helper()
		if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
			t.Fatalf("deploy = %d %s", code, body)
		}
		waitUntil(t, "deploy "+state, func() bool {
			st, ok := deployState(s, it.ID)
			return ok && st.Record != nil && st.Record.State == state && !s.rel.deploying.Load()
		})
	}

	deployTo(depFailed)
	items := rolloutItems(s)
	if len(items) != 1 || !items[0].active() {
		t.Fatalf("after one failure: %+v", items)
	}
	a := items[0]
	if a.Kind != "action" || a.Question != "The v0.5.9 hub rollout failed" || a.CreatedBy != AutoCreator {
		t.Fatalf("item = %+v", a)
	}
	for _, want := range []string{"Phase: `reconcile`", "flux: command not found", "Bump hub"} {
		if !strings.Contains(a.Context, want) {
			t.Fatalf("context lacks %q:\n%s", want, a.Context)
		}
	}
	if strings.Contains(a.Context, "hunter2") {
		t.Fatalf("context leaks a secret:\n%s", a.Context)
	}
	if len(a.Links) != 1 || a.Links[0].URL != "/#/todo/"+it.ID {
		t.Fatalf("links = %+v", a.Links)
	}

	deployTo(depFailed)
	if items := rolloutItems(s); len(items) != 1 || !items[0].active() {
		t.Fatalf("a second failure changed the items: %+v", items)
	}

	if err := os.WriteFile(okFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deployTo(depOK)
	items = rolloutItems(s)
	if len(items) != 1 || items[0].State != TodoDone || items[0].ClosedBy != AutoCreator {
		t.Fatalf("after the success: %+v", items)
	}
}

// A host upgrade of the released version: the same rules, per host.
func TestRolloutAlarmUpgradePerHost(t *testing.T) {
	s, _ := deployHub(t)
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approve(s, it.ID, tok, "v0.5.9", relSHA)
	waitRel(t, s, it.ID, relTagged)

	rec := func(id, state, errText string) *upgrade.Record {
		return &upgrade.Record{ID: id, Version: "v0.5.9", From: "v0.5.8", State: state, Error: errText, Started: time.Now(), Output: "fetching\nsmoke check failed"}
	}
	s.settleUpgradeAudit("peer1", rec("u1", upgrade.RecRolledBack, "the new binary failed its smoke check"))
	s.settleUpgradeAudit("peer1", rec("u1", upgrade.RecRolledBack, "the new binary failed its smoke check")) // the same run, seen again
	s.settleUpgradeAudit("peer1", rec("u2", upgrade.RecFailed, "download failed"))
	s.settleUpgradeAudit("peer2", rec("u3", upgrade.RecFailed, "download failed"))
	s.settleUpgradeAudit("peer3", &upgrade.Record{ID: "u4", Version: "v0.5.9", Rollback: true, State: upgrade.RecFailed}) // a rollback is not a rollout
	s.settleUpgradeAudit("peer4", &upgrade.Record{ID: "u5", Version: "v0.6.0", State: upgrade.RecFailed})                 // no release item
	act := activeRollout(rolloutItems(s))
	if len(act) != 2 {
		t.Fatalf("open items = %+v", act)
	}
	byQ := map[string]Todo{}
	for _, a := range act {
		byQ[a.Question] = a
	}
	p1, ok := byQ["The v0.5.9 upgrade of peer1 failed"]
	if !ok || !strings.Contains(p1.Context, "smoke check") {
		t.Fatalf("peer1 item = %+v", byQ)
	}
	if _, ok := byQ["The v0.5.9 upgrade of peer2 failed"]; !ok {
		t.Fatalf("peer2 item missing: %+v", byQ)
	}

	s.settleUpgradeAudit("peer1", rec("u6", upgrade.RecOK, ""))
	act = activeRollout(rolloutItems(s))
	if len(act) != 1 || act[0].Question != "The v0.5.9 upgrade of peer2 failed" {
		t.Fatalf("after peer1's success: %+v", act)
	}
	// A new failure after the item closed files a new one.
	s.settleUpgradeAudit("peer1", rec("u7", upgrade.RecFailed, "again"))
	if act = activeRollout(rolloutItems(s)); len(act) != 2 {
		t.Fatalf("after a new failure: %+v", act)
	}
}
