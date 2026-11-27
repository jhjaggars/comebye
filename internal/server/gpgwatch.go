package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/gpgunlock"
	"github.com/jhjaggars/comebye/internal/herdr"
)

// The GPG watcher, inside the server (-gpg-watch; issue #143 slice 8). It
// replaces the `comebye gpg watch` daemon and its 30 s look at /api/agents:
// the server knows its own agents, so the watcher wakes when one of this
// host's agents starts or stops being active, when an unlock or a request
// lands here, and otherwise only when something is due (the unlock's 8 hour
// end, the standing item's renewal; see gpgunlock.Watcher.Run). With the
// feature off nothing runs.

// gpgOldUnit is the old daemon's systemd user unit.
const gpgOldUnit = "comebye-gpg-watch.service"

// gpgUnitDirs are where a systemd user unit is installed (Config.GPGUnitDirs
// in tests).
func (s *Server) gpgUnitDirs() []string {
	if s.cfg.GPGUnitDirs != nil {
		return s.cfg.GPGUnitDirs
	}
	var dirs []string
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		dirs = append(dirs, filepath.Join(d, "systemd", "user"))
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "systemd", "user"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "systemd", "user"))
	}
	return append(dirs, "/etc/systemd/user")
}

// gpgOldUnitWarning says, once at start, that the old daemon's unit is
// still installed, and how to remove it. "" when it is not.
func (s *Server) gpgOldUnitWarning() string {
	for _, d := range s.gpgUnitDirs() {
		p := filepath.Join(d, gpgOldUnit)
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		msg := "gpg watch: the old " + gpgOldUnit + " is still installed (" + p + "); the server runs the GPG watcher itself now, so remove it: systemctl --user disable --now " + gpgOldUnit + " && rm " + p + " && systemctl --user daemon-reload"
		if !s.cfg.GPGWatch {
			msg += "; first turn the server's watcher on (-gpg-watch, or COMEBYE_GPG_WATCH=1 in ~/.config/comebye/env) and restart comebye"
		}
		return msg
	}
	return ""
}

// gpgFloorWarning checks gpg-agent's max-cache-ttl, the floor that ends an
// unlock while comebye is down. `comebye gpg setup` writes it; the server
// does not, since reloading gpg-agent would forget an unlock in force. ""
// when the floor holds.
func gpgFloorWarning(h *gpgunlock.Host) string {
	life := h.Lifetime
	if life <= 0 {
		life = gpgunlock.DefaultLifetime
	}
	ttl, err := h.GPG.MaxCacheTTL()
	if err != nil {
		return "gpg watch: could not read gpg-agent's max-cache-ttl (" + err.Error() + "): if comebye stops, an unlock may outlive its 8 hours; set max-cache-ttl 28800 in gpg-agent.conf (comebye gpg setup does it)"
	}
	if ttl > life {
		return fmt.Sprintf("gpg watch: gpg-agent's max-cache-ttl is %s, longer than the unlock's %s: if comebye stops, an unlock outlives it; set max-cache-ttl %d in gpg-agent.conf and run gpgconf --reload gpg-agent (or run comebye gpg setup again)", ttl, life, int(life/time.Second))
	}
	return ""
}

// gpgActiveOn: some agent of this host is working or waiting on someone (an
// idle or finished one will not sign anything).
func gpgActiveOn(agents []herdr.AgentInfo, host string) bool {
	for _, a := range agents {
		if (a.Host == "" || sameHost(a.Host, host)) && (a.Status == herdr.StatusWorking || a.Status == herdr.StatusBlocked) {
			return true
		}
	}
	return false
}

// gpgPoke wakes the watcher for a round (it does nothing when the watcher
// is off). A wake already pending covers this one.
func (s *Server) gpgPoke() {
	s.gpgMu.Lock()
	ch := s.gpgWake
	s.gpgMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// gpgNotedAfter is how long after an approval route the watcher looks: the
// worker's `comebye gpg unlock` records its request once the hub answered.
var gpgNotedAfter = 2 * time.Second

// gpgNoted runs a route and then wakes the watcher: a worker's `comebye gpg
// unlock` (POST /api/approvals) filed an item the watcher renews, or a
// withdrawal changed what stands. It wraps the guard, so a peer's request
// forwarded to its hub counts too. Nothing happens with the watcher off.
func (s *Server) gpgNoted(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r)
		s.gpgMu.Lock()
		on := s.gpgWake != nil
		s.gpgMu.Unlock()
		if on {
			time.AfterFunc(gpgNotedAfter, s.gpgPoke)
		}
	}
}

// gpgCall asks the hub for an approval route as this host's agent role
// would: in process where this instance keeps the approvals, through the
// upstream link on a peer.
func (s *Server) gpgCall(ctx context.Context, segs []string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	local := s.fleet.LocalName()
	if s.approvals != nil {
		path := "/" + strings.Join(segs, "/")
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
		r = r.WithContext(context.WithValue(ctx, callerKey{}, caller{role: roleAgent, host: strings.ToLower(local)}))
		rec := httptest.NewRecorder()
		switch {
		case len(segs) == 2:
			s.handleApprovalCreate(rec, r)
		case len(segs) == 4 && segs[3] == "withdraw":
			r.SetPathValue("id", segs[2])
			s.handleApprovalWithdraw(rec, r)
		default:
			return 0, nil, errors.New("no such approval route")
		}
		return rec.Code, rec.Body.Bytes(), nil
	}
	if s.cfg.Upstream == nil {
		return 0, nil, errors.New("approvals live on the hub, and this instance has none (-hub)")
	}
	code, _, out, err := s.cfg.Upstream.Do(ctx, fleet.UpstreamRequest{
		Method: http.MethodPost, Segments: segs, Body: b, MaxBody: 16 << 10, Timeout: 30 * time.Second,
		Header: http.Header{HeaderForwarded: {"1"}, HeaderAgentHost: {local}},
	})
	return code, out, err
}

func gpgCallErr(code int, out []byte) error {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(out, &e)
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(out))
	}
	if len(e.Message) > 200 {
		e.Message = e.Message[:200]
	}
	return fmt.Errorf("the hub answered %d: %s", code, e.Message)
}

// gpgFile files the "GPG is locked" request for this host, as the watcher
// (auto: the hub may answer that the user said "Not now"). The request
// carries no secret: the host's one-use public key and its signature.
func (s *Server) gpgFile(ctx context.Context, h *gpgunlock.Host) (approval, request string, err error) {
	cfg, err := h.Config()
	if err != nil {
		return "", "", err
	}
	setup := !h.HasBlob()
	id, req, err := h.NewRequest(h.Pinned().RPID, setup)
	if err != nil {
		return "", "", err
	}
	action, subject := "gpg-unlock", "Unlock GPG key "+cfg.FPR
	if setup {
		action, subject = "gpg-setup", "Set up GPG unlock for key "+cfg.FPR
	}
	code, out, err := s.gpgCall(ctx, []string{"api", "approvals"}, map[string]any{
		"action": action, "subject": subject, // the host is the caller's: this one, by the hub's name
		"request": json.RawMessage(req), "auto": true,
	})
	if err != nil {
		h.DropPending(id)
		return "", "", err
	}
	var rep struct {
		Approval struct {
			ID string `json:"id"`
		} `json:"approval"`
		Snoozed bool      `json:"snoozed"`
		Until   time.Time `json:"until"`
	}
	if code/100 != 2 || json.Unmarshal(out, &rep) != nil {
		h.DropPending(id)
		return "", "", gpgCallErr(code, out)
	}
	if rep.Snoozed {
		h.DropPending(id)
		return "", "", &gpgunlock.SnoozedError{Until: rep.Until}
	}
	return rep.Approval.ID, id, nil
}

func (s *Server) gpgWithdraw(ctx context.Context, approval string) error {
	if approval == "" || strings.ContainsAny(approval, "/?#") {
		return nil
	}
	code, out, err := s.gpgCall(ctx, []string{"api", "approvals", approval, "withdraw"}, struct{}{})
	if err != nil {
		return err
	}
	if code == http.StatusNotFound { // gone already: nothing stands
		return nil
	}
	if code/100 != 2 {
		return gpgCallErr(code, out)
	}
	return nil
}

// RunGPGWatch runs this host's GPG watcher until ctx is done, where
// -gpg-watch turned it on. Off, it only says once that the old daemon's
// unit is still installed, and returns: no loop runs, nothing is watched.
func (s *Server) RunGPGWatch(ctx context.Context) {
	if msg := s.gpgOldUnitWarning(); msg != "" {
		log.Print(msg)
	}
	if !s.cfg.GPGWatch {
		return
	}
	h := s.gpgHost()
	if !h.GPG.Available() {
		log.Print("gpg watch: off: gpg, gpg-connect-agent and gpgconf are needed on this host")
		return
	}
	if _, err := h.Config(); errors.Is(err, gpgunlock.ErrNotSetUp) {
		log.Print("gpg watch: GPG unlock is not set up on this host yet (comebye gpg setup); the watcher waits for it")
	} else if msg := gpgFloorWarning(h); msg != "" {
		log.Print(msg)
	}
	wake := make(chan struct{}, 1)
	s.gpgMu.Lock()
	s.gpgWake = wake
	s.gpgMu.Unlock()
	defer func() {
		s.gpgMu.Lock()
		s.gpgWake = nil
		s.gpgMu.Unlock()
	}()

	local := s.fleet.LocalName()
	events, cancel := s.fleet.Hub().Subscribe()
	defer cancel()
	s.gpgActive.Store(gpgActiveOn(s.fleet.Agents(), local))
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev.Type != "agents" {
					continue
				}
				a := gpgActiveOn(ev.Agents, local)
				if s.gpgActive.Swap(a) != a {
					s.gpgPoke() // an agent here started or stopped being active
				}
			}
		}
	}()

	w := &gpgunlock.Watcher{
		Host:     h,
		File:     func() (string, string, error) { return s.gpgFile(ctx, h) },
		Withdraw: func(ap string) error { return s.gpgWithdraw(ctx, ap) },
		Active:   func() (bool, error) { return s.gpgActive.Load(), nil },
	}
	log.Printf("gpg watch: on: an unlock ends after %s; the \"GPG is locked\" item follows this host's agents", gpgLifetimeOf(h))
	w.Run(ctx, wake, func(err error) { log.Printf("gpg watch: %v", err) })
}

func gpgLifetimeOf(h *gpgunlock.Host) time.Duration {
	if h.Lifetime > 0 {
		return h.Lifetime
	}
	return gpgunlock.DefaultLifetime
}
