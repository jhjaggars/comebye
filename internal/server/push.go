package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/herdr-phone/internal/push"
)

// callerLogin is the Tailscale identity of the request, lowercased; empty
// without Tailscale Serve in front.
func callerLogin(r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(r.Header.Get("Tailscale-User-Login")))
}

// decodeBody decodes a small JSON body, answering 400 on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(v); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return false
	}
	return true
}

// pushEnabled answers 404 {code:"push_disabled"} when Web Push is off.
func (s *Server) pushEnabled(w http.ResponseWriter) bool {
	if s.cfg.Push == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "push_disabled", "message": "Web Push is disabled on this server"})
		return false
	}
	return true
}

// handlePushKey returns the VAPID public key for pushManager.subscribe.
func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Push == nil {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, map[string]any{"enabled": true, "key": s.cfg.Push.PublicKey()})
}

// subscribeBody is PushSubscription.toJSON() plus, from the service
// worker's pushsubscriptionchange handler, the endpoint it replaces.
type subscribeBody struct {
	Endpoint string    `json:"endpoint"`
	Keys     push.Keys `json:"keys"`
	Replaces string    `json:"replaces,omitempty"`
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.pushEnabled(w) {
		return
	}
	var body subscribeBody
	if !decodeBody(w, r, &body) {
		return
	}
	if err := push.ValidateEndpoint(body.Endpoint); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_endpoint", "message": err.Error()})
		return
	}
	sub := push.Subscription{
		Endpoint:  body.Endpoint,
		Keys:      body.Keys,
		Login:     callerLogin(r),
		UserAgent: truncate(r.UserAgent(), 200),
		Created:   time.Now().UTC(),
	}
	if err := sub.Validate(); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_keys", "message": err.Error()})
		return
	}
	store := s.cfg.Push.Store()
	if prev, ok := store.Get(sub.Endpoint); ok {
		// Like unsubscribe: another login may not take over (or re-key)
		// someone's subscription and redirect their notifications.
		if prev.Login != sub.Login {
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "not_owner", "message": "that subscription belongs to another login"})
			return
		}
		if prev.Keys == sub.Keys {
			sub.Created = prev.Created // an idempotent re-sync from the app
		}
	}
	if err := store.Put(sub); err != nil {
		if errors.Is(err, push.ErrStoreFull) {
			writeJSONStatus(w, http.StatusInsufficientStorage, map[string]string{"code": "too_many", "message": err.Error()})
			return
		}
		http.Error(w, "saving subscription failed", http.StatusInternalServerError)
		return
	}
	if body.Replaces != "" && body.Replaces != sub.Endpoint {
		if old, ok := store.Get(body.Replaces); ok && old.Login == sub.Login {
			_, _ = store.Delete(body.Replaces)
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

type endpointBody struct {
	Endpoint string `json:"endpoint"`
}

// handlePushUnsubscribe removes the caller's subscription for endpoint.
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.pushEnabled(w) {
		return
	}
	var body endpointBody
	if !decodeBody(w, r, &body) {
		return
	}
	store := s.cfg.Push.Store()
	sub, ok := store.Get(body.Endpoint)
	if !ok {
		writeJSON(w, map[string]any{"ok": true, "removed": false})
		return
	}
	if sub.Login != callerLogin(r) {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "not_owner", "message": "that subscription belongs to another login"})
		return
	}
	removed, err := store.Delete(body.Endpoint)
	if err != nil {
		http.Error(w, "removing subscription failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "removed": removed})
}

// handlePushTest sends a test notification to the caller's subscriptions
// (only the given endpoint when one is named) and reports the results.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if !s.pushEnabled(w) {
		return
	}
	var body endpointBody
	if !decodeBody(w, r, &body) {
		return
	}
	login := callerLogin(r)
	var targets []push.Subscription
	for _, sub := range s.cfg.Push.Store().List() {
		if sub.Login == login && (body.Endpoint == "" || sub.Endpoint == body.Endpoint) {
			targets = append(targets, sub)
		}
	}
	if len(targets) == 0 {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "not_subscribed", "message": "no push subscription for this device"})
		return
	}
	payload, _ := json.Marshal(push.Message{
		Title: "herdr test notification",
		Body:  "Notifications from herdr-phone are working.",
		URL:   "/",
		Tag:   "herdr-test",
	})
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	type result struct {
		Status int    `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	var results []result
	sent := 0
	for _, sub := range targets {
		code, err := s.cfg.Push.Send(ctx, sub, payload, push.Options{TTL: time.Minute, Urgency: "high"})
		res := result{Status: code}
		if err != nil {
			res.Error = err.Error()
		} else {
			sent++
		}
		results = append(results, res)
	}
	writeJSON(w, map[string]any{"sent": sent, "results": results})
}

type presenceBody struct {
	Client  string `json:"client"`
	Pane    string `json:"pane"`
	Visible bool   `json:"visible"`
}

// handlePresence records which agent this app instance is showing, for
// foreground suppression.
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	var body presenceBody
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Client == "" || len(body.Client) > 64 || len(body.Pane) > 256 {
		http.Error(w, "client is required (at most 64 chars)", http.StatusBadRequest)
		return
	}
	if s.cfg.Presence != nil {
		// Keyed per login so one user's tab cannot clear another's entry.
		s.cfg.Presence.Update(callerLogin(r)+"\x00"+body.Client, body.Pane, body.Visible)
	}
	w.WriteHeader(http.StatusNoContent)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
