package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// Stopping a running worker from the app: the counterpart of Start (wake.go).
// A worker stops itself when idle; this is the user's Stop, disks kept.
//
//	POST /api/provision/hosts/{name}/stop   {via?}   the user, or the coordinator
//
// The node that can stop the host (the same choice as a start: pickOwner) runs
// `worker stop NAME` as a job. The hub keeps a job for it and the app follows
// it with GET /api/provision/jobs/{id}, as it does a destroy.

type stopBody struct {
	Via string `json:"via"`
}

// handleProvisionHostStop: POST /api/provision/hosts/{name}/stop. Only the
// user (with the page's CSRF token) or the coordinator may stop a worker:
// whatever runs on it stops too.
func (s *Server) handleProvisionHostStop(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	switch {
	case c.agent() && !s.callerIsCoordinator(c):
		relErr(w, http.StatusForbidden, "agent_forbidden", "only the user or the coordinator may stop a worker: ask the user to stop it")
		return
	case !c.agent() && !s.csrfOK(r):
		relErr(w, http.StatusForbidden, "csrf", "this page's token is missing or stale: reload the page")
		return
	}
	name := r.PathValue("name")
	var b stopBody
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			relErr(w, http.StatusBadRequest, "bad_request", "invalid body")
			return
		}
	}
	if !provision.ValidName(name) || !s.fleet.HasPeer(name) {
		relErr(w, http.StatusNotFound, "no_host", name+" is not a worker on this hub")
		return
	}
	if cur, ok := s.wake.get(name); ok && cur.State == wakeStarting {
		relErr(w, http.StatusConflict, "starting", name+" is being started: stop it once it is up")
		return
	}
	owner, hv, amb := s.ownerOf(r.Context(), name, b.Via)
	switch {
	case amb:
		relErr(w, http.StatusConflict, "ambiguous", "more than one node reports a host with this name: say which")
		return
	case owner == "":
		relErr(w, http.StatusNotFound, "not_provisioned", name+" is not a provisioned worker: no provisioner lists it")
		return
	}
	if st := s.peerStatus(owner); st != "ok" {
		relErr(w, http.StatusConflict, "provisioner_offline", owner+" is offline, and it is the node that can stop "+name+": try again when it is back")
		return
	}
	switch hv.State {
	case "running":
	case "stopped", "stopping":
		relErr(w, http.StatusConflict, "not_running", name+" is already "+hv.State)
		return
	default:
		relErr(w, http.StatusConflict, "bad_state", "the cloud says "+name+" is "+hv.State+", not running: it cannot be stopped now")
		return
	}
	by := s.by(r, "")
	code, body, err := s.callBody(r.Context(), owner, "provision-host-stop", map[string]any{"name": name, "why": "from the app: " + by})
	if err != nil {
		relErr(w, http.StatusBadGateway, "node_unreachable", oneLine(provision.Text(owner+" did not answer: "+err.Error(), 200), 200))
		return
	}
	if code != http.StatusAccepted && code != http.StatusOK {
		relayPeerError(w, code, body)
		return
	}
	pv, ok := parsePeerJob(body)
	if !ok {
		relErr(w, http.StatusBadGateway, "bad_answer", "the node's job was not understood")
		return
	}
	var idb [8]byte
	rand.Read(idb[:])
	j := &hubJob{ID: "hj_" + hex.EncodeToString(idb[:]), Key: "p" + hex.EncodeToString(idb[:]), Via: owner, PeerID: pv.ID, Spec: pv.Spec, Region: pv.Spec.Zone, Op: opStop,
		Created: s.hjobs.now(), By: by}
	applyPeerJob(j, pv)
	s.hjobs.mu.Lock()
	s.hjobs.jobs[j.ID] = j
	s.hjobs.saveLocked()
	s.hjobs.mu.Unlock()
	s.prov.mu.Lock()
	if c := s.prov.hosts[owner]; c != nil {
		c.At = time.Time{} // look again at once: it is stopping now
	}
	s.prov.mu.Unlock()
	s.audit(r, ledger.AuditEvent{Action: "provision.stop", OK: true, Host: owner, Detail: name + " stopped from the app, " + j.ID})
	s.hjobs.mu.Lock()
	v := s.hubView(j, 0, r)
	s.hjobs.mu.Unlock()
	writeJSONStatus(w, http.StatusAccepted, v)
}
