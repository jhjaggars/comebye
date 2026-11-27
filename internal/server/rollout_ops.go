package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// serverRolloutOps is the engine's real world: this server's fleet and the
// same start paths the per-host Upgrade button uses.
type serverRolloutOps struct{ s *Server }

func (o serverRolloutOps) Hosts(ctx context.Context, version string) []relHost {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hosts := o.s.relHostsCached(cctx)
	markRollout(hosts, version)
	return hosts
}

// StartUpgrade is handleHostUpgrade without the browser: the build must be
// green (asked afresh, as the button does), then this host starts the run or
// the peer is asked to. A refusal comes back as a *peerFault.
func (o serverRolloutOps) StartUpgrade(ctx context.Context, job Rollout, host string) (upgrade.Record, error) {
	s := o.s
	it, ok := s.todos.get(job.Todo)
	if !ok || it.Release == nil || it.Release.State != relTagged || it.Release.Version != job.Version {
		return upgrade.Record{}, &peerFault{host: host, code: "not_released", msg: "the release item is gone or is not this version"}
	}
	if s.cfg.Release.Tool == nil {
		return upgrade.Record{}, &peerFault{host: host, code: "release_disabled", msg: "this hub cannot check the release build"}
	}
	if bd, err := s.relBuild(ctx, it, relBuildTTL); err != nil {
		return upgrade.Record{}, fmt.Errorf("the release build could not be checked: %w", err)
	} else if bd.State != "success" {
		s.auditBy(ledger.AuditEvent{By: "comebye", Action: "upgrade.refused", Todo: it.ID, Host: host, Detail: job.Version + ": the release build is " + bd.State})
		return upgrade.Record{}, &peerFault{host: host, code: "build_not_green", msg: "the release build is " + bd.State}
	}
	if host == s.fleet.LocalName() {
		rec, code, msg := s.startUpgrade(job.By, job.Version, false)
		s.auditBy(ledger.AuditEvent{By: job.By, Action: "upgrade.request", Todo: it.ID, Host: host, OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s rollback=false rollout=%s -> %d", job.Version, job.ID, code)})
		if code != http.StatusAccepted {
			return upgrade.Record{}, &peerFault{host: host, code: msg[0], msg: msg[1]}
		}
		return rec, nil
	}
	inner, _ := json.Marshal(upgradeBody{Version: job.Version})
	code, body, err := s.fleet.CallHost(ctx, host, "release-upgrade", url.Values{}, inner)
	s.dropRelHosts()
	if err != nil {
		s.auditBy(ledger.AuditEvent{By: job.By, Action: "upgrade.request", Todo: it.ID, Host: host, Detail: job.Version + ": " + oneLine(err.Error(), 200)})
		return upgrade.Record{}, err
	}
	s.auditBy(ledger.AuditEvent{By: job.By, Action: "upgrade.request", Todo: it.ID, Host: host, OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s rollback=false rollout=%s -> %d", job.Version, job.ID, code)})
	if code != http.StatusAccepted {
		var e struct{ Code, Message string }
		_ = json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = fmt.Sprintf("answered %d", code)
		}
		return upgrade.Record{}, &peerFault{host: host, code: e.Code, msg: oneLine(e.Message, 300)}
	}
	var resp struct {
		Record upgrade.Record `json:"record"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Record.ID == "" {
		return upgrade.Record{}, fmt.Errorf("%s accepted the upgrade but sent no run id", host)
	}
	return resp.Record, nil
}

// rolloutNoteOf is the line a release item keeps about its rollout: why it
// stopped, which hosts were skipped, who cancelled it, or that it is on
// every host. Empty while it runs or is paused. It is also the body of the
// rollout's one note (tellRollout).
func rolloutNoteOf(j Rollout) string {
	switch j.State {
	case roStopped:
		if j.Stop != nil {
			for _, st := range j.Plan {
				if st.Host == j.Stop.Host && isHubStep(st) {
					// Roll out again leaves the hub wave out: the rollout's Retry, or the hub's Bump hub, retries it.
					return fmt.Sprintf("%s stopped at the hub (%s): %s: Retry the rollout, or bump the hub from its row in Peers", j.Version, j.Stop.Host, oneLine(j.Stop.Reason, 160))
				}
			}
			return fmt.Sprintf("%s stopped at %s: %s", j.Version, j.Stop.Host, oneLine(j.Stop.Reason, 160))
		}
		return j.Version + " stopped"
	case roDone:
		var skipped []string
		for _, st := range j.Plan {
			if st.Phase == roSkipped {
				skipped = append(skipped, st.Host)
			}
		}
		if hubSkipped(j) {
			// "Roll out again" never retries the hub: only its Bump hub does.
			return fmt.Sprintf("%s not rolled out to the hub: %s skipped (%s): bump the hub from its row in Peers", j.Version, strings.Join(skipped, ", "), skippedWhy(j))
		}
		if behind := behindOnly(j); behind != nil {
			// Stopped or offline: the catch-up upgrades them when they are back.
			return fmt.Sprintf("%s is on every host (%d behind until they're back: %s)", j.Version, len(behind), strings.Join(behind, ", "))
		}
		if len(skipped) > 0 {
			return fmt.Sprintf("%s rolled out; %s skipped (%s): roll it out again from Peers", j.Version, strings.Join(skipped, ", "), skippedWhy(j))
		}
		return j.Version + " is on every host"
	case roCancelled:
		if j.SupersededBy != "" {
			return fmt.Sprintf("%s rollout stopped and was superseded by %s, which rolls out instead", j.Version, j.SupersededBy)
		}
		return j.Version + " rollout cancelled: hosts keep the version they have"
	}
	return ""
}

func skippedWhy(j Rollout) string {
	for _, st := range j.Plan {
		if st.Phase == roSkipped {
			return st.Detail
		}
	}
	return ""
}

// noteRollout keeps the release item's note in step with the job: the item
// is what the user's to-do list and the Peers badge show.
func (s *Server) noteRollout(j Rollout) {
	if s.todos == nil || j.Todo == "" {
		return
	}
	note := rolloutNoteOf(j)
	_, err := s.todos.mutate(j.Todo, func(t *Todo) error {
		if t.Release == nil || t.Release.RolloutNote == note {
			return errNoChange
		}
		rel := *t.Release
		rel.RolloutNote = note
		t.Release = &rel
		return nil
	})
	if err != nil && err != errNoChange {
		log.Printf("rollout: note on %s: %v", j.Todo, err)
	}
}

var errNoChange = errors.New("no change")

// tellRollout is the rollout's one note to the user (the engine's onEnd: the
// first time the job finished or stopped): a push notification, when the
// app has them, with the line the release item now shows. A stop has filed
// its To do already (the alarm); this adds no other.
func (s *Server) tellRollout(j Rollout) {
	body := rolloutNoteOf(j)
	if body == "" {
		return
	}
	title := j.Version + " rolled out"
	if j.State == roStopped {
		title = j.Version + " rollout stopped"
	} else if hubSkipped(j) || (behindOnly(j) == nil && skippedAny(j)) {
		title = j.Version + " rolled out, not everywhere"
	}
	s.auditBy(ledger.AuditEvent{By: "comebye", Action: "rollout.told", Todo: j.Todo, OK: j.State == roDone, Detail: j.ID + ": " + body})
	if s.rolloutPush != nil {
		s.rolloutPush(j.Todo, j.ID, title, body, j.State == roStopped)
	}
}

func skippedAny(j Rollout) bool {
	for _, st := range j.Plan {
		if st.Phase == roSkipped {
			return true
		}
	}
	return false
}

// DeployStatus is the deploy record, as the release host keeps it.
func (o serverRolloutOps) DeployStatus(ctx context.Context) (deployStatus, error) {
	st, err := o.s.deployStatusFor(ctx)
	if err == nil && st.Enabled {
		o.s.settleDeployAudit(st) // the hub wave's verdict is settled without the page open too
	}
	return st, err
}

// StartDeploy is handleReleaseDeploy without the browser: the build must be
// green, then the release host (this one, or the peer that owns the clone)
// starts the deploy command. It returns the record's start time, which names
// the run.
func (o serverRolloutOps) StartDeploy(ctx context.Context, job Rollout) (string, error) {
	s := o.s
	cfg := s.cfg.Release
	it, ok := s.todos.get(job.Todo)
	if !ok || it.Release == nil || it.Release.State != relTagged || it.Release.Version != job.Version {
		return "", &peerFault{host: s.releaseHost(), code: "not_released", msg: "the release item is gone or is not this version"}
	}
	if cfg.Tool == nil {
		return "", &peerFault{host: s.releaseHost(), code: "release_disabled", msg: "this hub cannot check the release build"}
	}
	if bd, err := s.relBuild(ctx, it, relBuildTTL); err != nil {
		return "", fmt.Errorf("the release build could not be checked: %w", err)
	} else if bd.State != "success" {
		s.auditBy(ledger.AuditEvent{By: "comebye", Action: "deploy.refused", Todo: it.ID, Detail: job.Version + ": the release build is " + bd.State})
		return "", &peerFault{host: s.releaseHost(), code: "build_not_green", msg: "the release build is " + bd.State}
	}
	if cfg.Host == "" {
		rec, code, msg := s.startDeployLocal(job.By, it.ID, job.Version)
		s.auditBy(ledger.AuditEvent{By: job.By, Action: "deploy.request", Todo: it.ID, Host: s.releaseHost(), OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s rollout=%s -> %d", job.Version, job.ID, code)})
		if code != http.StatusAccepted {
			return "", &peerFault{host: s.releaseHost(), code: msg[0], msg: msg[1]}
		}
		return rec.Started.UTC().Format(time.RFC3339Nano), nil
	}
	inner, _ := json.Marshal(deployReq{Version: job.Version, Todo: it.ID, By: job.By})
	code, body, err := s.fleet.CallHost(ctx, cfg.Host, "release-deploy", url.Values{}, inner)
	s.dropRelHosts()
	if err != nil {
		s.auditBy(ledger.AuditEvent{By: job.By, Action: "deploy.request", Todo: it.ID, Host: cfg.Host, Detail: job.Version + ": " + oneLine(err.Error(), 200)})
		return "", err
	}
	s.auditBy(ledger.AuditEvent{By: job.By, Action: "deploy.request", Todo: it.ID, Host: cfg.Host, OK: code == http.StatusAccepted, Detail: fmt.Sprintf("%s rollout=%s -> %d", job.Version, job.ID, code)})
	if code != http.StatusAccepted {
		var e struct{ Code, Message string }
		_ = json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = fmt.Sprintf("answered %d", code)
		}
		return "", &peerFault{host: cfg.Host, code: e.Code, msg: oneLine(e.Message, 300)}
	}
	var resp struct {
		Record DeployRecord `json:"record"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Record.Started.IsZero() {
		return "", fmt.Errorf("%s accepted the deploy but sent no record", cfg.Host)
	}
	return resp.Record.Started.UTC().Format(time.RFC3339Nano), nil
}

// Wake starts a stopped provisioned worker for a rollout (issue #66). It is
// an automatic wake: the budget cap and the destroy timer are never
// confirmed here, and the hourly limits apply. A host that is not a stopped
// provisioned worker is wakeNone, and the rollout keeps its offline grace.
func (o serverRolloutOps) Wake(ctx context.Context, job Rollout, host string) wakeVerdict {
	v := o.s.wakeAuto(ctx, host, "rollout", job.By, job.Todo, "", "for rollout "+job.ID, "started for rollout "+job.ID+" (it was stopped)")
	if v.Kind == wakeRefused {
		v.Reason = "not started: " + v.Reason
	}
	return v
}

// WakeStatus is the wake book's word on a host.
func (o serverRolloutOps) WakeStatus(_ context.Context, host string) wakeVerdict {
	if o.s.wake == nil {
		return wakeVerdict{Kind: wakeNone}
	}
	v, ok := o.s.wake.get(host)
	if !ok {
		return wakeVerdict{Kind: wakeNone}
	}
	return wakeVerdict{Kind: v.State, Step: v.Step, Reason: v.Reason}
}
