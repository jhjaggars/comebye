package server

import (
	"fmt"
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

// ---- the way back from a hub bump that failed after its push (issue #141, slice 6) ----

const bumpSHA = "0123456789abcdef0123456789abcdef01234567"

func bumpOut(phases ...string) string {
	var b strings.Builder
	for i, p := range phases {
		extra := ""
		if p == "push" {
			extra = bumpSHA
		}
		fmt.Fprintf(&b, "bump: %-9s +%ds %s\n", p, i, extra)
	}
	return b.String()
}

func TestHubUndoNamesTheCommandOnlyAfterThePush(t *testing.T) {
	failed := func(phase string, phases ...string) DeployRecord {
		return DeployRecord{Version: "v0.5.59", State: depFailed, Output: bumpOut(phases...) + "bump: FAILED phase=" + phase + " reason=x",
			Headline: "bump: FAILED phase=" + phase + " reason=x", Error: "exit status 1"}
	}
	got := hubUndo(failed("readyz", "sums", "edit", "push", "reconcile", "rollout", "readyz"), "/home/u/.local/bin/comebye-hub-bump", "v0.5.58", "relhost")
	for _, want := range []string{"homelab commit `0123456789ab`", "main pins v0.5.59", "run this on relhost", "comebye-hub-bump v0.5.58 --allow-older", "pins v0.5.58 again", "waits until the hub reports v0.5.58"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/home/") {
		t.Fatalf("the command's path leaked:\n%s", got)
	}
	// Nothing was pushed: no undo.
	for _, rec := range []DeployRecord{
		failed("edit", "sums", "edit"),
		failed("push", "sums", "edit", "push"), // the push itself was refused
		failed("pull"),                         // the wrapper failed first
		{Version: "v0.5.59", State: depFailed, Output: "some other deploy command\nFAILED"}, // not the bump script
	} {
		if u := hubUndo(rec, "comebye-hub-bump", "v0.5.58", "relhost"); u != "" {
			t.Errorf("%q: undo %q", rec.Headline, u)
		}
	}
	// The previous version is not known (or not older): the command, with vPREV to fill in.
	for _, prev := range []string{"", "v0.5.59", "dev"} {
		u := hubUndo(failed("reconcile", "sums", "edit", "push", "reconcile"), "comebye-hub-bump", prev, "relhost")
		if !strings.Contains(u, "`comebye-hub-bump vPREV --allow-older` on relhost") {
			t.Errorf("prev %q: %s", prev, u)
		}
	}
	// A deploy command name that is not a plain program falls back to the script's name.
	if u := hubUndo(failed("reconcile", "sums", "edit", "push", "reconcile"), "rm -rf `x`", "v0.5.58", "relhost"); !strings.Contains(u, "comebye-hub-bump.sh v0.5.58 --allow-older") {
		t.Fatalf("odd command: %s", u)
	}
}

// Through a real deploy: the bump pushed, then the hub never reported the
// version; the item names the command that puts the hub's version back.
func TestRolloutAlarmDeployAfterThePushNamesTheUndo(t *testing.T) {
	script, _ := deployScript(t, `echo "bump: sums      +0s"
echo "bump: edit      +0s attempt 1"
echo "bump: push      +1s `+bumpSHA+`"
echo "bump: reconcile +2s flux reconcile source git, then kustomization"
echo "bump: rollout   +3s waiting up to 300s"
echo "bump: readyz    +9s asking the hub"
echo "bump: FAILED phase=readyz reason=/readyz did not report status ok on $1 within 2 minutes"
exit 1`)
	s, _ := deployHub(t, script)
	s.cfg.Version = "v0.5.8" // the hub still runs the old one
	it := newRelease(t, s, "v0.5.9", relSHA)
	tok := token(t, s, it.ID)
	approve(s, it.ID, tok, "v0.5.9", relSHA)
	waitRel(t, s, it.ID, relTagged)
	if code, body := doDeploy(s, it.ID, tok, "v0.5.9", relSHA); code != 202 {
		t.Fatalf("deploy = %d %s", code, body)
	}
	waitUntil(t, "deploy failed", func() bool {
		st, ok := deployState(s, it.ID)
		return ok && st.Record != nil && st.Record.State == depFailed && !s.rel.deploying.Load()
	})
	items := rolloutItems(s)
	if len(items) != 1 {
		t.Fatalf("items %+v", items)
	}
	c := items[0].Context
	for _, want := range []string{"Phase: `readyz`", "homelab commit `0123456789ab`", filepath.Base(script) + " v0.5.8 --allow-older", "pins v0.5.8 again"} {
		if !strings.Contains(c, want) {
			t.Fatalf("context lacks %q:\n%s", want, c)
		}
	}
}
