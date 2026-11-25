package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// ---- the skipped hub (v0.5.57) ----

// The plan was made while the release host's deploy status could not be
// read: the hub wave stays a deploy, and the hub is bumped.
func TestRolloutPlanKeepsTheHubWhenTheDeployStatusFails(t *testing.T) {
	r := newProvRig(t)
	r.f.set("hub", func(h *relHost) { h.Enabled = false })
	hosts := r.f.Hosts(context.Background(), "v0.6.4")
	plan, err := rolloutPlan(hosts, nil, "", true, hubDeploys(hosts, deployStatus{}, errors.New("relhost did not answer in time"), true))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, st := range plan {
		got = append(got, st.Host+"/"+st.Mode+"/"+st.Phase)
	}
	if strings.Join(got, " ") != "a//pending b//pending hub/deploy/pending prov/with_hub/pending" {
		t.Fatal(got)
	}
	// With -release-host set, a peer's "no command" (or a non-200) is
	// decided at the hub's turn too; this host's own answer is believed.
	if !hubDeploys(hosts, deployStatus{Enabled: false}, nil, true) {
		t.Fatal("a remote release host's 'no command' skipped the deploy at plan time")
	}
	if hubDeploys(hosts, deployStatus{Enabled: false}, nil, false) {
		t.Fatal("no local command, yet a deploy")
	}
	// A hub that upgrades itself as a host is not taken for a pod.
	r.f.set("hub", func(h *relHost) { h.Enabled = true })
	if hubDeploys(r.f.Hosts(context.Background(), "v0.6.4"), deployStatus{}, errors.New("x"), true) {
		t.Fatal("a self-upgrading hub was planned as a deploy")
	}

	// Run it: the peers, then the deploy.
	j, err := r.e.Start("t1", "v0.6.4", "", "jesse", plan)
	if err != nil {
		t.Fatal(err)
	}
	r.peersDone("a", "b")
	r.want("running a=ok b=ok hub=upgrading prov=pending")
	if r.f.deploys != 1 || j.ID == "" {
		t.Fatalf("deploys = %d", r.f.deploys)
	}
}

// When the hub step's turn comes and the release host really has no deploy
// command, the rollout stops at the hub: it is not skipped, and not "ok".
func TestRolloutStopsAtTheHubWhenTheDeployIsRefused(t *testing.T) {
	r := newProvRig(t)
	r.startAll()
	r.f.deployErr = &peerFault{host: "relhost", code: "deploy_disabled", msg: "this host has no deploy command"}
	r.f.statusErr = errors.New("unreachable") // so the loop's re-ask does not retry it at once
	r.peersDone("a", "b")
	r.tick()
	r.want("stopped a=ok b=ok hub=failed prov=pending")
	if j, _ := r.e.Get(); !j.Plan[2].Reask {
		t.Fatal("deploy_disabled is not re-asked")
	}
	// A refusal that is not about the command is left to the user.
	r2 := newProvRig(t)
	r2.startAll()
	r2.f.deployErr = &peerFault{host: "relhost", code: "build_not_green", msg: "the release build is failure"}
	r2.peersDone("a", "b")
	r2.tick()
	if j, _ := r2.e.Get(); j.State != roStopped || j.Plan[2].Reask {
		t.Fatalf("build_not_green: %s reask=%v", j.State, j.Plan[2].Reask)
	}
	j, _ := r.e.Get()
	if j.Stop == nil || j.Stop.Host != "hub" || !strings.Contains(j.Stop.Reason, "no deploy command") {
		t.Fatalf("stop = %+v", j.Stop)
	}
}

// A rollout that ended with a skipped step is not reported as ok, and one
// that skipped the hub says the hub was not bumped and how to bump it.
func TestRolloutFinishWithASkippedHubIsNotOK(t *testing.T) {
	r := newProvRig(t)
	r.f.set("hub", func(h *relHost) { h.Enabled = false })
	plan, err := rolloutPlan(r.f.Hosts(context.Background(), "v0.6.4"), nil, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Start("t1", "v0.6.4", "", "jesse", plan); err != nil {
		t.Fatal(err)
	}
	r.peersDone("a", "b")
	r.tick()
	r.want("done a=ok b=ok hub=skipped prov=skipped")
	last := r.audit[len(r.audit)-1]
	if last.Action != "rollout.finish" || last.OK || !strings.Contains(last.Detail, "not ok: 2 of 4 hosts upgraded, 2 skipped (hub, prov); the hub was not bumped") {
		t.Fatalf("finish = %+v", last)
	}
	j, _ := r.e.Get()
	if n := rolloutNoteOf(j); !strings.Contains(n, "not rolled out to the hub") || !strings.Contains(n, "bump the hub") {
		t.Fatalf("note = %q", n)
	}

	// And one with nothing skipped is ok.
	r2 := newRolloutRig(t, "a")
	r2.e.soak = 0
	r2.start()
	r2.tick()
	r2.f.finish("a", upgrade.RecOK, "v0.6.4")
	r2.tick()
	r2.tick()
	last = r2.audit[len(r2.audit)-1]
	if last.Action != "rollout.finish" || !last.OK || !strings.HasPrefix(last.Detail, "ok: all 1 hosts") {
		t.Fatalf("finish = %+v", last)
	}
}

// A hub step that stopped because the release host could not start the
// deploy is retried by the loop once the release host answers with a
// command; one whose deploy ran and failed is not.
func TestRolloutRetriesAStoppedHubOnceTheReleaseHostAnswers(t *testing.T) {
	r := newProvRig(t)
	var trouble []string
	r.e.onHubTrouble = func(j Rollout, st RolloutStep) { trouble = append(trouble, st.Host+"="+st.Phase) }
	r.e.catchUpEvery = time.Minute
	r.startAll()
	r.f.deployErr = errors.New("relhost is unreachable: connection refused")
	r.peersDone("a", "b")
	r.want("running a=ok b=ok hub=pending prov=pending")
	r.f.advance(r.e.startRetry)
	r.tick()
	r.want("stopped a=ok b=ok hub=failed prov=pending")
	j, _ := r.e.Get()
	if !j.Plan[2].Reask || !strings.Contains(j.Stop.Reason, "connection refused") || strings.Join(trouble, " ") != "hub=failed" {
		t.Fatalf("stop = %+v %+v trouble=%v", j.Stop, j.Plan[2], trouble)
	}
	if n := rolloutNoteOf(j); !strings.Contains(n, "stopped at the hub") || !strings.Contains(n, "bump the hub") {
		t.Fatalf("note = %q", n)
	}
	// Still unreachable: it stays stopped.
	r.f.statusErr = errors.New("unreachable")
	r.f.advance(time.Minute)
	r.tick()
	r.want("stopped a=ok b=ok hub=failed prov=pending")
	// It answers: the loop retries the hub, and the bump runs.
	r.f.statusErr, r.f.deployErr = nil, nil
	r.f.advance(time.Minute)
	r.tick()
	r.want("running a=ok b=ok hub=pending prov=pending")
	r.tick()
	r.want("running a=ok b=ok hub=upgrading prov=pending")
	if r.f.deploys != 1 || !strings.Contains(r.actions(), "rollout.catchup:hub") {
		t.Fatalf("deploys=%d %s", r.f.deploys, r.actions())
	}
	// A deploy that ran and failed waits for the user's Retry.
	r.f.endDeploy(depFailed, "exit status 1", "")
	r.tick()
	r.want("stopped a=ok b=ok hub=failed prov=pending")
	r.f.advance(time.Hour)
	r.tick()
	r.tick()
	r.want("stopped a=ok b=ok hub=failed prov=pending")
}

// Every hub stop or skip files the hub's To do, once per release.
func TestRolloutHubStopFilesATodo(t *testing.T) {
	s, f, _, it, _ := rolloutRouteRig(t, "a")
	f.hosts["hub"] = &relHost{Name: "hub", Status: "ok", Version: "v0.5.8", Hub: true, Local: true}
	f.order = append(f.order, "hub")
	s.rollout.onHubTrouble = s.alarmHubStep
	f.deployErr = &peerFault{host: "relhost", code: "deploy_disabled", msg: "this host has no deploy command"}
	plan, err := rolloutPlan(f.Hosts(context.Background(), "v0.5.9"), nil, "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.rollout.Start(it.ID, "v0.5.9", relSHA, "me", plan); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.rollout.tick(ctx)
	f.finish("a", upgrade.RecOK, "v0.5.9")
	for range 3 {
		s.rollout.tick(ctx)
	}
	if j, _ := s.rollout.Get(); j.State != roStopped || j.Stop.Host != "hub" {
		t.Fatalf("job = %s %+v", j.State, j.Stop)
	}
	var items []Todo
	for _, t := range s.todos.list("") {
		if t.Source == SourceRollout {
			items = append(items, t)
		}
	}
	if len(items) != 1 || !strings.Contains(items[0].Question, "hub rollout failed") || !strings.Contains(items[0].Context, "no deploy command") {
		t.Fatalf("items = %+v", items)
	}
	// A hub planned skipped (it cannot be upgraded at all) files it too.
	s2, f2, _, it2, _ := rolloutRouteRig(t, "a")
	f2.hosts["hub"] = &relHost{Name: "hub", Status: "ok", Version: "v0.5.8", Hub: true, Local: true}
	f2.order = append(f2.order, "hub")
	s2.rollout.onHubTrouble = s2.alarmHubStep
	plan, _ = rolloutPlan(f2.Hosts(context.Background(), "v0.5.9"), nil, "", true, false)
	if _, err := s2.rollout.Start(it2.ID, "v0.5.9", relSHA, "me", plan); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, t := range s2.todos.list("") {
		if t.Source == SourceRollout {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("planned-skipped hub filed %d items", n)
	}
}
