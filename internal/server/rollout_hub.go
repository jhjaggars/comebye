package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The hub wave: the last step of a rollout.
//
// On a hub that is a pod, the hub is upgraded by the deploy command (Bump
// hub), which runs on the release host, commits the version to the GitOps
// repository and waits for the cluster to roll it. That is a different path
// with a different failure story (nothing rolls it back by itself), so it is
// its own wave, last, and only after the peers before it are proven.
//
// The deploy restarts this very hub, and with it the loop. That is why the
// job is a file: the new hub reads rollout.json, sees the hub step in
// "upgrading", asks the release host for the deploy record, and, with the
// record ok and itself reporting the target version, finishes the rollout.
// A hub that comes back on the old version fails the gate and stops the
// rollout with the deploy's output.
//
// When the hub is an ordinary host with -release-upgrade (a single-host
// setup) its step is an upgrade like any other's, and the same restart
// handling applies through the upgrade record.

const stepModeDeploy = "deploy"

// stepModeWithHub: a peer the hub's deploy upgrades along with the hub (it
// reports with_hub: a -hub-only peer such as the in-cluster provisioner,
// which the app cannot upgrade in place). Its step follows the hub's and
// only watches it come back on the version; it is never started.
const stepModeWithHub = "with_hub"

// the deploy cannot outlive its own cap by much
const hubRunWait = deployTimeout + time.Minute

// hubWave builds the last step of the plan, or none when the hub already runs
// the version. deploy says the hub is deployed by the command, not upgraded
// as a host.
func hubWave(hosts []relHost, deploy bool) (*RolloutStep, error) {
	for _, h := range hosts {
		if !h.Hub {
			continue
		}
		if h.Current && !(deploy && withHubBehind(hosts)) {
			return nil, nil
		}
		st := RolloutStep{Host: h.Name, Wave: waveHub, Phase: roPending}
		switch {
		case deploy:
			st.Mode = stepModeDeploy
		case hostUp(h) && !h.Enabled:
			return nil, errors.New("the hub cannot be upgraded from the app: it has no deploy command and is not started with -release-upgrade")
		}
		return &st, nil
	}
	return nil, errors.New("this fleet has no hub host")
}

// withHubBehind: a reachable peer that the hub's deploy upgrades is not on
// the version yet (a hub already on it is deployed again for it).
func withHubBehind(hosts []relHost) bool {
	for _, h := range hosts {
		if !h.Hub && h.WithHub && !h.Current && hostUp(h) {
			return true
		}
	}
	return false
}

// rolloutPlan is the whole plan of a rollout: the peers (planRollout), then,
// when hub is asked for, the hub wave: the hub's own step, then the peers
// its deploy upgrades. What the app cannot upgrade does not refuse the plan:
// it is in it as a step already skipped, with the reason, and the rest goes
// ahead. Only a bad explicit order is an error.
func rolloutPlan(hosts []relHost, order []string, canary string, hub, deploy bool) ([]RolloutStep, error) {
	plan, err := planRollout(hosts, order, canary)
	if err != nil || !hub {
		return plan, err
	}
	st, herr := hubWave(hosts, deploy)
	switch {
	case herr != nil:
		for _, h := range hosts {
			if h.Hub {
				plan = append(plan, RolloutStep{Host: h.Name, Wave: waveHub, Phase: roSkipped, Detail: herr.Error()})
			}
		}
	case st != nil:
		plan = append(plan, *st)
	}
	bumps := st != nil && st.Mode == stepModeDeploy
	for _, h := range hosts {
		if h.Hub || !h.WithHub || h.Current {
			continue
		}
		w := RolloutStep{Host: h.Name, Wave: waveHub, Mode: stepModeWithHub, Phase: roPending}
		if !bumps {
			w.Phase, w.Detail = roSkipped, "it is upgraded with the hub, and this rollout does not deploy the hub"
		}
		plan = append(plan, w)
	}
	return plan, nil
}

// actionable: the plan has a step that will do something.
func actionable(plan []RolloutStep) bool {
	for _, st := range plan {
		if st.Phase != roSkipped && st.Phase != roOK {
			return true
		}
	}
	return false
}

// withHubPending: a step of the hub's deploy is still to come for a peer
// that is not on the version.
func (e *rolloutEngine) withHubPending(j Rollout) bool {
	for _, st := range j.Plan {
		if st.Mode != stepModeWithHub || st.Phase != roPending {
			continue
		}
		if h, ok := findHost(e.hosts, st.Host); !ok || !h.Current {
			return true
		}
	}
	return false
}

// beginWithHub: the hub's step is over; a peer its deploy upgrades is
// watched until it is back on the version. Nothing is started.
func (e *rolloutEngine) beginWithHub(j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	if !found {
		e.skip(j, st, now, "no longer in the fleet")
		return true
	}
	t := now
	st.From, st.Started, st.At = h.Version, &t, now
	for _, hs := range j.Plan {
		if hs.Wave == waveHub && hs.Mode == stepModeDeploy && hs.Dry {
			st.Dry = true
			e.finishStep(j, st, h, now, "dry run: upgraded with the hub")
			return true
		}
	}
	if hostUp(h) && h.Current {
		e.finishStep(j, st, h, now, "upgraded with the hub")
		return true
	}
	st.Phase, st.Detail = roVerifying, "upgraded with the hub: waiting for it to be back on "+j.Version
	j.Note = "waiting for " + st.Host + " (upgraded with the hub)"
	return true
}

// verifyWithHub: the hub's deploy restarts the peer too. One that does not
// come back on the version is skipped with the reason, not a stop: the
// deploy already ran, and nothing the app could retry would change that.
func (e *rolloutEngine) verifyWithHub(j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	if found && hostUp(h) && h.Current {
		st.To = h.Version
		e.finishStep(j, st, h, now, "upgraded with the hub")
		return true
	}
	if now.Sub(st.At) >= e.backWait {
		why := fmt.Sprintf("not back on %s within %s of the hub's deploy", j.Version, e.backWait.Round(time.Second))
		switch {
		case !found:
			why += " (not in the fleet any more)"
		case !hostUp(h):
			why += " (" + h.Status + ")"
		default:
			why += " (it runs " + orUnknown(h.Version) + ")"
		}
		e.skip(j, st, now, why)
		return true
	}
	j.Note = "waiting for " + st.Host + " (upgraded with the hub)"
	return false
}

// peersNotBack splits the peers an earlier wave finished that are not up and
// on the version now: old are reachable on an older version (gone back, a
// real sign of trouble), away are not reachable at all (a worker that
// powered off when idle, a laptop asleep: it was proven on the version when
// its step finished).
func (e *rolloutEngine) peersNotBack(j Rollout) (old, away []string) {
	for _, st := range j.Plan {
		if st.Wave == waveHub || st.Phase != roOK {
			continue
		}
		h, found := findHost(e.hosts, st.Host)
		switch {
		case !found || !hostUp(h):
			away = append(away, st.Host)
		case !(h.Current || (h.DryRun && h.Record != nil && h.Record.State == "dry_run")):
			old = append(old, st.Host+" ("+orUnknown(h.Version)+")")
		}
	}
	return old, away
}

func (e *rolloutEngine) beginHub(ctx context.Context, j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	if !found {
		e.fail(j, st, now, "the hub is not in the fleet", "")
		return false
	}
	if h.Current && !e.withHubPending(*j) {
		e.finishStep(j, st, h, now, "already on the version")
		return true
	}
	// The hub wave must not start over a peer that went back to an older
	// version: that is a sign of trouble, and it holds the step (5 minutes,
	// then it stops). A peer of an earlier wave that is only unreachable now
	// (a worker that powered off when idle, a laptop asleep) is treated as
	// the rest of the engine treats an offline host: it was proven on the
	// version when its step finished, and it does not hold the hub. A peer
	// the rollout skipped (offline, or failed after the canary) is not
	// counted at all (issue #141).
	old, away := e.peersNotBack(*j)
	if len(old) > 0 {
		if st.OfflineSince == nil {
			t := now
			st.OfflineSince = &t
		}
		if now.Sub(*st.OfflineSince) >= e.backWait {
			e.fail(j, st, now, "peers of the earlier waves went back to an older version: "+strings.Join(old, ", "), "")
			return false
		}
		st.Detail = "waiting for the peers to be back on " + j.Version + ": " + strings.Join(old, ", ")
		j.Note = st.Detail
		return false
	}
	st.OfflineSince = nil
	awayNote := ""
	if len(away) > 0 {
		awayNote = "peers not reachable now (upgraded earlier in this rollout): " + strings.Join(away, ", ")
	}
	cctx, cancel := context.WithTimeout(ctx, rolloutHostCallWait)
	defer cancel()
	// A deploy already running for this version is adopted, not started twice
	// (the hub went down between the start and the save).
	if ds, err := e.ops.DeployStatus(cctx); err == nil && ds.Record != nil && ds.Record.Version == j.Version && ds.Record.State == depRunning {
		e.hubStarted(j, st, ds.Record.Started, now, joinNote("adopted the deploy already running", awayNote))
		return false
	}
	// The hub's own restart would cut short what it is busy with (a sign-in
	// on the hub), the same as a peer's.
	wait, how := e.waitBusy(j, st, h, now)
	if wait {
		return false
	}
	started, err := e.ops.StartDeploy(cctx, *j)
	if err != nil {
		var pf *peerFault
		refused := errors.As(err, &pf) && pf.code != "deploy_busy" && pf.code != ""
		if st.TryingSince == nil {
			t := now
			st.TryingSince = &t
		}
		if refused || now.Sub(*st.TryingSince) >= e.startRetry {
			// Asked again by the loop once the release host answers with a
			// command (retryHub): it could not be reached, or had none. Any
			// other refusal (a build that is not green, say) is the user's.
			st.Reask = !refused || pf.code == "deploy_disabled"
			e.fail(j, st, now, "could not start the deploy: "+oneLine(err.Error(), 300), "")
			return false
		}
		st.Detail = "could not start the deploy yet: " + oneLine(err.Error(), 160)
		j.Note = st.Detail
		return false
	}
	e.hubStarted(j, st, parseDeployStart(started), now, joinNote(how, awayNote))
	st.BusyNote = how
	return false
}

// joinNote joins two notes of a step with " · ", either of them maybe empty.
func joinNote(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " · " + b
}

func parseDeployStart(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func (e *rolloutEngine) hubStarted(j *Rollout, st *RolloutStep, started, now time.Time, how string) {
	h, _ := findHost(e.hosts, st.Host)
	t := now
	st.Phase, st.Run, st.From, st.At, st.Started = roUpgrading, started.UTC().Format(time.RFC3339Nano), h.Version, now, &t
	st.TryingSince, st.BusySince, st.Detail = nil, nil, how
	j.Note = "deploying the hub"
	d := "deploy start from=" + h.Version + " run=" + st.Run
	if how != "" {
		d += " (" + how + ")"
	}
	e.emit(*j, st.Host, "rollout.step", true, d, "comebye")
}

// watchHub follows the deploy record. It also runs in a hub that was
// restarted by the deploy itself: nothing here is remembered.
func (e *rolloutEngine) watchHub(ctx context.Context, j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	j.Note = "deploying the hub"
	cctx, cancel := context.WithTimeout(ctx, rolloutHostCallWait)
	defer cancel()
	ds, err := e.ops.DeployStatus(cctx)
	if err == nil && ds.Record != nil && ds.Record.Version == j.Version {
		rec := ds.Record
		key := rec.Started.UTC().Format(time.RFC3339Nano)
		running := rec.State == depRunning
		switch {
		case key != st.Run && !running && rec.State != depOK:
			e.fail(j, st, now, "another deploy replaced this one ("+rec.State+")", rec.Output)
			return false
		case rec.State == depOK || rec.State == depDryRun:
			st.Dry = rec.State == depDryRun
			st.Run = key
			st.Phase, st.At, st.Detail = roVerifying, now, ""
			j.Note = "verifying the hub"
			return true
		case rec.State == depFailed:
			// A release host that is the hub itself marks a deploy that was cut
			// short by its own restart as failed: the hub's version decides.
			if strings.Contains(rec.Error, "restarted while it ran") && found && h.Current {
				st.Phase, st.At, st.Detail = roVerifying, now, "the deploy restarted this hub"
				return true
			}
			why := "the deploy failed"
			if rec.Headline != "" {
				why += ": " + oneLine(rec.Headline, 240)
			} else if rec.Error != "" {
				why += ": " + oneLine(rec.Error, 240)
			}
			e.fail(j, st, now, why, rec.Output)
			return false
		}
		st.Detail = "the deploy is running"
	} else if err != nil {
		st.Detail = "cannot read the deploy record: " + oneLine(err.Error(), 120)
	} else {
		st.Detail = "waiting for the deploy record"
	}
	if now.Sub(st.At) >= hubRunWait {
		e.fail(j, st, now, "no verdict from the deploy after "+hubRunWait.Round(time.Second).String(), "")
	}
	return false
}
