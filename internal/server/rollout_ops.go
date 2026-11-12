package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
