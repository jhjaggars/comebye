// Package server exposes the phone web app: static UI, a small JSON API
// over herdr, and an SSE feed of agent state.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/push"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
	"github.com/jhjaggars/comebye/web"
)

// Config configures the HTTP server.
type Config struct {
	// Addr to listen on. Intended to be loopback only; exposure to phones
	// happens through Tailscale Serve, which provides TLS.
	Addr string
	// AllowLogins, when set, admits requests whose Tailscale-User-Login
	// header (set by Tailscale Serve) matches one of these lowercase logins.
	// Only valid with a loopback Addr, where Serve is the sole client.
	AllowLogins []string
	// InsecureNoAuth lets ListenAndServe run without AllowLogins, serving
	// an unauthenticated API to anyone who reaches it. Off by default.
	InsecureNoAuth bool
	// AllowHosts are extra Host names (lowercase, no port) the server
	// answers to besides localhost, loopback IPs and *.ts.net; any other
	// Host is refused as possible DNS rebinding (see hostAllowed).
	AllowHosts []string
	// ReadLines caps the ?lines= parameter for output reads.
	ReadLines int
	// Push, when set, enables the Web Push subscription endpoints.
	Push *push.Service
	// Presence, when set, records which agent each client is viewing so
	// notifications for it are suppressed.
	Presence *push.Presence
	// Fleet is the merged view of this host and its peers (and runs the
	// broadcasts). When nil, New builds a peerless one named HostName
	// (default "local") that nobody runs, which is enough for tests.
	Fleet *fleet.Fleet
	// HostName names the local host when Fleet is nil.
	HostName string
	// PingInterval is the SSE heartbeat interval (default 15s). Hubs use
	// the pings to tell that a peer is alive.
	PingInterval time.Duration
	// UploadDir is where images sent from the app are stored for agents
	// to read. Empty disables uploads.
	UploadDir string
	// LaunchRoots are the directories (absolute, symlinks allowed) inside
	// which the app may start new agents; ones that do not exist are
	// ignored. Empty turns launching off.
	LaunchRoots []string
	// WorktreeRoot is where workers' worktrees are created, as
	// <root>/<repo>/<branch> (default ~/.worktrees). Checks and removals
	// may touch it besides the launch roots.
	WorktreeRoot string
	// StateDir holds the handoff ledger (ledger.json), the briefing
	// (brief.json) and its schedule (brief-schedule.json), the roles (roles.json) and an optional
	// handoff-template.md. Empty turns handoffs and marks off.
	StateDir string
	// NoLedger turns the ledger (and the briefing and roles) off even with
	// a state directory: the instance is a peer that is not the hub.
	NoLedger bool
	// Upstream, on a peer, is its hub: the agent role's requests are
	// forwarded there, and the peer keeps no ledger of its own.
	Upstream *fleet.Upstream
	// AgentToken is the local agent token (see agenttoken); requests that
	// carry it get the agent role. Empty disables the agent role.
	AgentToken string
	// CoordinatorDir is where the "Coordinator" launch preset starts its
	// agent (default the home directory). It need not lie in a launch
	// root; only that preset may use it.
	CoordinatorDir string
	// CoordinatorArgs are extra arguments per agent kind that the
	// Coordinator preset appends to the preset's own (e.g. pi: --auto,
	// a flag only some Pi installs have).
	CoordinatorArgs map[string][]string
}

// Server is the phone app HTTP server.
type Server struct {
	cfg     Config
	client  *herdr.Client
	manager *state.Manager
	fleet   *fleet.Fleet
	mux     *http.ServeMux
	// version is this build's app version (a hash of the embedded web
	// assets). Pages compare it to the one they loaded with and reload
	// after a deploy.
	version string
	// streams counts open /api/events streams (see maxStreams).
	streams atomic.Int32
	// findExec reports whether an executable is installed (default
	// installed; tests replace it).
	findExec func(name string) bool
	// agentReadyPoll and agentReadyWait pace waitForNamedAgent, the wait
	// for a launched agent to become an active named agent (tests shrink
	// them).
	agentReadyPoll time.Duration
	agentReadyWait time.Duration
	// briefWait, briefTries and briefBackoff pace deliverPrompt, the
	// verified delivery of a first prompt (tests shrink them).
	briefWait    time.Duration
	briefTries   int
	briefBackoff time.Duration
	// askPoll and askWait pace asking a marked session whether it is safe
	// to remove (tests shrink them); latest, when set, replaces latestOf.
	askPoll time.Duration
	askWait time.Duration
	latest  func(ctx context.Context, host, pane string) *transcript.Latest
	// planPush tells the user a plan settled (the dispatcher's NotifyPlan;
	// tests replace it).
	planPush func(id, title, body string)
	// ledger records handoffs and marks (nil without a state directory,
	// and on a peer that is not the hub).
	ledger *ledger.Store
	// brief and roles live next to the ledger (nil when it is).
	brief      *briefStore
	pq         *promptQueue
	briefSched *briefSchedStore
	roles      *roleStore
	// kinds remembers the agent kinds that worked on this host (nil
	// without a state directory).
	kinds *kindStore
	// models caches each kind's model list; listModels, when set,
	// replaces the discovery (tests).
	models     modelCache
	listModels func(ctx context.Context, kind string) *modelCatalog
	coord      coord
}

// maxStreams caps concurrent SSE clients. A phone holds one or two and a
// hub one per peer; each stream costs a goroutine and a hub subscription.
const maxStreams = 64

// New builds the server. manager is the local host's state manager.
func New(cfg Config, client *herdr.Client, manager *state.Manager) *Server {
	f := cfg.Fleet
	if f == nil {
		name := cfg.HostName
		if name == "" {
			name = "local"
		}
		f = fleet.New(manager, name, nil, fleet.Options{})
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 15 * time.Second
	}
	s := &Server{cfg: cfg, client: client, manager: manager, fleet: f, mux: http.NewServeMux(),
		agentReadyPoll: 200 * time.Millisecond, agentReadyWait: launchStartTimeout,
		briefWait: 20 * time.Second, briefTries: 3, briefBackoff: 3 * time.Second, askPoll: askPoll, askWait: askWait}
	s.coord = coord{checks: map[string]cachedChecks{}, checking: map[string]bool{}, reporting: map[string]time.Time{}, starting: map[string]bool{}, started: map[string]time.Time{}, quiet: map[string]time.Time{}, calling: map[string]time.Time{}}
	if cfg.StateDir != "" {
		s.kinds = openKinds(kindsPath(cfg.StateDir))
	}
	if cfg.StateDir != "" && !cfg.NoLedger && cfg.Upstream == nil {
		l, err := openLedger(cfg.StateDir)
		if err != nil {
			log.Printf("handoff ledger disabled: %v", err)
		} else {
			s.ledger = l
			s.brief = &briefStore{path: filepath.Join(cfg.StateDir, "brief.json")}
			if s.briefSched, err = openBriefSched(filepath.Join(cfg.StateDir, "brief-schedule.json")); err != nil {
				log.Printf("brief schedule: %v (starting with none)", err)
				s.briefSched = &briefSchedStore{path: filepath.Join(cfg.StateDir, "brief-schedule.json")}
			}
			if s.roles, err = openRoles(filepath.Join(cfg.StateDir, "roles.json")); err != nil {
				log.Printf("roles: %v (starting with none)", err)
				s.roles = &roleStore{path: filepath.Join(cfg.StateDir, "roles.json"), data: map[string]Role{}}
			}
			if s.pq, err = openPromptQueue(queuePath(cfg.StateDir)); err != nil {
				log.Printf("prompt queue: %v (starting empty)", err)
				s.pq = newPromptQueue("") // in memory: never overwrite the unreadable file
			}
			s.endStaleAsks()
		}
	}
	if manager != nil {
		manager.Dispatcher().Quiet = s.quietAgent
		s.planPush = manager.Dispatcher().NotifyPlan
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	static, err := newStaticHandler(web.FS)
	if err != nil {
		log.Fatalf("static assets: %v", err)
	}
	s.version = static.version
	s.mux.Handle("GET /", static)
	// Routes the agent role may use are wrapped in authAgent (forwarded to
	// the hub on a peer with -hub) or authAgentLocal; everything wrapped
	// in auth is the user's alone (see agentauth.go).
	s.mux.HandleFunc("GET /api/version", s.authAgentLocal(s.handleVersion))
	s.mux.HandleFunc("GET /api/whoami", s.authAgentLocal(s.handleWhoami))
	s.mux.HandleFunc("GET /api/playbook", s.authAgentLocal(s.handlePlaybook))
	s.mux.HandleFunc("GET /api/agents", s.authAgent(s.handleAgents))
	s.mux.HandleFunc("GET /api/agents/{target}/output", s.authAgent(s.handleOutput))
	s.mux.HandleFunc("GET /api/agents/{target}/transcript", s.authAgent(s.handleTranscript))
	s.mux.HandleFunc("GET /api/agents/{target}/subagents/{sub}/transcript", s.authAgent(s.handleSubagentTranscript))
	s.mux.HandleFunc("POST /api/agents/{target}/prompt", s.authAgent(s.selfGuard(s.handlePrompt)))
	s.mux.HandleFunc("POST /api/agents/{target}/keys", s.auth(s.handleKeys))
	s.mux.HandleFunc("GET /api/agents/{target}/dialog", s.auth(s.handleDialog))
	s.mux.HandleFunc("GET /api/agents/{target}/activity", s.auth(s.handleActivity))
	s.mux.HandleFunc("POST /api/agents/{target}/answer", s.auth(s.handleAnswer))
	s.mux.HandleFunc("POST /api/agents/{target}/image", s.auth(s.handleImage))
	// Host-qualified routes: the local host is served directly, a peer's
	// agents are proxied to that peer (hub only).
	s.mux.HandleFunc("GET /api/hosts", s.authAgent(s.handleHosts))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/output", s.authAgent(s.onHost("output", s.handleOutput)))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/transcript", s.authAgent(s.onHost("transcript", s.handleTranscript)))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/subagents/{sub}/transcript", s.authAgent(s.onHost("subagent-transcript", s.handleSubagentTranscript)))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/prompt", s.authAgent(s.selfGuard(s.onHost("prompt", s.handlePrompt))))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/keys", s.auth(s.onHost("keys", s.handleKeys)))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/dialog", s.auth(s.onHost("dialog", s.handleDialog)))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/activity", s.auth(s.onHost("activity", s.handleActivity)))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/answer", s.auth(s.onHost("answer", s.handleAnswer)))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/image", s.auth(s.onHost("image", s.handleImage)))
	s.mux.HandleFunc("GET /api/launch/options", s.auth(s.handleLaunchOptions))
	s.mux.HandleFunc("GET /api/launch/dirs", s.auth(s.handleLaunchDirs))
	s.mux.HandleFunc("GET /api/models", s.authAgent(s.handleModels))
	s.mux.HandleFunc("GET /api/hosts/{host}/models", s.authAgent(s.onHostLaunch("models", s.handleModels)))
	s.mux.HandleFunc("POST /api/launch", s.auth(s.handleLaunch))
	s.mux.HandleFunc("GET /api/hosts/{host}/launch/options", s.auth(s.onHostLaunch("launch-options", s.handleLaunchOptions)))
	s.mux.HandleFunc("GET /api/hosts/{host}/launch/dirs", s.auth(s.onHostLaunch("launch-dirs", s.handleLaunchDirs)))
	s.mux.HandleFunc("POST /api/hosts/{host}/launch", s.auth(s.handleHostLaunch))
	// Workers, on this host: the hub calls a peer's through fleet.CallHost.
	s.mux.HandleFunc("GET /api/repos", s.auth(s.handleRepos))
	s.mux.HandleFunc("GET /api/checks", s.auth(s.handleChecks))
	s.mux.HandleFunc("POST /api/worker", s.auth(s.handleWorker))
	s.mux.HandleFunc("POST /api/remove", s.auth(s.handleRemove))
	s.mux.HandleFunc("GET /api/hosts/{host}/repos", s.auth(s.onHostLaunch("repos", s.handleRepos)))
	s.mux.HandleFunc("GET /api/hosts/{host}/checks", s.auth(s.onHostLaunch("checks", s.handleChecks)))
	// The handoff ledger (hub side).
	s.mux.HandleFunc("GET /api/ledger", s.authAgent(s.handleLedger))
	s.mux.HandleFunc("GET /api/inspect", s.authAgent(s.handleInspect))
	s.mux.HandleFunc("POST /api/handoffs", s.authAgent(s.handleHandoff))
	s.mux.HandleFunc("POST /api/handoffs/{id}/release", s.authAgent(s.handleReleaseHandoff))
	s.mux.HandleFunc("POST /api/handoffs/{id}/cancel", s.authAgent(s.handleCancelHandoff))
	s.mux.HandleFunc("POST /api/report", s.authAgent(s.handleReport))
	s.mux.HandleFunc("POST /api/plans", s.authAgent(s.handleCreatePlan))
	s.mux.HandleFunc("GET /api/plans", s.authAgent(s.handleListPlans))
	s.mux.HandleFunc("GET /api/plans/{id}", s.authAgent(s.handleGetPlan))
	s.mux.HandleFunc("POST /api/queue", s.authAgent(s.handleQueueAdd))
	s.mux.HandleFunc("GET /api/queue", s.authAgent(s.handleQueueList))
	s.mux.HandleFunc("DELETE /api/queue/{id}", s.authAgent(s.handleQueueCancel))
	s.mux.HandleFunc("POST /api/marks", s.authAgent(s.handleMark))
	s.mux.HandleFunc("POST /api/marks/clear", s.authAgent(s.handleUnmark))
	s.mux.HandleFunc("GET /api/removals", s.auth(s.handleRemovals))
	// Removal confirmation is the user's alone, always.
	s.mux.HandleFunc("POST /api/removals/confirm", s.auth(s.handleConfirmRemovals))
	// The briefing and the coordinator roles (hub side).
	s.mux.HandleFunc("GET /api/brief", s.authAgent(s.handleGetBrief))
	s.mux.HandleFunc("POST /api/brief", s.authAgent(s.handleSetBrief))
	s.mux.HandleFunc("GET /api/brief/schedule", s.authAgent(s.handleGetBriefSchedule))
	s.mux.HandleFunc("POST /api/brief/schedule", s.authAgent(s.handleSetBriefSchedule))
	s.mux.HandleFunc("POST /api/brief/schedule/dismiss", s.authAgent(s.handleDismissBriefMiss))
	s.mux.HandleFunc("GET /api/roles", s.authAgent(s.handleRoles))
	s.mux.HandleFunc("POST /api/roles", s.auth(s.handleSetRole))
	s.mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	s.mux.HandleFunc("GET /api/push/key", s.auth(s.handlePushKey))
	s.mux.HandleFunc("POST /api/push/subscribe", s.auth(s.handlePushSubscribe))
	s.mux.HandleFunc("POST /api/push/unsubscribe", s.auth(s.handlePushUnsubscribe))
	s.mux.HandleFunc("POST /api/push/test", s.auth(s.handlePushTest))
	s.mux.HandleFunc("POST /api/push/presence", s.auth(s.handlePresence))
}

// ListenAndServe runs until the listener fails.
func (s *Server) ListenAndServe() error {
	if len(s.cfg.AllowLogins) > 0 && !isLoopbackAddr(s.cfg.Addr) {
		return fmt.Errorf("Tailscale login auth needs a loopback listen address, got %q: any other client could forge the Tailscale-User-Login header", s.cfg.Addr)
	}
	if len(s.cfg.AllowLogins) == 0 {
		if !s.cfg.InsecureNoAuth {
			return errors.New("no Tailscale login allowlist configured: set -ts-allow-login / COMEBYE_TS_ALLOW_LOGIN (or pass -insecure-no-auth to serve an unauthenticated API)")
		}
		log.Printf("WARNING: -insecure-no-auth: no Tailscale login allowlist; the API is unauthenticated, so Tailscale Serve exposes it to your whole tailnet")
	}
	log.Printf("listening on http://%s (phone app; put Tailscale Serve in front)", s.cfg.Addr)
	hs := &http.Server{
		Addr:    s.cfg.Addr,
		Handler: s.Handler(),
		// No WriteTimeout: /api/events streams for as long as the client
		// stays. Slow or idle connections are still bounded.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return hs.ListenAndServe()
}

// Handler returns the server's HTTP handler with Host checking (against DNS
// rebinding, see hostAllowed) and CSRF protection applied.
// Tailscale identity auth is ambient (Serve attaches it to every request,
// like a cookie), so without this any web page the phone visits could POST
// prompts or keys to the agents. CrossOriginProtection rejects cross-origin
// unsafe requests via Sec-Fetch-Site/Origin; requiring a JSON content type
// also forces a CORS preflight on browsers too old to send those headers.
func (s *Server) Handler() http.Handler {
	jsonOnly := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ct := r.Header.Get("Content-Type")
			if mt, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/json" {
				http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
	return s.checkHost(http.NewCrossOriginProtection().Handler(withGzip(jsonOnly)))
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleAgents returns the current agent list (every host's, on a hub)
// and each host's status. It is served from cache and never dials.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if fullRecords(r) {
		writeJSON(w, map[string]any{"agents": s.fleet.Agents(), "hosts": s.fleet.Hosts()})
		return
	}
	writeJSON(w, map[string]any{"agents": slimAgents(s.fleet.Agents()), "hosts": slimHosts(s.fleet.Hosts())})
}

// fullRecords reports whether the caller (a hub reading a peer) asked for
// herdr's full agent records instead of the browser's slim view.
func fullRecords(r *http.Request) bool { return r.URL.Query().Get("full") == "1" }

// handleVersion returns the app version, which every /api response also
// carries in X-App-Version.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": s.version})
}

// handleHosts returns each host's status.
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"hosts": s.fleet.Hosts()})
}

// onHost routes a host-qualified per-agent call: the local host's to the
// local handler, a configured peer's to that peer, anything else to 404.
func (s *Server) onHost(op string, local http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.PathValue("host")
		switch {
		case host == s.fleet.LocalName():
			local(w, r)
		case s.fleet.HasPeer(host):
			s.fleet.Proxy(w, r, host, r.PathValue("target"), op)
		default:
			writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_host", "message": "no such host"})
		}
	}
}

// handleOutput reads a pane's recent output (recent_unwrapped), or with
// ?source=visible just the current screen — what a selection dialog looks
// like right now, for the "needs your answer" snapshot.
func (s *Server) handleOutput(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	source := r.URL.Query().Get("source")
	switch source {
	case "":
		source = "recent_unwrapped"
	case "recent_unwrapped", "visible":
	default:
		http.Error(w, "source must be recent_unwrapped or visible", http.StatusBadRequest)
		return
	}
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= s.cfg.ReadLines {
			lines = n
		}
	}
	// Resolve the target (agent name or pane id) to a pane id via the cache.
	paneID := s.resolvePane(target)
	if paneID == "" {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if source == "visible" {
		lines = 0 // the whole screen; a line count only applies to scrollback reads
	}
	read, err := s.client.ReadPane(ctx, paneID, source, lines)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, read)
}

// handleTranscript renders the agent's session JSONL (Pi or Claude Code) as
// chat messages. Agents whose session file cannot be resolved get 404 with
// code "no_session", which the client uses to fall back to raw pane output.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	agent := s.resolveAgent(r.PathValue("target"))
	if agent == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	path, err := transcript.SessionPath(*agent, s.manager.Agents())
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_session", "message": err.Error()})
		return
	}
	s.serveTranscript(w, r, path, transcript.Read)
}

// handleSubagentTranscript renders the transcript of one of a Claude
// agent's subagents ({sub} is its agentId), which Claude keeps next to the
// agent's own session.
func (s *Server) handleSubagentTranscript(w http.ResponseWriter, r *http.Request) {
	agent := s.resolveAgent(r.PathValue("target"))
	if agent == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	session, err := transcript.SessionPath(*agent, s.manager.Agents())
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_session", "message": err.Error()})
		return
	}
	s.serveSubagent(w, r, session, r.PathValue("sub"))
}

// serveSubagent answers for subagent id of the session at session.
func (s *Server) serveSubagent(w http.ResponseWriter, r *http.Request, session, id string) {
	if !transcript.ValidAgentID(id) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_subagent", "message": "invalid subagent id"})
		return
	}
	path, err := transcript.SubagentPath(session, id)
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_subagent", "message": err.Error()})
		return
	}
	s.serveTranscript(w, r, path, transcript.ReadSubagent)
}

// serveTranscript answers a transcript request for the session file at
// path, parsed by read (transcript.Read or transcript.ReadSubagent).
func (s *Server) serveTranscript(w http.ResponseWriter, r *http.Request, path string, read func(string, int, int) (*transcript.Transcript, error)) {
	limit, skip := queryInt(r, "limit", 100), queryInt(r, "skip", 0)
	if limit < 1 || limit > 500 {
		limit = 100
	}
	if skip < 0 {
		skip = 0
	}
	from := r.URL.Query().Get("from")
	// The app polls this every few seconds while the agent works; most
	// polls find the file unchanged. The ETag covers everything the
	// response depends on, so an unchanged poll is answered 304 without
	// reading the file (the browser revalidates on its own).
	fi, err := os.Stat(path)
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_session", "message": "session file is gone"})
		return
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s\x00%d\x00%d\x00%s", path, fi.Size(), fi.ModTime().UnixNano(), s.version, limit, skip, from))
	etag := `W/"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	t, err := read(path, limit, skip)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_session", "message": "session file is gone"})
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// ?from=<id>: only that message and the ones after it. The app passes
	// its oldest message that can still change (a tool call still waiting
	// for its result), else its newest, so a poll carries only what is new.
	// An id outside the window (or gone) gets the whole window instead.
	if from != "" {
		for i, m := range t.Messages {
			if m.ID == from {
				t.Messages = t.Messages[i:]
				t.Partial = true
				break
			}
		}
	}
	writeJSON(w, t)
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// promptBody is the JSON body of a prompt request.
type promptBody struct {
	Text      string `json:"text"`
	Wait      bool   `json:"wait"`
	TimeoutMs int64  `json:"timeout_ms"`
}

// handlePrompt sends a prompt to an agent. By default it returns as soon as
// herdr accepts the text; with wait=true herdr also starts a status wait in
// the same call, so the response reflects the settled state (no race).
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	var body promptBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	opts := herdr.PromptOptions{}
	if body.Wait {
		opts.Wait = true
		opts.Until = []herdr.AgentStatus{herdr.StatusIdle, herdr.StatusBlocked, herdr.StatusDone}
		if body.TimeoutMs > 0 {
			opts.TimeoutMs = body.TimeoutMs
		}
	}
	// A prompt+wait can legitimately take minutes; allow a long request.
	timeout := 30 * time.Second
	if body.Wait {
		timeout = time.Duration(body.TimeoutMs)*time.Millisecond + 30*time.Second
		if timeout > 30*time.Minute {
			timeout = 30 * time.Minute
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	if a := s.resolveAgent(target); a != nil {
		if err := s.guardPrompt(ctx, *a); err != nil {
			writeError(w, err)
			return
		}
	}
	agent, err := s.client.Prompt(ctx, target, body.Text, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"agent": agent})
}

// keysBody is the JSON body of a key-press request.
type keysBody struct {
	Keys []string `json:"keys"`
}

// handleKeys sends whitelisted keys (esc, enter, ctrl+c, ...) to an agent.
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	var body keysBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if len(body.Keys) == 0 {
		http.Error(w, "keys is required", http.StatusBadRequest)
		return
	}
	for _, k := range body.Keys {
		if !herdr.ValidKey(k) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_key", "message": "key " + k + " not allowed"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.client.SendKeys(ctx, target, body.Keys); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleEvents streams agent state over SSE. The initial agents snapshot is
// sent on connect; later events update it. On resync the client should
// refetch /api/agents.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if s.streams.Add(1) > maxStreams {
		s.streams.Add(-1)
		http.Error(w, "too many event streams", http.StatusServiceUnavailable)
		return
	}
	defer s.streams.Add(-1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	events, cancel := s.fleet.Hub().Subscribe()
	defer cancel()

	full := fullRecords(r)
	var last []byte // the last "agents" payload sent to this client
	send := func(ev state.Event) {
		if ev.Type == "agents" {
			// Always this instance's version: a hub's merged snapshots
			// are built locally, never relayed from a peer.
			ev.Version = s.version
		}
		var payload []byte
		var err error
		if full {
			payload, err = json.Marshal(ev)
		} else {
			payload, err = json.Marshal(slimEvent(ev))
		}
		if err != nil {
			return
		}
		if ev.Type == "agents" && !full {
			// Most updates change only fields browsers do not get: skip
			// them rather than wake the phone's radio and redraw for
			// nothing.
			if bytes.Equal(payload, last) {
				return
			}
			last = payload
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
		flusher.Flush()
	}

	// Initial snapshot.
	send(s.fleet.Snapshot())

	heartbeat := time.NewTicker(s.cfg.PingInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			send(ev)
		}
	}
}

// resolvePane maps an agent name or pane id to a pane id using the cached
// agent list.
func (s *Server) resolvePane(target string) string {
	for _, a := range s.manager.Agents() {
		if a.PaneID == target || (a.Name != "" && a.Name == target) {
			return a.PaneID
		}
	}
	return ""
}

// resolveAgent returns the cached AgentInfo for a target, if live.
func (s *Server) resolveAgent(target string) *herdr.AgentInfo {
	for _, a := range s.manager.Agents() {
		if a.PaneID == target || (a.Name != "" && a.Name == target) {
			copyOfAgent := a
			return &copyOfAgent
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var he *herdr.Error
	if errors.As(err, &he) {
		status := http.StatusBadGateway
		switch he.Code {
		case "agent_blocked", "dialog_open":
			status = http.StatusConflict
		case "pane_not_found", "agent_not_found":
			status = http.StatusNotFound
		case "agent_name_taken":
			status = http.StatusConflict
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": he.Code, "message": he.Message})
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}
