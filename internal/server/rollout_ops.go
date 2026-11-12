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
	hosts := o.s.relHosts(cctx)
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

// RunRollout drives the rolling rollout on a clock. Only the ledger host has
// the engine: a peer never runs a rollout.
func (s *Server) RunRollout(ctx context.Context) {
	if s.rollout == nil || s.fleet == nil || s.todos == nil {
		return
	}
	s.rollout.Run(ctx)
}

// rolloutNoteOf is the line a release item keeps about its rollout: why it
// stopped, which hosts were skipped, who cancelled it. Empty when there is
// nothing to say (running, paused, or done with every host).
func rolloutNoteOf(j Rollout) string {
	switch j.State {
	case roStopped:
		if j.Stop != nil {
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
		if len(skipped) > 0 {
			return fmt.Sprintf("%s rolled out; %s skipped (%s): roll it out again from Peers", j.Version, strings.Join(skipped, ", "), skippedWhy(j))
		}
	case roCancelled:
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

// DeployStatus is the deploy record, as the release host keeps it.
func (o serverRolloutOps) DeployStatus(ctx context.Context) (deployStatus, error) {
	return o.s.deployStatusFor(ctx)
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
