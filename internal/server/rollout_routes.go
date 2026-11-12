package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The rollout's routes. The user alone, with the page's CSRF token, naming
// what the page showed (version, commit and the ordered hosts), so a reload
// between the click and the confirm cannot start something different.
// An agent can never start one.
//
//	POST /api/release/rollout          {todo, version, commit, hosts: [...]}
//	POST /api/release/rollout/pause    {id}
//	POST /api/release/rollout/resume   {id}   (also: retry the failed step)
//	POST /api/release/rollout/cancel   {id}
//
// The job itself is part of GET /api/releases (rollout_job).

type rolloutStartBody struct {
	Todo    string   `json:"todo"`
	Version string   `json:"version"`
	Commit  string   `json:"commit"`
	Hosts   []string `json:"hosts"`
}

func (s *Server) rolloutOff(w http.ResponseWriter) bool {
	if s.rollout == nil {
		relErr(w, http.StatusConflict, "rollout_disabled", "a rollout runs on the host that keeps the ledger, with -release-repo or -release-host set")
		return true
	}
	return false
}

// rolloutLive: a rollout is running, paused or stopped. Manual upgrades are
// refused meanwhile, so a click and the loop never fight over a host.
func (s *Server) rolloutLive() bool {
	if s.rollout == nil {
		return false
	}
	j, ok := s.rollout.Get()
	return ok && j.live()
}

func (s *Server) handleRolloutStart(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) || s.todoOff(w, r) || s.rolloutOff(w) {
		return
	}
	var b rolloutStartBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	it, ok := s.releaseItem(w, b.Todo)
	if !ok {
		return
	}
	if it.Release.State != relTagged || it.Release.Dismissed {
		relErr(w, http.StatusConflict, "not_released", "this release has not been tagged, or was dismissed")
		return
	}
	if b.Version != it.Release.Version || strings.ToLower(b.Commit) != it.Release.Commit {
		relErr(w, http.StatusConflict, "confirm_mismatch", "the version and commit you confirmed are not this item's: reload the page")
		return
	}
	if len(b.Hosts) == 0 {
		relErr(w, http.StatusBadRequest, "no_hosts", "name the hosts, in the order they are to be upgraded")
		return
	}
	if s.cfg.Release.Tool == nil {
		relErr(w, http.StatusConflict, "release_disabled", "this hub cannot check the release build: start comebye with -release-repo or -release-host")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if bd, err := s.relBuild(ctx, it, 0); err != nil || bd.State != "success" {
		why := "the release build is " + bd.State
		if err != nil {
			why = err.Error()
		}
		relErr(w, http.StatusConflict, "build_not_green", why)
		return
	}
	if cur, ok := s.rollout.Get(); ok && cur.live() {
		relErr(w, http.StatusConflict, "rollout_active", "a rollout of "+cur.Version+" is already in progress: finish or cancel it first")
		return
	}
	hosts := s.rollout.ops.Hosts(ctx, it.Release.Version)
	plan, err := planRollout(hosts, b.Hosts, "")
	if err != nil {
		relErr(w, http.StatusConflict, "bad_plan", err.Error())
		return
	}
	if st, err := s.deployStatusFor(ctx); err == nil && st.Record != nil && st.Record.State == depRunning {
		relErr(w, http.StatusConflict, "deploy_running", "a deploy is running: wait for it to finish")
		return
	}
	job, err := s.rollout.Start(it.ID, it.Release.Version, it.Release.Commit, s.by(r, ""), plan)
	switch {
	case errors.Is(err, errRolloutActive):
		relErr(w, http.StatusConflict, "rollout_active", err.Error())
		return
	case err != nil:
		relErr(w, http.StatusInternalServerError, "rollout_error", err.Error())
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"rollout": job})
}

type rolloutCtlBody struct {
	ID string `json:"id"`
}

func (s *Server) handleRolloutControl(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.rolloutControl(op, w, r) }
}

func (s *Server) rolloutControl(op string, w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) || s.todoOff(w, r) || s.rolloutOff(w) {
		return
	}
	var b rolloutCtlBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	cur, ok := s.rollout.Get()
	if !ok {
		relErr(w, http.StatusNotFound, "no_rollout", "there is no rollout")
		return
	}
	if b.ID != cur.ID {
		relErr(w, http.StatusConflict, "confirm_mismatch", "that is not the current rollout: reload the page")
		return
	}
	by := s.by(r, "")
	var job Rollout
	var err error
	switch op {
	case "pause":
		job, err = s.rollout.Pause(by)
	case "resume":
		job, err = s.rollout.Resume(by)
	case "cancel":
		job, err = s.rollout.Cancel(by)
	default:
		http.NotFound(w, r)
		return
	}
	switch {
	case errors.Is(err, errRolloutState):
		relErr(w, http.StatusConflict, "bad_state", "the rollout is "+job.State+": that does not apply")
		return
	case err != nil:
		relErr(w, http.StatusInternalServerError, "rollout_error", err.Error())
		return
	}
	writeJSON(w, map[string]any{"rollout": job})
}

// rolloutView adds the rollout to a tagged release's view: the job when it
// is this release's, and otherwise the plan the confirm would show.
func (s *Server) rolloutView(out map[string]any, it Todo, hosts []relHost) {
	if s.rollout == nil || s.cfg.Release.Tool == nil {
		return
	}
	out["rollout_enabled"] = true
	job, has := s.rollout.Get()
	if has && job.Todo == it.ID {
		out["rollout_job"] = job
		// A rollout in progress is the hub's work: it needs the user only when it stopped or waits for them.
		switch job.State {
		case roRunning:
			out["needs_you"], out["needs_text"] = false, "Rolling out "+job.Version+": "+job.Note
		case roPaused:
			out["needs_you"], out["needs_text"] = true, "Rollout of "+job.Version+" is paused"
		case roStopped:
			why := ""
			if job.Stop != nil {
				why = " at " + job.Stop.Host + ": " + oneLine(job.Stop.Reason, 120)
			}
			out["needs_you"], out["needs_text"] = true, "Rollout of "+job.Version+" stopped"+why
		}
	}
	if has && job.live() {
		return
	}
	plan, err := planRollout(hosts, nil, s.cfg.Release.Canary)
	if err != nil {
		out["rollout_plan_error"] = err.Error()
		return
	}
	out["rollout_plan"] = plan
}
