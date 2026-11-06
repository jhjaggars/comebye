package server

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/jhjaggars/comebye/internal/fleet"
)

// The peer registry API (the user's alone, like /keys: the agent role is
// refused by the route's guard). Peers added here are health-checked and
// streamed at once, with no restart.

func (s *Server) notHub(w http.ResponseWriter) bool {
	if s.fleet.IsHub() {
		return false
	}
	writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "not_a_hub", "message": "this instance is not a hub: start it with -hub-mode (COMEBYE_HUB_MODE=1) or -peers to manage peers"})
	return true
}

// handlePeers is GET /api/peers: every peer, static and dynamic.
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	writeJSON(w, map[string]any{"peers": s.fleet.List(), "version": s.cfg.Version})
}

// handleAddPeer is POST /api/peers {name, urls[], tag?}.
func (s *Server) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	var req struct {
		Name string   `json:"name"`
		URLs []string `json:"urls"`
		Tag  string   `json:"tag"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	info, err := s.fleet.Add(req.Name, req.URLs, requester(r, ""), req.Tag)
	if err != nil {
		s.peerError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, info)
}

// handleSetInstance is PUT /api/peers/{name}/instance {instance}: `worker
// create` records the cloud's instance id for a peer that announced itself
// (the user's alone: auth refuses the agent role and a tagged node, so a
// worker cannot name its own or anyone's incarnation). A different value ends
// the old incarnation: its unfinished ledger entries become lost.
func (s *Server) handleSetInstance(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	var req struct {
		Instance string `json:"instance"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	info, err := s.fleet.SetInstance(r.PathValue("name"), strings.TrimSpace(req.Instance), requester(r, ""))
	if err != nil {
		s.peerError(w, err)
		return
	}
	writeJSON(w, info)
}

// handleRemovePeer is DELETE /api/peers/{name}.
func (s *Server) handleRemovePeer(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	if err := s.fleet.Remove(r.PathValue("name"), requester(r, "")); err != nil {
		s.peerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) peerError(w http.ResponseWriter, err error) {
	var inv fleet.InvalidError
	code, status := "peer_error", http.StatusInternalServerError
	switch {
	case errors.As(err, &inv):
		code, status = "invalid_peer", http.StatusBadRequest
	case errors.Is(err, fleet.ErrStaticPeer):
		code, status = "static_peer", http.StatusConflict
	case errors.Is(err, fleet.ErrExists):
		code, status = "peer_exists", http.StatusConflict
	case errors.Is(err, fleet.ErrLocalName):
		code, status = "peer_is_local", http.StatusConflict
	case errors.Is(err, fleet.ErrNotFound):
		code, status = "unknown_peer", http.StatusNotFound
	case errors.Is(err, fleet.ErrNotHub):
		code, status = "not_a_hub", http.StatusConflict
	case errors.Is(err, fleet.ErrNoStore):
		code, status = "no_registry", http.StatusServiceUnavailable
	}
	writeJSONStatus(w, status, map[string]string{"code": code, "message": err.Error()})
}

// hostInstance is the current incarnation of a peer host ("" for this host
// and for static peers).
func (s *Server) hostInstance(host string) string {
	if s.fleet == nil {
		return ""
	}
	return s.fleet.Instances()[host]
}

// trackInstances ties the ledger to the peers' incarnations: entries from
// before instances get the one their host has now, and a host removed or
// replaced settles its unfinished entries as lost.
func (s *Server) trackInstances() {
	if s.ledger == nil || s.fleet == nil {
		return
	}
	if n, err := s.ledger.Backfill(s.fleet.Instances()); err != nil {
		log.Printf("ledger: could not record hosts' instances: %v", err)
	} else if n > 0 {
		log.Printf("ledger: %d entries now follow their host's current instance", n)
	}
	s.fleet.OnInstanceEnd(func(host, instance, why string) {
		lost, err := s.ledger.LoseInstance(host, instance, why)
		if err != nil {
			log.Printf("ledger: %v", err)
		}
		for _, e := range lost {
			log.Printf("ledger: %s (%s) is lost: %s", e.ID, e.Branch, why)
		}
	})
}
