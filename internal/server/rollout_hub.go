package server

import (
	"context"
	"errors"
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
		if h.Current {
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

// peersBack: every peer an earlier wave finished is up and on the version
// (the hub wave must not start, or finish, over a peer that went away).
func (e *rolloutEngine) peersBack(j Rollout, hub *RolloutStep) bool {
	_, ok := e.peersMissing(j, hub)
	return ok
}

func (e *rolloutEngine) peersMissing(j Rollout, hub *RolloutStep) ([]string, bool) {
	var missing []string
	for _, st := range j.Plan {
		if st.Wave == waveHub || st.Phase != roOK {
			continue
		}
		h, found := findHost(e.hosts, st.Host)
		if !found || !hostUp(h) || !(h.Current || (h.DryRun && h.Record != nil && h.Record.State == "dry_run")) {
			missing = append(missing, st.Host)
		}
	}
	return missing, len(missing) == 0
}

func (e *rolloutEngine) beginHub(ctx context.Context, j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	if !found {
		e.fail(j, st, now, "the hub is not in the fleet", "")
		return false
	}
	if h.Current {
		e.finishStep(j, st, h, now, "already on the version")
		return true
	}
	if missing, ok := e.peersMissing(*j, st); !ok {
		if st.OfflineSince == nil {
			t := now
			st.OfflineSince = &t
		}
		if now.Sub(*st.OfflineSince) >= e.backWait {
			e.fail(j, st, now, "peers of the earlier waves are not back: "+strings.Join(missing, ", "), "")
			return false
		}
		st.Detail = "waiting for the peers to be back: " + strings.Join(missing, ", ")
		j.Note = st.Detail
		return false
	}
	st.OfflineSince = nil
	cctx, cancel := context.WithTimeout(ctx, rolloutHostCallWait)
	defer cancel()
	// A deploy already running for this version is adopted, not started twice
	// (the hub went down between the start and the save).
	if ds, err := e.ops.DeployStatus(cctx); err == nil && ds.Record != nil && ds.Record.Version == j.Version && ds.Record.State == depRunning {
		e.hubStarted(j, st, ds.Record.Started, now, "adopted the deploy already running")
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
			e.fail(j, st, now, "could not start the deploy: "+oneLine(err.Error(), 300), "")
			return false
		}
		st.Detail = "could not start the deploy yet: " + oneLine(err.Error(), 160)
		j.Note = st.Detail
		return false
	}
	e.hubStarted(j, st, parseDeployStart(started), now, "")
	return false
}

func parseDeployStart(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func (e *rolloutEngine) hubStarted(j *Rollout, st *RolloutStep, started, now time.Time, how string) {
	h, _ := findHost(e.hosts, st.Host)
	t := now
	st.Phase, st.Run, st.From, st.At, st.Started = roUpgrading, started.UTC().Format(time.RFC3339Nano), h.Version, now, &t
	st.TryingSince, st.Detail = nil, how
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
