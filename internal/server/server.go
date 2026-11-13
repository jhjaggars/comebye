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
	"github.com/jhjaggars/comebye/internal/archive"
	"github.com/jhjaggars/comebye/internal/gitfetch"
	"github.com/jhjaggars/comebye/internal/provision"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
	"github.com/jhjaggars/comebye/internal/push"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
	"github.com/jhjaggars/comebye/internal/tsauth"
	"github.com/jhjaggars/comebye/web"
)

// Config configures the HTTP server.
type Config struct {
	// OpencodeDir is where OpenCode sessions are mirrored as files (default
	// under the state directory); see internal/opencode.
	OpencodeDir string

	// Version is the binary's release version (comebye -version); every
	// API response carries it in X-Comebye-Version, so `comebye upgrade`
	// can tell the restarted service runs the binary it installed.
	Version string
	// Archive, when set, receives a final copy of an agent's transcript
	// (and its subagents') just before the agent is removed; ArchiveSources
	// are the session directories it covers. Off when nil.
	Archive        *archive.Archiver
	ArchiveSources []archive.Source
	// RecallIndex is the transcript index file the app searches, read-only.
	// Empty, or no such file: the app says there is no index.
	RecallIndex string
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
	// PublicURL is the https URL this hub is reached at (the Tailscale Serve
	// name, no trailing slash); links to published pages are built from it.
	// Empty: the API gives paths only.
	PublicURL string
	// PagesURL is the https URL of the pages listener (-pages-listen) as the
	// browser reaches it, e.g. https://hub.tailnet.ts.net:8443: the app frames
	// pages from there, and page links are built from it. Empty: pages are
	// served on the app's own origin.
	PagesURL string
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
	// ScratchDir is the temporary directory whose leftovers are cleaned up
	// when a session is removed and by the periodic prune (/tmp). Empty
	// turns all of that off.
	ScratchDir string
	// ScratchEvery is how often the prune runs (0: not at all);
	// TmpMaxAge is how old a tmp.* directory must be to be pruned (0: the
	// default of 48h; negative: tmp.* directories are left alone).
	ScratchEvery time.Duration
	TmpMaxAge    time.Duration
	// StateDir holds the handoff ledger (ledger.json), the briefing
	// (brief.json) and its schedule (brief-schedule.json), the roles (roles.json) and an optional
	// handoff-template.md. Empty turns handoffs and marks off.
	StateDir string
	// RepoHealth hears about damaged repositories (fetches, other git
	// commands, the daily fsck); nil leaves them unreported.
	RepoHealth *gitfetch.Health
	// NoLedger turns the ledger (and the briefing and roles) off even with
	// a state directory: the instance is a peer that is not the hub.
	NoLedger bool
	// Upstream, on a peer, is its hub: the agent role's requests are
	// forwarded there, and the peer keeps no ledger of its own.
	Upstream *fleet.Upstream
	// TestMode is for the browser tests and the dev server (-test-mode): the app polls every
	// TestPollMs instead of every 3 to 20 s, and the to-do list has no per-minute limit.
	// It changes no other behaviour; off by default.
	TestMode bool
	// AutoTodo turns on the to-do items the hub files itself (autotodo.go).
	AutoTodo AutoTodoConfig
	// Release turns on the release and upgrade buttons (release.go).
	Release ReleaseConfig
	// Provision turns on adding hosts: on a peer, the provisioner role; on a hub,
	// the default provisioner (provision.go).
	Provision ProvisionConfig
	// AgentToken is the local agent token (see agenttoken); requests that
	// carry it get the agent role. Empty disables the agent role.
	AgentToken string
	// CoordinatorDir is where the "Coordinator" launch preset starts its
	// agent (default the home directory). It need not lie in a launch
	// root; only that preset may use it.
	CoordinatorDir string
	// CoordinatorResetTokens is the context size, in tokens, past which the
	// first briefing marks the coordinator's in-place reset due (reset.go).
	// 0 turns the automatic reset off. Hub only.
	CoordinatorResetTokens int
	// RelayBlocked, when positive, is how long an agent that has a
	// coordinator must be blocked before the coordinator is told (the
	// blocked relay, blockedrelay.go). Hub only: it needs the ledger.
	RelayBlocked time.Duration
	// QuietAfter, when positive, is how long a working handoff worker may
	// leave its session file untouched before the hub marks it quiet and
	// files one to-do (quiet.go). Hub only.
	QuietAfter time.Duration
	// ParkSuggestAfter, when positive, is how long an agent must have been idle
	// before the hub suggests parking it (at least an hour). Suggest only: nothing is parked by itself.
	ParkSuggestAfter time.Duration
	// ParkRetention, when positive, is how long a parked agent is kept before the
	// hub proposes removing it in the removal review (the user confirms).
	ParkRetention time.Duration
	// LandTurnTimeout is how long a worker may hold its turn in a landing
	// queue before the slot is released and its coordinator told (0: the
	// default, 45 minutes; negative: never). Hub only.
	LandTurnTimeout time.Duration
	// DigestEvery is how long routine coordinator notices (landed,
	// finished with no decisions, cleanup lines) wait to go out together in
	// one digest; 0 sends each as it comes. Urgent notices are never held.
	DigestEvery time.Duration
	// CoordinatorArgs are extra arguments per agent kind that the
	// Coordinator preset appends to the preset's own (e.g. pi: --auto,
	// a flag only some Pi installs have).
	CoordinatorArgs map[string][]string
	// DefaultKind and AllowedKinds seed the host settings (-default-kind,
	// -allowed-kinds): used where settings.json has no value.
	DefaultKind  string
	AllowedKinds []string
	// AllowCap is the app capability that admits a tagged node through
	// Tailscale Serve (capPeers); WhoIs identifies the callers there.
	AllowCap string
	// WhoIs identifies a connection's tailnet node (tailscaled's WhoIs).
	WhoIs tsauth.Resolver
	// SelfRegister lets a tagged node announce itself as a peer.
	SelfRegister bool
	// HubOnly: this hub hosts no agents (no herdr); /healthz says so.
	HubOnly bool
	// Usage configures token counting (usage.go).
	Usage UsageConfig
	// LockErr, when set, is the single-hub lock's health (hublock.Lock.Err);
	// /readyz fails while it returns an error.
	LockErr func() error
}

// Server is the phone app HTTP server.
type Server struct {
	// bg counts the queued-prompt senders that run in the background, so a test can wait for them before its
	// state directory is removed (waitBackground).
	bg      sync.WaitGroup
	cfg     Config
	recall  *archive.Reader
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
	// panels holds the side-panel change watchers (panelwatch.go).
	panels panelHub
	// bgAgents caches each idle agent's background tasks (bgagents.go).
	bgAgents bgAgents
	// quiet follows working workers that stopped writing (quiet.go).
	quiet *quietMon
	// parkSug suggests parking long-idle agents (parksuggest.go); nil when off.
	parkSug *parkSuggest
	// peerAgents replaces the fleet's agent list for peerBackground (tests).
	peerAgents func() []herdr.AgentInfo
	// health tests that the state directory is writable (nil without one);
	// shuttingDown fails /readyz.
	health       *healthProbe
	shuttingDown atomic.Bool
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
	// landTurn is the landing queue's turn timeout (0: never).
	landTurn time.Duration
	// notices and noticeArriveWait: see notice.go.
	notices          noticeBook
	noticeArriveWait time.Duration
	// askPoll and askWait pace asking a marked session whether it is safe
	// to remove (tests shrink them); latest, when set, replaces latestOf.
	askPoll time.Duration
	askWait time.Duration
	// asking counts the asks under way (they run git checks in the
	// session's worktree), so tests can wait for them before cleaning up.
	asking sync.WaitGroup
	// probes caches the removals review's re-checks (see askcheck.go);
	// probeTTL is how long one lives (tests shrink it).
	probes   *probeCache
	probeTTL time.Duration
	// reaskGap, reaskCap and reaskMax pace the hub's automatic re-asks
	// (tests change them); reaskEvery is how often RunReask looks.
	reaskGap, reaskCap time.Duration
	reaskMax           int
	reaskEvery         time.Duration
	// releasing counts the releaseQueued passes started by queueing a step,
	// so tests can wait for them before asserting or cleaning up.
	releasing sync.WaitGroup
	latest    func(ctx context.Context, host, pane string) *transcript.Latest
	// planPush tells the user a plan settled (the dispatcher's NotifyPlan;
	// tests replace it).
	planPush func(id, title, body string)
	// ledger records handoffs and marks (nil without a state directory,
	// and on a peer that is not the hub).
	ledger *ledger.Store
	// brief and roles live next to the ledger (nil when it is).
	brief          *briefStore
	pq             *promptQueue
	todos          *todoStore
	outbox         *outbox
	digests        *digestStore
	digestEvery    time.Duration
	auto           *autoTodo
	relay          *blockedRelay
	rel            *relState
	rollout        *rolloutEngine // the rolling rollout; only on the ledger host (see rollout.go)
	prov           *provState
	pjobs          *provJobs           // on a provisioner: the jobs it runs
	phosts         *provision.HostBook // on a provisioner: the hosts it built and what they cost
	phostMu        sync.Mutex          // guards phostErr, phostAt
	phostErr       string              // why the cloud could not be asked last time
	phostAt        time.Time           // when it last was
	pseen          *provSeen
	provObserveGap time.Duration // how often the provisioner asks (tests shorten it)
	hjobs          *hubJobs      // on a hub: the jobs it has asked provisioners to run
	pages          *pageStore
	commentLim     commentLimiter
	cmtOnce        sync.Once
	pageCmts       *commentStore
	briefSched     *briefSchedStore
	resets         *resetStore
	// resetPoll and resetWait pace a reset's wait for the new session
	// (tests shrink them).
	resetPoll, resetWait time.Duration
	roles                *roleStore
	// kinds remembers the agent kinds that worked on this host (nil
	// without a state directory).
	kinds *kindStore
	// unrec saves the raw screen of a blocked agent whose dialog is not recognised.
	unrec *unrecCapture
	// pins: the documents agents declared (comebye show), see panelpin.go.
	pins *pinStore
	// settings is this host's settings.json (see settings.go); nil without a state directory.
	settings *settingsStore
	// parks is the park record store (parked.json); see park.go. Opened on first use.
	parkOnce sync.Once
	parks    *park.Store
	parkErr  error
	// pcache is the hub's view of every host's park records (parkhub.go).
	pcache parkCache
	// prop remembers the parked agents already proposed for removal (parkretain.go).
	propOnce sync.Once
	prop     *parkProposed
	// switchGap is the pause between the slash commands of one model
	// switch (tests shrink it).
	switchGap time.Duration
	// models caches each kind's model list; listModels, when set,
	// replaces the discovery (tests).
	models     modelCache
	listModels func(ctx context.Context, kind string) *modelCatalog
	coord      coord
	// typing holds messages for panes whose user is typing (typing.go).
	typing *typingTracker
	// usage counts tokens (usage.go); nil when off.
	usage *usageState
}

// maxStreams caps concurrent SSE clients. A phone holds one or two and a
// hub one per peer; each stream costs a goroutine and a hub subscription.
const maxStreams = 64

// New builds the server. manager is the local host's state manager.
func New(cfg Config, client *herdr.Client, manager *state.Manager) *Server {
	if cfg.RepoHealth != nil {
		originFetcher.Health = cfg.RepoHealth
		gitHealth = cfg.RepoHealth
	}
	f := cfg.Fleet
	if f == nil {
		name := cfg.HostName
		if name == "" {
			name = "local"
		}
		f = fleet.New(manager, name, nil, fleet.Options{Version: cfg.Version})
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 15 * time.Second
	}
	s := &Server{cfg: cfg, recall: archive.NewReader(cfg.RecallIndex), client: client, manager: manager, fleet: f, mux: http.NewServeMux(),
		agentReadyPoll: 200 * time.Millisecond, agentReadyWait: launchStartTimeout, switchGap: 700 * time.Millisecond, resetPoll: 500 * time.Millisecond, resetWait: 30 * time.Second,
		briefWait: 20 * time.Second, briefTries: 3, briefBackoff: 3 * time.Second, askPoll: askPoll, askWait: askWait,
		probes: newProbeCache(), probeTTL: probeTTL, reaskGap: reaskGap, reaskCap: reaskCap, reaskMax: reaskMax, reaskEvery: reaskEvery}
	switch {
	case cfg.LandTurnTimeout == 0:
		s.landTurn = DefaultLandTurnTimeout
	case cfg.LandTurnTimeout > 0:
		s.landTurn = cfg.LandTurnTimeout
	}
	s.typing = newTypingTracker()
	s.initParkSuggest(cfg.ParkSuggestAfter)
	s.rel = newRelState()
	s.prov = newProvState()
	if cfg.TestMode { // browser tests: a forced refresh may repeat after a moment, not after 20 s
		s.prov.catGap, s.prov.hostsGap = time.Second, 300*time.Millisecond
	}
	if cfg.Provision.Tool != nil {
		s.pjobs = openProvJobs(cfg.StateDir)
		s.phosts = provision.OpenHostBook(cfg.StateDir)
		s.pjobs.book = s.phosts
		for _, name := range s.pjobs.interruptedDestroy {
			s.phosts.EndDestroy(name, false, false, "the provisioner restarted while this was being destroyed: destroy it again")
		}
	}
	s.hjobs = openHubJobs(cfg.StateDir)
	s.pseen = openProvSeen(cfg.StateDir)
	switch rc := &s.cfg.Release; {
	case rc.Host != "" && rc.Tool != nil:
		log.Printf("release: both a local release tool and -release-host %s are set: tagging is off", rc.Host)
		rc.Host, rc.Tool = "", nil
	case rc.Host != "":
		rc.Tool = &remoteTool{s: s, host: rc.Host}
	}
	if cfg.StateDir != "" {
		s.health = &healthProbe{dir: cfg.StateDir, now: time.Now}
	}
	s.coord = coord{checks: map[string]cachedChecks{}, checking: map[string]bool{}, reporting: map[string]time.Time{}, starting: map[string]bool{}, started: map[string]time.Time{}, quiet: map[string]time.Time{}, calling: map[string]time.Time{}, landSched: map[string]landSched{}, bg: map[string]bgCache{}}
	if cfg.StateDir != "" {
		s.kinds = openKinds(kindsPath(cfg.StateDir))
		s.unrec = newUnrecCapture(cfg.StateDir, s.fleet.LocalName())
		s.settings = openSettings(settingsPath(cfg.StateDir))
		s.pins = openPins(pinsPath(cfg.StateDir))
	} else {
		s.pins = &pinStore{data: map[string]docPin{}}
	}
	if cfg.StateDir != "" && !cfg.NoLedger && cfg.Upstream == nil {
		l, err := openLedger(cfg.StateDir)
		if err != nil {
			log.Printf("handoff ledger disabled: %v", err)
		} else {
			s.ledger = l
			s.rollout = newRolloutEngine(filepath.Join(cfg.StateDir, "release", "rollout.json"), serverRolloutOps{s}, s.auditBy)
			s.rollout.onState = s.noteRollout
			if cfg.Release.Simulated {
				s.rollout.soak = 8 * time.Second // a demo does not wait two minutes
			}
			if cfg.TestMode { // browser tests: the rollout engine ticks and soaks in a second, not 5 and 8
				s.rollout.tickEvery = TestPollMs * time.Millisecond
				s.rollout.soak = time.Second
			}
			s.outbox = openOutbox(outboxPath(cfg.StateDir))
			s.digests = &digestStore{path: filepath.Join(cfg.StateDir, "digest-history.json")}
			s.digestEvery = cfg.DigestEvery
			s.brief = &briefStore{path: filepath.Join(cfg.StateDir, "brief.json")}
			if s.briefSched, err = openBriefSched(filepath.Join(cfg.StateDir, "brief-schedule.json")); err != nil {
				log.Printf("brief schedule: %v (starting with none)", err)
				s.briefSched = &briefSchedStore{path: filepath.Join(cfg.StateDir, "brief-schedule.json")}
			}
			if s.resets, err = openResets(filepath.Join(cfg.StateDir, "coordinator-reset.json")); err != nil {
				log.Printf("coordinator reset: %v (starting with none)", err)
				s.resets = &resetStore{path: filepath.Join(cfg.StateDir, "coordinator-reset.json"), busy: map[string]bool{}}
			}
			if s.roles, err = openRoles(filepath.Join(cfg.StateDir, "roles.json")); err != nil {
				log.Printf("roles: %v (starting with none)", err)
				s.roles = &roleStore{path: filepath.Join(cfg.StateDir, "roles.json"), data: map[string]Role{}}
			}
			if s.pq, err = openPromptQueue(queuePath(cfg.StateDir)); err != nil {
				log.Printf("prompt queue: %v (starting empty)", err)
				s.pq = newPromptQueue("") // in memory: never overwrite the unreadable file
			}
			if s.todos, err = openTodoStore(todoPath(cfg.StateDir)); err != nil {
				log.Printf("to-do list: %v (starting empty)", err)
				s.todos = newTodoStore("") // in memory: never overwrite the unreadable file
			}
			if cfg.TestMode {
				s.todos.rateMax = -1
			}
			s.initAutoTodo(cfg.AutoTodo)
			s.initBlockedRelay(cfg.RelayBlocked)
			s.initQuiet(cfg.QuietAfter)
			if s.pages, err = openPageStore(filepath.Join(cfg.StateDir, "pages")); err != nil {
				log.Printf("pages: %v (published pages are off)", err)
				s.pages = nil // never overwrite the unreadable index
			}
			s.endStaleAsks()
			s.trackInstances()
			s.recoverTagging()
		}
	}
	s.initUsage()
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
	static.csp = appCSP(s.pagesOrigin())
	if dir := os.Getenv("COMEBYE_DEV_WEB_DIR"); dir != "" {
		log.Printf("serving the app from %s (COMEBYE_DEV_WEB_DIR; edits show on reload)", dir)
		s.mux.Handle("GET /", diskStatic{dir, appCSP(s.pagesOrigin())})
	} else {
		s.mux.Handle("GET /", static)
	}
	// Routes the agent role may use are wrapped in authAgent (forwarded to
	// the hub on a peer with -hub), authWorker (the same, and open to a tagged
	// node) or authAgentLocal; everything wrapped in auth is the user's alone
	// (see agentauth.go).
	s.recallRoutes()
	s.usageRoutes()
	s.mux.HandleFunc("GET /api/version", s.authAgentLocal(s.handleVersion))
	s.mux.HandleFunc("GET /api/test-mode", s.auth(s.handleTestMode))
	s.mux.HandleFunc("GET /api/stt", s.auth(s.handleSTT))
	s.mux.HandleFunc("GET /stt/{version}/{path...}", s.serveSTT)
	s.mux.HandleFunc("GET /api/whoami", s.authAgentLocal(s.handleWhoami))
	s.mux.HandleFunc("GET /api/playbook", s.authAgentLocal(s.handlePlaybook))
	s.mux.HandleFunc("GET /api/agents", s.authWorker(s.handleAgents))
	s.mux.HandleFunc("GET /api/agents/{target}/output", s.authAgent(s.readGuard(s.handleOutput)))
	s.mux.HandleFunc("GET /api/agents/{target}/transcript", s.authAgent(s.readGuard(s.handleTranscript)))
	s.mux.HandleFunc("GET /api/agents/{target}/subagents/{sub}/transcript", s.authAgent(s.readGuard(s.handleSubagentTranscript)))
	s.mux.HandleFunc("POST /api/agents/{target}/prompt", s.authWorker(s.selfGuard(s.promptGuard(s.handlePrompt))))
	s.mux.HandleFunc("POST /api/agents/{target}/model", s.authAgent(s.selfGuard(s.handleSwitchModel)))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/model", s.authAgent(s.selfGuard(s.handleSwitchModel)))
	// The coordinator's context reset, on the agent's host: the hub's own
	// calls (the user's identity), never an agent's.
	s.mux.HandleFunc("GET /api/agents/{target}/context", s.auth(s.handleContext))
	s.mux.HandleFunc("POST /api/agents/{target}/reset", s.auth(s.handleResetAgent))
	s.mux.HandleFunc("POST /api/agents/{target}/noticeseen", s.auth(s.handleNoticeSeen))
	s.mux.HandleFunc("POST /api/agents/{target}/keys", s.auth(s.handleKeys))
	s.mux.HandleFunc("GET /api/agents/{target}/dialog", s.auth(s.handleDialog))
	s.mux.HandleFunc("GET /api/agents/{target}/activity", s.auth(s.handleActivity))
	s.mux.HandleFunc("POST /api/agents/{target}/answer", s.auth(s.handleAnswer))
	s.mux.HandleFunc("POST /api/agents/{target}/image", s.auth(s.handleImage))
	// Host-qualified routes: the local host is served directly, a peer's
	// agents are proxied to that peer (hub only).
	s.mux.HandleFunc("GET /api/hosts", s.authWorker(s.handleHosts))
	s.mux.HandleFunc("GET /api/peers", s.auth(s.handlePeers))
	s.mux.HandleFunc("POST /api/peers", s.auth(s.handleAddPeer))
	s.mux.HandleFunc("DELETE /api/peers/{name}", s.auth(s.handleRemovePeer))
	s.mux.HandleFunc("PUT /api/peers/{name}/instance", s.auth(s.handleSetInstance))
	// Self-registration: answered only to a tagged node (capPeers).
	s.mux.HandleFunc("POST /api/peers/announce", s.authAgentLocal(s.handleAnnounce))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/output", s.authAgent(s.readGuard(s.onHost("output", s.handleOutput))))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/transcript", s.authAgent(s.readGuard(s.onHost("transcript", s.handleTranscript))))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/subagents/{sub}/transcript", s.authAgent(s.readGuard(s.onHost("subagent-transcript", s.handleSubagentTranscript))))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/prompt", s.authWorker(s.selfGuard(s.promptGuard(s.onHost("prompt", s.handlePrompt)))))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/keys", s.auth(s.onHost("keys", s.handleKeys)))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/dialog", s.auth(s.onHost("dialog", s.handleDialog)))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/activity", s.auth(s.onHost("activity", s.handleActivity)))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/answer", s.auth(s.onHost("answer", s.handleAnswer)))
	s.mux.HandleFunc("POST /api/hosts/{host}/agents/{target}/image", s.auth(s.onHost("image", s.handleImage)))
	s.mux.HandleFunc("GET /api/launch/options", s.auth(s.handleLaunchOptions))
	s.mux.HandleFunc("GET /api/launch/dirs", s.auth(s.handleLaunchDirs))
	s.mux.HandleFunc("GET /api/models", s.authAgent(s.handleModels))
	// Host settings: read by the user, a coordinator (any host) and a tagged
	// node (its own host); written by the user alone.
	s.mux.HandleFunc("GET /api/settings", s.authWorker(s.settingsGuard(s.handleGetSettings)))
	s.mux.HandleFunc("PUT /api/settings", s.auth(s.handlePutSettings))
	s.mux.HandleFunc("GET /api/hosts/{host}/settings", s.authWorker(s.settingsGuard(s.onHostLaunch("settings", s.handleGetSettings))))
	s.mux.HandleFunc("PUT /api/hosts/{host}/settings", s.auth(s.onHostLaunch("settings-set", s.handlePutSettings)))
	s.mux.HandleFunc("GET /api/hosts/{host}/models", s.authAgent(s.onHostLaunch("models", s.handleModels)))
	s.mux.HandleFunc("POST /api/launch", s.auth(s.handleLaunch))
	s.mux.HandleFunc("GET /api/hosts/{host}/launch/options", s.auth(s.onHostLaunch("launch-options", s.handleLaunchOptions)))
	s.mux.HandleFunc("GET /api/hosts/{host}/launch/dirs", s.auth(s.onHostLaunch("launch-dirs", s.handleLaunchDirs)))
	s.mux.HandleFunc("POST /api/hosts/{host}/launch", s.auth(s.handleHostLaunch))
	// The side panels (see panel.go): the user reads any agent's, an agent its
	// own; the hub reads a peer's through fleet.CallHost (/api/panel/*).
	s.mux.HandleFunc("GET /api/agents/{target}/diff", s.authAgent(s.handlePanelDiff))
	s.mux.HandleFunc("GET /api/agents/{target}/file", s.authAgent(s.handlePanelFile))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/diff", s.authAgent(s.handlePanelDiff))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/file", s.authAgent(s.handlePanelFile))
	s.mux.HandleFunc("GET /api/agents/{target}/panel/events", s.authAgent(s.handlePanelEvents))
	s.mux.HandleFunc("GET /api/hosts/{host}/agents/{target}/panel/events", s.authAgent(s.handlePanelEvents))
	s.mux.HandleFunc("POST /api/show", s.authAgent(s.handleShow))
	s.mux.HandleFunc("GET /api/panel/diff", s.auth(s.handlePanelHost("diff")))
	s.mux.HandleFunc("GET /api/panel/file", s.auth(s.handlePanelHost("file")))
	// Workers, on this host: the hub calls a peer's through fleet.CallHost.
	s.mux.HandleFunc("GET /api/repos", s.auth(s.handleRepos))
	s.mux.HandleFunc("GET /api/checks", s.auth(s.handleChecks))
	s.mux.HandleFunc("GET /api/probe", s.auth(s.handleProbe))
	s.mux.HandleFunc("POST /api/worker", s.auth(s.handleWorker))
	s.mux.HandleFunc("POST /api/remove", s.auth(s.handleRemove))
	s.mux.HandleFunc("POST /api/scratch", s.authAgentLocal(s.handleScratch))
	// Park and resume (park.go): the user's alone. The host writes the record.
	s.mux.HandleFunc("POST /api/park", s.auth(s.handlePark))
	s.mux.HandleFunc("POST /api/resume", s.auth(s.handleResume))
	s.mux.HandleFunc("GET /api/parked", s.authAgent(s.handleParked))
	s.mux.HandleFunc("POST /api/hosts/{host}/park", s.auth(s.onHostLaunch("park", s.handlePark)))
	s.mux.HandleFunc("POST /api/hosts/{host}/resume", s.auth(s.onHostLaunch("resume", s.handleResume)))
	s.mux.HandleFunc("POST /api/park/mark", s.auth(s.handleParkMark))
	s.mux.HandleFunc("POST /api/park/pin", s.auth(s.handleParkPin))
	s.mux.HandleFunc("POST /api/hosts/{host}/park/pin", s.auth(s.onHostLaunch("park-pin", s.handleParkPin)))
	s.mux.HandleFunc("GET /api/parked/all", s.authAgent(s.handleParkedAll))
	s.mux.HandleFunc("GET /api/hosts/{host}/parked", s.authAgent(s.onHostLaunch("parked", s.handleParked)))
	s.mux.HandleFunc("GET /api/hosts/{host}/repos", s.auth(s.onHostLaunch("repos", s.handleRepos)))
	s.mux.HandleFunc("GET /api/hosts/{host}/checks", s.auth(s.onHostLaunch("checks", s.handleChecks)))
	// The handoff ledger (hub side).
	s.mux.HandleFunc("GET /api/ledger", s.authWorker(s.handleLedger))
	s.mux.HandleFunc("GET /api/inspect", s.authWorker(s.handleInspect))
	s.mux.HandleFunc("GET /api/ledger/{id}/hubcheck", s.authWorker(s.handleHubCheck))
	s.mux.HandleFunc("POST /api/handoffs", s.authAgent(s.handleHandoff))
	s.mux.HandleFunc("POST /api/handoffs/{id}/release", s.authAgent(s.handleReleaseHandoff))
	s.mux.HandleFunc("POST /api/handoffs/{id}/cancel", s.authAgent(s.handleCancelHandoff))
	s.mux.HandleFunc("POST /api/report", s.authWorker(s.handleReport))
	s.mux.HandleFunc("POST /api/landings", s.authAgent(s.handleLanding))
	s.mux.HandleFunc("GET /api/landings/queue", s.authAgent(s.handleLandQueue))
	s.mux.HandleFunc("POST /api/landings/queue", s.authAgent(s.handleLandQueueEdit))
	s.mux.HandleFunc("POST /api/plans", s.authAgent(s.handleCreatePlan))
	s.mux.HandleFunc("GET /api/plans", s.authAgent(s.handleListPlans))
	s.mux.HandleFunc("GET /api/plans/{id}", s.authAgent(s.handleGetPlan))
	s.mux.HandleFunc("POST /api/queue", s.authWorker(s.handleQueueAdd))
	s.mux.HandleFunc("GET /api/queue", s.authWorker(s.handleQueueList))
	s.mux.HandleFunc("DELETE /api/queue/{id}", s.authWorker(s.handleQueueCancel))
	// The to-do list: any agent adds and reads; answering, finishing and
	// dropping are the user's or a coordinator's (checked in the handler).
	s.mux.HandleFunc("POST /api/todos", s.authWorker(s.handleTodoAdd))
	s.mux.HandleFunc("POST /api/todos/from-last", s.authWorker(s.handleTodoFromLast))
	s.mux.HandleFunc("GET /api/todos", s.authWorker(s.handleTodoList))
	s.mux.HandleFunc("GET /api/todos/{id}", s.authWorker(s.handleTodoGet))
	s.mux.HandleFunc("POST /api/todos/{id}/answer", s.authAgent(s.handleTodoAnswer))
	s.mux.HandleFunc("PUT /api/todos/{id}/draft", s.authAgent(s.handleTodoDraft))
	s.mux.HandleFunc("POST /api/todos/{id}/submit", s.authAgent(s.handleTodoSubmit))
	s.mux.HandleFunc("POST /api/todos/{id}/done", s.authAgent(s.handleTodoDone))
	s.mux.HandleFunc("POST /api/todos/{id}/drop", s.authAgent(s.handleTodoDrop))
	s.mux.HandleFunc("POST /api/todos/{id}/keep", s.authAgent(s.handleTodoKeep))
	s.mux.HandleFunc("POST /api/todos/{id}/dismiss", s.authAgent(s.handleTodoDismiss))
	// Releasing and upgrading: the user role only (s.auth), never an agent
	// level; TestReleaseRoutesAreUserOnly keeps it so.
	s.mux.HandleFunc("GET /api/release/upgrade", s.auth(s.handleUpgradeStatus))
	s.mux.HandleFunc("POST /api/release/upgrade", s.auth(s.handleUpgradeLocal))
	// A hub's tagging through this peer (-release-host there): hub calls only.
	s.mux.HandleFunc("POST /api/release/check", s.auth(s.handlePeerCheck))
	s.mux.HandleFunc("POST /api/release/tag", s.auth(s.handlePeerTag))
	s.mux.HandleFunc("GET /api/release/build", s.auth(s.handlePeerBuild))
	s.mux.HandleFunc("POST /api/release/deploy", s.auth(s.handlePeerDeploy))
	s.mux.HandleFunc("GET /api/release/deploy", s.auth(s.handlePeerDeployStatus))
	s.mux.HandleFunc("GET /api/releases", s.auth(s.handleReleases))
	s.mux.HandleFunc("GET /api/board", s.authAgent(s.handleBoard))
	s.mux.HandleFunc("GET /api/release/{id}", s.auth(s.handleReleaseView))
	s.mux.HandleFunc("POST /api/release/{id}/approve", s.auth(s.handleReleaseApprove))
	s.mux.HandleFunc("POST /api/release/{id}/dismiss", s.auth(s.handleReleaseDismiss))
	s.mux.HandleFunc("POST /api/release/{id}/deploy", s.auth(s.handleReleaseDeploy))
	s.mux.HandleFunc("POST /api/hosts/{host}/release/upgrade", s.auth(s.handleHostUpgrade))
	s.mux.HandleFunc("POST /api/release/rollout", s.auth(s.handleRolloutStart))
	for _, op := range []string{"pause", "resume", "cancel"} {
		s.mux.HandleFunc("POST /api/release/rollout/"+op, s.auth(s.handleRolloutControl(op)))
	}
	s.provisionRoutes()
	s.pageRoutes()
	s.mux.HandleFunc("POST /api/marks/reask", s.authAgent(s.handleReask))
	s.mux.HandleFunc("POST /api/marks", s.authAgent(s.handleMark))
	s.mux.HandleFunc("POST /api/marks/clear", s.authAgent(s.handleUnmark))
	s.mux.HandleFunc("POST /api/deliveries", s.authAgent(s.handleDeliver))
	s.mux.HandleFunc("POST /api/sweep", s.authAgent(s.handleSweep))
	s.mux.HandleFunc("GET /api/removals", s.auth(s.handleRemovals))
	// Removal confirmation is the user's alone, always.
	s.mux.HandleFunc("POST /api/removals/confirm", s.auth(s.handleConfirmRemovals))
	// The briefing and the coordinator roles (hub side).
	s.mux.HandleFunc("GET /api/brief", s.authAgent(s.handleGetBrief))
	s.mux.HandleFunc("POST /api/brief", s.authAgent(s.handleSetBrief))
	s.mux.HandleFunc("GET /api/brief/history", s.auth(s.handleGetBriefHistory))
	s.mux.HandleFunc("GET /api/digests", s.auth(s.handleGetDigests))
	s.mux.HandleFunc("GET /api/brief/schedule", s.authAgent(s.handleGetBriefSchedule))
	s.mux.HandleFunc("POST /api/brief/schedule", s.authAgent(s.handleSetBriefSchedule))
	s.mux.HandleFunc("POST /api/brief/schedule/dismiss", s.authAgent(s.handleDismissBriefMiss))
	s.mux.HandleFunc("GET /api/coordinator", s.authAgent(s.handleCoordinator))
	s.mux.HandleFunc("POST /api/coordinator/reset", s.auth(s.handleCoordinatorReset))
	s.mux.HandleFunc("GET /api/roles", s.authWorker(s.handleRoles))
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
	app := s.pagesHeaders(false, s.checkHost(s.capPeers(s.base())))
	// The health probes come first: no Host check (a kubelet probes the pod
	// IP), no login, nothing secret in the answer (see health.go).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.serveHealth(w, r) {
			app.ServeHTTP(w, r)
		}
	})
}

func (s *Server) base() http.Handler {
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
	return http.NewCrossOriginProtection().Handler(withGzip(jsonOnly))
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
	if fullRecords(r) && !callerOf(r).tagged {
		writeJSON(w, map[string]any{"agents": s.withBackground(s.fleet.Agents()), "hosts": s.fleet.Hosts()})
		return
	}
	writeJSON(w, map[string]any{"agents": s.slimAgents(s.fleet.Agents()), "hosts": slimHosts(s.fleet.Hosts())})
}

// fullRecords reports whether the caller (a hub reading a peer) asked for
// herdr's full agent records instead of the browser's slim view.
func fullRecords(r *http.Request) bool { return r.URL.Query().Get("full") == "1" }

// waitBackground waits for the queued-prompt senders that are running. Tests call it before their temp directory
// goes, so a sender that settles a prompt late does not write into a directory being removed.
func (s *Server) waitBackground() { s.bg.Wait() }

// handleVersion returns the app version, which every /api response also
// carries in X-App-Version.
// TestPollMs is the app's poll interval in test mode, in place of the 3 to 20 s ones.
const TestPollMs = 300

// handleTestMode tells the app whether to poll fast: {"poll_ms":300} in test mode, {} otherwise.
func (s *Server) handleTestMode(w http.ResponseWriter, r *http.Request) {
	if s.cfg.TestMode {
		writeJSON(w, map[string]int{"poll_ms": TestPollMs})
		return
	}
	writeJSON(w, map[string]int{})
}

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
	s.syncOpencode(r.Context(), *agent, false)
	path, err := transcript.SessionPath(*agent, s.manager.Agents())
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_session", "message": err.Error()})
		return
	}
	s.serveTranscript(w, r, path, transcript.Read, func(t *transcript.Transcript) { t.Context = s.contextUsage(*agent, path) })
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
	s.serveTranscript(w, r, path, transcript.ReadSubagent, nil)
}

// serveTranscript answers a transcript request for the session file at
// path, parsed by read (transcript.Read or transcript.ReadSubagent).
func (s *Server) serveTranscript(w http.ResponseWriter, r *http.Request, path string, read func(string, int, int) (*transcript.Transcript, error), decorate func(*transcript.Transcript)) {
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
	if decorate != nil {
		decorate(t)
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
	// Force is the app's "Send anyway": skip the typing hold and the
	// unrecognised-blocked refusal (a recognised dialog still refuses).
	Force bool `json:"force,omitempty"`
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
	// The guard runs here, on the host that owns the pane (a prompt
	// forwarded from another host arrives at this handler), and for any
	// target herdr knows, cached or not.
	a := s.resolveAgent(target)
	if a == nil {
		if got, err := s.client.GetAgent(ctx, target); err == nil {
			a = &got
		}
	}
	if a != nil {
		if err := s.guardPromptForce(ctx, *a, body.Force); err != nil {
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
		if full && ev.Agents != nil {
			ev.Agents = s.withBackground(ev.Agents)
		}
		if full {
			payload, err = json.Marshal(ev)
		} else {
			payload, err = json.Marshal(s.slimEvent(ev))
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
		case "agent_blocked", "dialog_open", "input_busy":
			status = http.StatusConflict
		case "pane_not_found", "agent_not_found":
			status = http.StatusNotFound
		case "agent_name_taken", "no_herdr":
			status = http.StatusConflict
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": he.Code, "message": he.Message})
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}
