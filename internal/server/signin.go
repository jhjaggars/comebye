package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jhjaggars/comebye/internal/signin"
)

// Sign an agent in on this host.
//
//	POST   /api/hosts/{host}/signin            {agent}  start; answers the link
//	GET    /api/hosts/{host}/signin/{id}                state
//	POST   /api/hosts/{host}/signin/{id}/code  {code}   the pasted one-time code
//	DELETE /api/hosts/{host}/signin/{id}                cancel
//
// All four are the user's alone (s.auth): the agent role, a forwarded agent
// request and a tagged node get 403. The code is read from the body into one
// variable, written to the terminal and dropped; nothing here logs, stores or
// echoes a request body, and no error text carries one.

const signinMaxBody = 1 << 10

func signinError(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
}

func (s *Server) handleSigninStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent string `json:"agent"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, signinMaxBody)).Decode(&body); err != nil {
			signinError(w, http.StatusBadRequest, "bad_body", "invalid body")
			return
		}
	}
	if body.Agent == "" {
		body.Agent = "claude"
	}
	snap, err := s.signin.Start(r.Context(), body.Agent)
	var busy *signin.BusyError
	var fail *signin.FailError
	switch {
	case err == nil:
		writeJSON(w, snap)
	case errors.As(err, &busy):
		writeJSONStatus(w, http.StatusConflict, map[string]any{"code": "busy", "message": busy.Error(), "session": busy.Session})
	case errors.Is(err, signin.ErrUnknownAgent):
		signinError(w, http.StatusBadRequest, "unknown_agent", err.Error())
	case errors.Is(err, signin.ErrTooMany):
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]string{"code": "too_many", "message": err.Error()})
	case errors.As(err, &fail) && fail.Reason == signin.ReasonNotInstalled:
		signinError(w, http.StatusUnprocessableEntity, fail.Reason, fail.Error())
	case errors.As(err, &fail):
		signinError(w, http.StatusBadGateway, fail.Reason, fail.Error())
	case errors.Is(err, context.Canceled):
		signinError(w, http.StatusBadRequest, signin.ReasonCancelled, "cancelled")
	default:
		signinError(w, http.StatusInternalServerError, "failed", "could not start the sign-in")
	}
}

func (s *Server) handleSigninGet(w http.ResponseWriter, r *http.Request) {
	snap, err := s.signin.Get(r.PathValue("id"))
	if err != nil {
		signinError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, snap)
}

func (s *Server) handleSigninCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, signinMaxBody)).Decode(&body); err != nil {
		signinError(w, http.StatusBadRequest, "bad_body", "invalid body")
		return
	}
	snap, err := s.signin.SendCode(r.PathValue("id"), body.Code)
	body.Code = ""
	switch {
	case err == nil:
		writeJSON(w, snap)
	case errors.Is(err, signin.ErrBadCode):
		signinError(w, http.StatusBadRequest, "bad_code", err.Error())
	case errors.Is(err, signin.ErrNotFound):
		signinError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, signin.ErrNotWaiting), errors.Is(err, signin.ErrNoCodeInput):
		signinError(w, http.StatusConflict, "not_waiting", err.Error())
	default:
		signinError(w, http.StatusInternalServerError, "failed", "could not send the code")
	}
}

func (s *Server) handleSigninCancel(w http.ResponseWriter, r *http.Request) {
	snap, err := s.signin.Cancel(r.PathValue("id"))
	if err != nil {
		signinError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, snap)
}
