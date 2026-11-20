package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// Waking a stopped worker because it is used (issue #66, slice 3).
//
// Sending a prompt, launching an agent, resuming a parked agent, queueing a
// prompt and a handoff to a stopped worker start it; opening a session does
// not (the app shows a Start banner). Only the user, or the coordinator, may
// do it: an agent's own call gets a refusal that says whom to ask. The rate
// limits of wake.go apply (a tap on Start is the only way past them), and a
// budget-capped host is never started here.
//
// The answer to a call that started a wake is 202 {code: host_waking}: a boot
// takes minutes and a phone drops long requests, so the hub does not hold
// the request. The app shows "starting kv1…" and repeats the action once the
// host is up (GET /api/provision/wake). A handoff is already a long call on
// the hub, so it waits inside.

// wakeAuto starts host if it is a stopped provisioned worker (or joins the
// start under way), as an automatic wake: the budget cap and the destroy
// timer are never confirmed here, and the hourly limits apply. wakeNone means
// it is not a stopped provisioned worker: nothing to wake. veto, when set, is
// the refusal for a caller who may not wake workers; it applies once the
// host is known to be stopped. why names the cause in the wake book, and
// startDetail is what the audit line says when a start is made.
func (s *Server) wakeAuto(ctx context.Context, host, why, by, todo, veto, refuseTag, startDetail string) wakeVerdict {
	if s.wake == nil || !provision.ValidName(host) {
		return wakeVerdict{Kind: wakeNone}
	}
	if cur, ok := s.wake.get(host); ok && cur.State == wakeStarting {
		if veto != "" {
			return wakeVerdict{Kind: wakeRefused, Reason: veto, Code: "agent_forbidden"}
		}
		return wakeVerdict{Kind: wakeStarting, Step: cur.Step} // one wake per host: join it
	}
	owner, hv, amb := s.ownerOf(ctx, host, "")
	if owner == "" || amb || hv == nil || hv.State != "stopped" {
		return wakeVerdict{Kind: wakeNone} // not provisioned, or not stopped: a network fault is not ours to fix
	}
	refuse := func(code, msg string) wakeVerdict {
		s.auditBy(ledger.AuditEvent{By: by, Action: "provision.start_refused", Todo: todo, Host: owner, Detail: host + " " + refuseTag + ": " + oneLine(msg, 160)})
		return wakeVerdict{Kind: wakeRefused, Reason: msg, Code: code}
	}
	if veto != "" {
		return refuse("agent_forbidden", veto)
	}
	if st := s.peerStatus(owner); st != "ok" {
		return refuse("provisioner_offline", owner+" is offline, and it is the node that can start "+host)
	}
	if hv.CapStopped != "" {
		return refuse("needs_confirm", host+" was stopped by its budget cap: start it from Peers")
	}
	if t, err := time.Parse(time.RFC3339, hv.DestroyDue); err == nil && !time.Now().Before(t) {
		return refuse("needs_confirm", host+" is due to be destroyed: start it from Peers if you want it")
	}
	if err := s.wake.wakeAllow(host); err != nil {
		return refuse("wake_limit", err.Error())
	}
	e, joined := s.wake.begin(host, owner, by, why)
	if !joined {
		s.wake.noteAuto(host)
		s.auditBy(ledger.AuditEvent{By: by, Action: "provision.start", Todo: todo, OK: true, Host: owner, Detail: host + " " + startDetail})
		go s.wakeRun(e.Host, owner, false, by, false)
	}
	v, _ := s.wake.get(host)
	return wakeVerdict{Kind: wakeStarting, Step: v.Step}
}

// wakeVetoFor is the refusal for a caller that may not wake a worker: any
// agent but the coordinator. "" when it may.
func (s *Server) wakeVetoFor(c caller, host string) string {
	if c.agent() && !s.callerIsCoordinator(c) {
		return host + " is stopped; ask the coordinator (or the user) to start it"
	}
	return ""
}

// wakeEnsure is the one call every routing point makes: wake.Ensure(host,
// reason, by). A host that is up, or not a stopped worker, gives wakeNone and
// the caller carries on as before.
func (s *Server) wakeEnsure(ctx context.Context, r *http.Request, host, reason string) wakeVerdict {
	if s.wake == nil || host == s.fleet.LocalName() || !s.fleet.HasPeer(host) || s.peerStatus(host) == "ok" {
		return wakeVerdict{Kind: wakeNone}
	}
	by := s.by(r, "")
	return s.wakeAuto(ctx, host, reason, by, "", s.wakeVetoFor(callerOf(r), host), "for "+reason, "started for "+reason+" (it was stopped)")
}

// wakeAnswerFor writes the answer to a call that must wait for a wake (202
// host_waking), or a refusal (409). It reports whether it wrote one.
func (s *Server) wakeAnswerFor(w http.ResponseWriter, host string, v wakeVerdict) bool {
	switch v.Kind {
	case wakeStarting:
		wv, _ := s.wake.get(host)
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"code": "host_waking", "host": host, "wake": wv,
			"message": "starting " + host + "…: it was stopped. Try again when it is up."})
		return true
	case wakeRefused:
		code := v.Code
		if code == "" {
			code = "host_stopped"
		}
		st := http.StatusConflict
		if code == "agent_forbidden" {
			st = http.StatusForbidden
		}
		writeJSONStatus(w, st, map[string]any{"code": code, "host": host, "stopped": true, "message": v.Reason})
		return true
	}
	return false
}

// wakeGate is the check at a routing point that acts on a host: it answers
// the request itself when the host is stopped and returns true.
func (s *Server) wakeGate(w http.ResponseWriter, r *http.Request, host, reason string) bool {
	if s.wake == nil || host == s.fleet.LocalName() {
		return false
	}
	return s.wakeAnswerFor(w, host, s.wakeEnsure(r.Context(), r, host, reason))
}

// wakeWait waits for the wake of host to end, for a call that is long anyway
// (a handoff). nil when the host is up.
func (s *Server) wakeWait(ctx context.Context, host string) error {
	b := s.wake
	limit := time.Now().Add(wakeJobWait + b.joinWait + b.busyWait)
	for {
		v, ok := b.get(host)
		switch {
		case ok && v.State == wakeUp && s.peerStatus(host) == "ok":
			return nil
		case ok && v.State == wakeFailed:
			return &wakeError{host: host, msg: "could not start " + host + ": " + v.Reason}
		case !ok:
			return &wakeError{host: host, msg: "the start of " + host + " was lost"}
		case time.Now().After(limit):
			return &wakeError{host: host, msg: host + " did not start in time"}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.pollGap):
		}
	}
}

type wakeError struct{ host, msg string }

func (e *wakeError) Error() string { return e.msg }

// wakeForHandoff wakes the host a handoff names and waits for it. It answers
// (status, payload) when the handoff must not go on.
func (s *Server) wakeForHandoff(ctx context.Context, r *http.Request, host string) (int, any, bool) {
	v := s.wakeEnsure(ctx, r, host, "a handoff")
	switch v.Kind {
	case wakeRefused:
		code := v.Code
		if code == "" {
			code = "host_stopped"
		}
		st := http.StatusConflict
		if code == "agent_forbidden" {
			st = http.StatusForbidden
		}
		return st, map[string]any{"code": code, "host": host, "stopped": true, "message": v.Reason}, true
	case wakeStarting:
		if err := s.wakeWait(ctx, host); err != nil {
			return http.StatusBadGateway, map[string]any{"code": "host_wake_failed", "host": host, "stopped": true, "message": err.Error()}, true
		}
	}
	return 0, nil, false
}

// ---- the session may not have survived the power-off ----

// wakeSessionGrace is how long after a host joined the hub looks for the
// sessions its queued prompts wait for (wakeBook.sessionGrace, which tests shorten).
const wakeSessionGrace = 45 * time.Second

// afterWake runs when a wake ends up: prompts queued for sessions on that
// host that are not in the fleet once it has settled are given up with the
// reason (the power-off ended the session), and their senders are told.
func (s *Server) afterWake(host string) {
	if s.pq == nil {
		return
	}
	grace := s.wake.sessionGrace
	go func() { // like the wake itself, not counted in s.bg: a wake outlives the request that began it
		time.Sleep(grace)
		s.settleGoneAfterWake(host)
	}()
}

func (s *Server) settleGoneAfterWake(host string) {
	agents := s.fleet.Agents()
	now := s.pq.now()
	for _, it := range s.pq.list("", false) {
		if it.State != QueuePending || !strings.HasPrefix(it.Agent, host+"/") {
			continue
		}
		if pr, ok := parkLookup(s.parkSnapshot(), it.Agent, it.Name); ok && pr.Rec.Parked() {
			continue // parked: it waits for its resume
		}
		if a, _ := findAgent(agents, it.Agent, it.Name); a != nil {
			continue
		}
		p, err := s.pq.settle(it.ID, QueueGone, true)
		if err != nil {
			logQueueErr(err)
			continue
		}
		s.sendNotice(agents, p, now)
	}
}

// sessionGoneWriter turns the host's "unknown agent" answer to a call made
// just after a wake into the plain explanation: the power-off ended the
// session.
type sessionGoneWriter struct {
	http.ResponseWriter
	msg     string
	parkID  string
	host    string
	swapped bool // the 404 was replaced
	sent    bool // a header went out as it was
}

func (g *sessionGoneWriter) WriteHeader(code int) {
	if code == http.StatusNotFound && !g.sent {
		g.swapped = true
		h := g.ResponseWriter.Header()
		h.Set("Content-Type", "application/json")
		h.Del("Content-Length")
		g.ResponseWriter.WriteHeader(http.StatusGone)
		body := map[string]any{"code": "session_gone", "host": g.host, "message": g.msg}
		if g.parkID != "" {
			body["park_id"] = g.parkID
		}
		_ = json.NewEncoder(g.ResponseWriter).Encode(body)
		return
	}
	g.sent = true
	g.ResponseWriter.WriteHeader(code)
}

func (g *sessionGoneWriter) Write(b []byte) (int, error) {
	if g.swapped {
		return len(b), nil
	}
	g.sent = true
	return g.ResponseWriter.Write(b)
}

func (g *sessionGoneWriter) Flush() {
	if f, ok := g.ResponseWriter.(http.Flusher); ok && !g.swapped {
		f.Flush()
	}
}

// recentlyWoke: the host came up from a wake of ours in the last while.
func (s *Server) recentlyWoke(host string) bool {
	v, ok := s.wake.get(host)
	if !ok || v.State != wakeUp {
		return false
	}
	t, err := time.Parse(time.RFC3339, v.Updated)
	return err == nil && time.Since(t) < 20*time.Minute
}

// sessionGone wraps w when the host woke recently, so that an unknown agent
// is said plainly.
func (s *Server) sessionGone(w http.ResponseWriter, host, target string) http.ResponseWriter {
	if s.wake == nil || !s.recentlyWoke(host) {
		return w
	}
	g := &sessionGoneWriter{ResponseWriter: w, host: host}
	g.msg = target + " is gone: " + host + " was powered off and its session did not survive. Launch it again"
	if pr, ok := s.parkedTarget(host+"/"+target, target); ok {
		g.parkID = pr.Rec.ID
		g.msg = target + " is not running: " + host + " was powered off. It is parked: resume it"
	}
	return g
}
