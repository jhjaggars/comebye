// comebye is a small web app for driving herdr agents from a phone.
//
// It talks to herdr's local socket API, serves a phone-friendly UI, and is
// meant to be exposed only through Tailscale Serve (tailnet-only HTTPS).
// See README.md for deployment.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"github.com/jhjaggars/comebye/internal/archive"
	"github.com/jhjaggars/comebye/internal/gitfetch"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhjaggars/comebye/internal/agenttoken"
	"github.com/jhjaggars/comebye/internal/cli"
	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/hublock"
	"github.com/jhjaggars/comebye/internal/push"
	"github.com/jhjaggars/comebye/internal/release"
	"github.com/jhjaggars/comebye/internal/server"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/tsauth"
)

// releaseScript is the release tagging script a person runs by hand
// (docs/releasing.md); the server runs this same text, not a checkout's copy.
//
//go:embed tools/release/tag
var releaseScript string

func main() {
	// "comebye <subcommand> ...": the coordinator CLI, not the server.
	if len(os.Args) > 1 && cli.IsCommand(os.Args[1]) {
		os.Exit(cli.Run(os.Args[1:], cli.OSEnv()))
	}
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage: comebye [flags]          run the server\n       comebye <command> [args]  run a CLI command (agents use these to coordinate)\n\nCommands:\n%s\nServer flags:\n", cli.Summary())
		flag.PrintDefaults()
	}
	var (
		addr          = flag.String("addr", "127.0.0.1:8741", "listen address (keep loopback; use Tailscale Serve for TLS)")
		sock          = flag.String("sock", defaultSocketPath(), "herdr API socket path (default $COMEBYE_SOCK, else $HERDR_SOCKET_PATH, else herdr's default)")
		tsAllow       = flag.String("ts-allow-login", getenv("COMEBYE_TS_ALLOW_LOGIN"), "comma-separated Tailscale logins admitted via Tailscale Serve identity headers (e.g. you@example.com)")
		ntfyBase      = flag.String("ntfy-url", envOr("COMEBYE_NTFY_URL", "https://ntfy.sh"), "ntfy root URL; empty disables ntfy")
		ntfyTopic     = flag.String("ntfy-topic", getenv("COMEBYE_NTFY_TOPIC"), "ntfy topic (optional fallback channel); empty disables ntfy")
		ntfyClick     = flag.String("ntfy-click", getenv("COMEBYE_NTFY_CLICK"), "URL opened when an ntfy notification is tapped (the Serve URL)")
		ntfyThottle   = flag.Duration("ntfy-throttle", 0, "deprecated alias for -notify-throttle")
		ntfyExcerpt   = flag.Int("ntfy-excerpt", envInt("COMEBYE_NTFY_EXCERPT", 0), "max characters of the agent's last message to include in ntfy notifications; 0 (default) sends none, since ntfy.sh topics are public")
		throttle      = flag.Duration("notify-throttle", envDuration("COMEBYE_NOTIFY_THROTTLE", 30*time.Second), "minimum interval between repeat notifications per agent and kind (Web Push and ntfy)")
		notifyKinds   = flag.String("notify-kinds", envOr("COMEBYE_NOTIFY_KINDS", "blocked,question,done,plan"), "which notifications to send (Web Push and ntfy), comma-separated: blocked (needs input), question (finished on a question), done (finished), plan (a plan settled); none for no notifications")
		doneDigest    = flag.Duration("notify-done-digest", envDuration("COMEBYE_NOTIFY_DONE_DIGEST", 0), "collect \"done\" notifications for this long after the first and send them as one (\"3 agents finished\"); 0 sends each at once. Questions are never delayed")
		blockedDelay  = flag.Duration("notify-blocked-delay", envDuration("COMEBYE_NOTIFY_BLOCKED_DELAY", 15*time.Second), "wait this long before notifying that an agent needs input, and send nothing if it stopped being blocked meanwhile; 0 notifies at once")
		stateDir      = flag.String("state-dir", envOr("COMEBYE_STATE_DIR", defaultStateDir()), "directory for the VAPID key and push subscriptions (created mode 0700)")
		pushOn        = flag.Bool("push", envBool("COMEBYE_PUSH", true), "enable Web Push notifications")
		pushSubject   = flag.String("push-subject", getenv("COMEBYE_PUSH_SUBJECT"), "VAPID contact (mailto:you@example.com or an https:// URL); default mailto:<first -ts-allow-login that is an email>, else the project URL")
		pushExcerpt   = flag.Int("push-excerpt", envInt("COMEBYE_PUSH_EXCERPT", 200), "max characters of the agent's last message in Web Push notifications (end-to-end encrypted); 0 sends none")
		hostName      = flag.String("host-name", envOr("COMEBYE_HOST_NAME", fleet.DefaultHostName()), "this host's name in agent IDs and URLs (lowercase DNS label)")
		peersSpec     = flag.String("peers", getenv("COMEBYE_PEERS"), "make this instance a hub over other comebye instances: comma-separated name=https://host.tailnet.ts.net (name optional; several addresses per peer, tried in order, separated by |)")
		noAuth        = flag.Bool("insecure-no-auth", false, "serve without -ts-allow-login: every device that reaches the port (your whole tailnet through Serve) can drive your agents")
		launchOn      = flag.Bool("launch", envBool("COMEBYE_LAUNCH", true), "let the app start new agents in directories under -launch-root")
		worktreeRoot  = flag.String("worktree-root", envOr("COMEBYE_WORKTREE_ROOT", "~/.worktrees"), "where handed-off workers get their git worktrees (<root>/<repo>/<branch>)")
		hubMode       = flag.Bool("hub-mode", envBool("COMEBYE_HUB_MODE", false), "make this instance a hub even with no peers yet, so peers can be added at runtime (comebye peer add); also implied by -peers or stored peers")
		peerDomains   = flag.String("peer-domains", getenv("COMEBYE_PEER_DOMAINS"), "extra tailnet domain suffixes (comma-separated, e.g. mytailnet.example.com) allowed for peers added at runtime; *.ts.net is always allowed; tailnet IPs only as fallback addresses")
		hubURL        = flag.String("hub", getenv("COMEBYE_HUB"), "on a peer: the hub's Tailscale Serve URL (https://hub.tailnet.ts.net); the coordinator CLI's ledger, handoff, mark, briefing and prompt requests are forwarded there and this instance keeps no ledger")
		tsAllowCap    = flag.String("ts-allow-cap", getenv("COMEBYE_TS_ALLOW_CAP"), "on a hub: the Tailscale app capability (example.com/cap/peer) that admits a tagged node through Tailscale Serve (run Serve with --accept-app-caps); the node must also be a registered peer, and gets the forwarded-agent role only")
		removedTags   = flag.String("ts-allow-tag", getenv("COMEBYE_TS_ALLOW_TAGS"), "REMOVED, ignored with a warning: tagged nodes come in through Serve with -ts-allow-cap")
		removedListen = flag.String("peer-listen", getenv("COMEBYE_PEER_LISTEN"), "REMOVED, ignored with a warning: the peer listener (port 8742) is gone; use Serve with -ts-allow-cap")
		selfRegister  = flag.Bool("peer-self-register", envBool("COMEBYE_PEER_SELF_REGISTER", false), "on a hub: let a node holding the -ts-allow-cap capability register itself as a peer (POST /api/peers/announce); needs -ts-allow-cap; off by default")
		tsSocket      = flag.String("ts-socket", envOr("COMEBYE_TS_SOCKET", tsauth.DefaultSocket), "tailscaled's LocalAPI socket (WhoIs for tagged nodes; this node's name for -announce)")
		announce      = flag.Bool("announce", envBool("COMEBYE_ANNOUNCE", false), "on a peer with -hub: announce this node to the hub as a peer at startup (the hub needs -peer-self-register and this node the -ts-allow-cap capability)")
		announceURL   = flag.String("announce-url", getenv("COMEBYE_ANNOUNCE_URL"), "the Serve URL to announce (default https://<this node's MagicDNS name>)")
		ledgerOn      = flag.Bool("ledger", envBool("COMEBYE_LEDGER", true), "keep the handoff ledger, briefing and coordinator roles here (the hub, or a single instance); off on a peer with -hub")
		coordDir      = flag.String("coordinator-dir", envOr("COMEBYE_COORDINATOR_DIR", "~"), "where the app's Coordinator preset starts a coordinator agent (may lie outside the launch roots; only that preset uses it)")
		publicURL     = flag.String("public-url", getenv("COMEBYE_PUBLIC_URL"), "the https URL this hub is reached at (https://hub.tailnet.ts.net); `comebye publish` prints links built from it")
		pagesListen   = flag.String("pages-listen", getenv("COMEBYE_PAGES_LISTEN"), "serve published pages on a second loopback listener (e.g. 127.0.0.1:8742) that has only /p and no /api; put Tailscale Serve on its own port in front (tailscale serve --https=8443 http://127.0.0.1:8742) so pages get an origin of their own; needs -pages-url")
		pagesURL      = flag.String("pages-url", getenv("COMEBYE_PAGES_URL"), "the https URL of that pages listener as the browser reaches it (https://hub.tailnet.ts.net:8443); the app frames pages from it and page links are built from it")
		allowHosts    = hostList{getenv("COMEBYE_ALLOW_HOSTS")}
		launchRoots   = hostList{envOr("COMEBYE_LAUNCH_ROOTS", "")}
		coordArgs     = hostList{getenv("COMEBYE_COORDINATOR_ARGS")}
		defaultKind   = flag.String("default-kind", getenv("COMEBYE_DEFAULT_KIND"), "the agent kind a start on this host uses when the request names none (a seed: settings.json, edited in the app or with `comebye settings`, wins); empty guesses from the running agents")
		allowedKinds  = hostList{getenv("COMEBYE_ALLOWED_KINDS")}
		archiveDir    = flag.String("archive-dir", getenv("COMEBYE_ARCHIVE_DIR"), "keep a final copy of an agent's transcript here when it is removed (see `comebye archive`); empty disables")
		archiveS3     = flag.String("archive-s3", getenv("COMEBYE_ARCHIVE_S3"), "keep those copies in this bucket instead: https://HOST/BUCKET (see `comebye archive`)")
		archiveS3Cr   = flag.String("archive-s3-credentials", getenv("COMEBYE_ARCHIVE_S3_CREDENTIALS"), "mode-0600 file of key_id= and secret_key= lines for -archive-s3")
		archiveEnc    = flag.String("archive-encryption", getenv("COMEBYE_ARCHIVE_ENCRYPTION"), "the archive's mode: age or none (plaintext; only where something else encrypts the copy); required with -archive-dir or -archive-s3")
		archiveRcpt   = flag.String("archive-recipient", getenv("COMEBYE_ARCHIVE_RECIPIENT"), "the archive's recipients: age1... and/or ssh-... public keys, or a file of them")
		recallIndex   = flag.String("recall-index", getenv("COMEBYE_RECALL_INDEX"), "the transcript index the app searches, read-only (default ~/.local/state/comebye/recall/index.db; see `comebye archive index`)")
		releaseRepo   = flag.String("release-repo", getenv("COMEBYE_RELEASE_REPO"), "a dedicated comebye clone this host tags releases from, with its own git and gh credentials (see docs/security.md). On a hub it backs the app's \"Approve release\" button; on a peer (-hub) it lets the hub tag through this host (-release-host there); empty: off")
		releaseHost   = flag.String("release-host", getenv("COMEBYE_RELEASE_HOST"), "on the hub: the peer whose -release-repo the \"Approve release\" button tags through (the hub asks it to check, tag and report the build; it keeps no clone or credentials of its own); not with -release-repo")
		releaseUp     = flag.Bool("release-upgrade", envBool("COMEBYE_RELEASE_UPGRADE", false), "let the app's Upgrade button run `comebye upgrade --version` on this host (a transient systemd user unit)")
		releaseDry    = flag.Bool("release-dry-run", envBool("COMEBYE_RELEASE_DRY_RUN", false), "the release and upgrade buttons do every check and change nothing (no tag, no install)")
		releaseDeploy = flag.String("release-deploy-cmd", getenv("COMEBYE_RELEASE_DEPLOY_CMD"), "on the host that owns -release-repo: a command (absolute program path and arguments, no shell) that the release item's second button runs once the build passed, with the version appended as the last argument and in COMEBYE_RELEASE_VERSION. Approve release never runs it. For example a script that commits the version to a deployment repository; empty: no such button")
		releaseDepLbl = flag.String("release-deploy-label", getenv("COMEBYE_RELEASE_DEPLOY_LABEL"), "the name of that button (default Deploy)")
		releaseSim    = flag.Bool("release-simulate", false, "demo and browser tests: the release button runs nothing at all and answers with canned results")
		autoTodo      = flag.String("auto-todo", envOr("COMEBYE_AUTO_TODO", "blocked,question"), "to-do items the hub files itself, comma-separated: blocked (an agent blocked at a dialog, unwatched, past -auto-todo-blocked-after), question (an agent idle on a question past -auto-todo-question-after, filed as a suggestion to keep or dismiss); off for none")
		autoBlocked   = flag.Duration("auto-todo-blocked-after", envDuration("COMEBYE_AUTO_TODO_BLOCKED_AFTER", server.DefaultAutoBlocked), "how long an agent must be blocked, with no device viewing it, before -auto-todo files an item")
		autoQuestion  = flag.Duration("auto-todo-question-after", envDuration("COMEBYE_AUTO_TODO_QUESTION_AFTER", server.DefaultAutoQuestion), "how long an agent must sit idle or done on a question, with no device viewing it, before -auto-todo suggests an item")
		hubOnly       = flag.Bool("hub-only", envBool("COMEBYE_HUB_ONLY", false), "run as a hub that hosts no agents (no herdr here, nothing waits for a socket): the ledger, to-dos, briefings, plans, peers, archive, push and the app all work; implies -hub-mode. Also implied by an explicitly empty -sock (see docs/setup.md)")
		lockWait      = flag.Duration("lock-wait", envDuration("COMEBYE_LOCK_WAIT", 0), "when another comebye holds the state directory, retry this long before refusing to start (0 refuses at once); a Kubernetes replacement pod can set 30s")
		healthAddr    = flag.String("health-addr", getenv("COMEBYE_HEALTH_ADDR"), "also serve /healthz and /readyz (nothing else, no login) on this address, e.g. :8743, for probes that cannot reach the loopback listener; empty (default) serves them on -addr only")
		showVersion   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Var(&allowedKinds, "allowed-kinds", "the only agent kinds this host starts (comma-separated, e.g. pi); a seed that settings.json overrides; empty allows every installed kind")
	flag.Var(&coordArgs, "coordinator-args", "extra arguments the app's Coordinator preset passes to an agent kind, as KIND=ARGS (ARGS split on spaces; repeatable, or ';'-separated), e.g. pi=--auto for a Pi install with a permission gate that has --auto")
	flag.Var(&launchRoots, "launch-root", "directory inside which the app may start new agents (repeatable or comma-separated; default ~/code); roots that do not exist are ignored")
	flag.Var(&allowHosts, "allow-host", "extra Host name the server answers to besides localhost, loopback IPs and *.ts.net (repeatable or comma-separated); other Hosts are refused against DNS rebinding")
	flag.Parse()
	if *showVersion {
		fmt.Println("comebye", buildVersion())
		return
	}
	// An explicitly empty -sock (flag, not env) says "no herdr here".
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "sock" && strings.TrimSpace(*sock) == "" {
			*hubOnly = true
		}
	})
	if *hubOnly {
		switch {
		case strings.TrimSpace(*hubURL) != "":
			log.Fatalf("-hub-only and -hub: a hub-only instance is the hub, not a peer")
		case !*ledgerOn:
			log.Fatalf("-hub-only and -ledger=false: the ledger is the point of a hub-only instance")
		}
		*hubMode = true
		*launchOn = false
	}
	if *ntfyThottle > 0 {
		*throttle = *ntfyThottle
	}
	kinds, err := state.ParseKinds(*notifyKinds)
	if err != nil {
		log.Fatalf("-notify-kinds: %v", err)
	}
	if *doneDigest < 0 || *blockedDelay < 0 {
		log.Fatalf("-notify-done-digest and -notify-blocked-delay must not be negative")
	}
	autoCfg, err := parseAutoTodo(*autoTodo, *autoBlocked, *autoQuestion)
	if err != nil {
		log.Fatalf("-auto-todo: %v", err)
	}
	logins := splitLogins(*tsAllow)
	if len(logins) == 0 && !*noAuth {
		log.Fatalf("no Tailscale login allowlist: set -ts-allow-login / COMEBYE_TS_ALLOW_LOGIN to your login (or pass -insecure-no-auth to serve an unauthenticated API)")
	}

	if !fleet.ValidName(*hostName) {
		log.Fatalf("-host-name %q: must be a lowercase DNS label (a-z, 0-9, -)", *hostName)
	}
	// TEST ONLY, deliberately not a flag: lets a hub use plain-http
	// loopback peers and (with -insecure-no-auth) run without an
	// allowlist, for wiring two local instances together without Tailscale
	// Serve. Never set it in a real deployment.
	insecureTestPeers := getenv("COMEBYE_TEST_INSECURE_PEERS") == "1"
	peers, err := fleet.ParsePeers(*peersSpec, fleet.ParseOptions{LocalName: *hostName, AllowHTTPLoopback: insecureTestPeers})
	if err != nil {
		log.Fatalf("-peers: %v", err)
	}
	// Peers added at runtime (comebye peer add) live in peers.json in the
	// state dir; they are merged with the static -peers, which win a name
	// clash. Runtime addresses must be tailnet hosts: a peer receives the
	// hub's requests and is trusted with its agents.
	addrOpt := fleet.AddrOptions{AllowHTTPLoopback: insecureTestPeers, Domains: []string{".ts.net"}}
	for _, d := range strings.Split(*peerDomains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			addrOpt.Domains = append(addrOpt.Domains, d)
		}
	}
	if *stateDir == defaultStateDir() {
		migrateStateDir(expandHome(*stateDir))
	}
	// One server per state directory: taken before anything reads or writes
	// it (see internal/hublock and docs/setup.md).
	lock, err := hublock.Acquire(expandHome(*stateDir), hublock.Options{Name: *hostName, Addr: *addr, Wait: *lockWait, Logf: log.Printf})
	if err != nil {
		log.Fatalf("%v", err)
	}
	peerStore, err := fleet.OpenStore(expandHome(*stateDir))
	if err != nil {
		log.Fatalf("peers: %v", err)
	}
	staticPeers := peers
	peers = append(append([]fleet.Peer(nil), staticPeers...), fleet.LoadDynamic(peerStore, staticPeers, *hostName, addrOpt, log.Printf)...)
	if len(peers) > 0 || *hubMode {
		if len(logins) == 0 && !insecureTestPeers {
			log.Fatalf("-peers / -hub-mode needs -ts-allow-login: a hub drives every peer, so it must not be open to the whole tailnet")
		}
		if insecureTestPeers {
			log.Printf("WARNING: COMEBYE_TEST_INSECURE_PEERS=1 (test only): http loopback peers and no-allowlist hubs are allowed")
		}
		for _, p := range peers {
			if len(p.URLs) > 1 {
				log.Printf("hub: peer %s at %s (addresses tried in order)", p.Name, p.Addrs())
			} else {
				log.Printf("hub: peer %s at %s", p.Name, p.Addrs())
			}
		}
	}

	// The plain-HTTP peer listener (port 8742) and its tag allowlist were
	// removed: tagged nodes come in through Serve with the app capability.
	// Warn and carry on rather than exit, so a hub upgraded before its env
	// file is cleaned up keeps running (nothing is listening on the old
	// port, and nothing is admitted that was not already admitted by the
	// capability).
	if strings.TrimSpace(*removedListen) != "" {
		log.Printf("WARNING: -peer-listen / COMEBYE_PEER_LISTEN is removed and ignored: the peer listener is gone; tagged nodes use the Serve URL with -ts-allow-cap (run Serve with --accept-app-caps)")
	}
	if strings.TrimSpace(*removedTags) != "" {
		log.Printf("WARNING: -ts-allow-tag / COMEBYE_TS_ALLOW_TAGS is removed and ignored: tags no longer admit anything; set -ts-allow-cap / COMEBYE_TS_ALLOW_CAP and a Tailscale grant carrying that capability")
	}
	allowCap, err := tsauth.NormalizeCap(*tsAllowCap)
	if err != nil {
		log.Fatalf("-ts-allow-cap: %v", err)
	}
	switch {
	case allowCap != "" && !(len(peers) > 0 || *hubMode):
		log.Fatalf("-ts-allow-cap is for a hub: set -hub-mode (or -peers)")
	case *selfRegister && allowCap == "":
		log.Fatalf("-peer-self-register needs -ts-allow-cap")
	case *announce && strings.TrimSpace(*hubURL) == "":
		log.Fatalf("-announce needs -hub")
	}
	localAPI := &tsauth.LocalAPI{Socket: *tsSocket}

	var upstream *fleet.Upstream
	if strings.TrimSpace(*hubURL) != "" {
		if len(peers) > 0 || *hubMode {
			log.Fatalf("-hub and -peers/-hub-mode: an instance is either the hub (with peers) or a peer (with a hub), not both")
		}
		upstream, err = fleet.NewUpstream(*hubURL, insecureTestPeers, nil)
		if err != nil {
			log.Fatalf("-hub: %v", err)
		}
		log.Printf("peer: the coordinator CLI's requests go to the hub at %s; no ledger here", upstream.URL())
	}

	sockPath := expandHome(*sock)
	if *hubOnly {
		sockPath = ""
	}
	client := herdr.NewClient(sockPath)
	// Wait for herdr when it is not up yet: after a reboot herdr's server
	// starts only when the user connects remotely, so its socket may
	// appear minutes or hours later. Waiting here (instead of exiting)
	// keeps systemd from crash-looping comebye into its start limit
	// before that happens. A socket that exists but cannot be reached is
	// a real problem and still fails fast.
	if *hubOnly {
		log.Printf("hub-only: no herdr; this host (%s) hosts no agents, everything else runs", *hostName)
	} else {
		waitForHerdr(client, sockPath, time.Sleep)
	}

	notifier := &state.Notifier{
		BaseURL:     *ntfyBase,
		Topic:       *ntfyTopic,
		ClickURL:    *ntfyClick,
		MinInterval: *throttle,
		// Bounded excerpt of the agent's last message; 0 keeps message
		// text off the (by default public) ntfy server.
		ExcerptChars: *ntfyExcerpt,
	}

	manager := state.New(client, notifier)
	presence := push.NewPresence()
	var pushSvc *push.Service
	if *pushOn {
		subject := *pushSubject
		if subject == "" {
			subject = push.DefaultSubjectFor(logins)
		}
		dir := expandHome(*stateDir)
		var err error
		pushSvc, err = push.New(push.Config{
			Dir:          dir,
			Subject:      subject,
			ExcerptChars: *pushExcerpt,
			MinInterval:  *throttle,
		})
		if err != nil {
			log.Fatalf("web push: %v (fix it, or disable with -push=false)", err)
		}
		log.Printf("web push enabled: state %s, subject %s, %d subscription(s)", dir, subject, pushSvc.Store().Len())
	}
	// The agent token lets the comebye CLI (run by a coordinator on this
	// host) act as the restricted agent role.
	agentToken, err := agenttoken.LoadOrCreate(expandHome(*stateDir))
	if err != nil {
		log.Printf("agent token: %v (the comebye CLI will not work on this host)", err)
	}
	// Avoid a typed-nil interface: a nil *push.Service is "no pusher".
	if pushSvc != nil {
		manager.SetPush(pushSvc, presence)
	} else {
		manager.SetPush(nil, presence)
	}
	// The fleet merges this host with its peers (none on a plain
	// instance) and owns the browser broadcasts. Peer notifications share
	// the local dispatcher: same channels, throttle and presence.
	manager.Dispatcher().ShowHost = len(peers) > 0 || *hubMode
	manager.Dispatcher().Kinds = kinds
	manager.Dispatcher().DoneDigest = *doneDigest
	manager.Dispatcher().BlockedDelay = *blockedDelay
	log.Printf("notifications: %s; blocked delay %v; done digest %v", *notifyKinds, *blockedDelay, *doneDigest)
	// What git has reported as damaged in the repositories agents work in here,
	// and the daily fsck that looks for it. Shown on the host, and to the hub.
	repoHealth := gitfetch.OpenHealth(filepath.Join(expandHome(*stateDir), "repo-health.json"), log.Printf)
	fl := fleet.New(manager, *hostName, peers, fleet.Options{Version: buildVersion(), HubMode: *hubMode, NoLocalAgents: *hubOnly, RepoDamage: repoHealth.List})
	repoHealth.OnChange = fl.Changed
	fl.AttachStore(peerStore, addrOpt)
	go fl.Run(context.Background())
	if !*hubOnly {
		go func() {
			if err := manager.Run(context.Background()); err != nil {
				log.Printf("state manager stopped: %v", err)
			}
		}()
	}

	var arch *archive.Archiver
	acfg := archive.Config{
		Encryption: *archiveEnc, Dir: *archiveDir, Recipient: *archiveRcpt, Host: *hostName,
		S3: *archiveS3, S3Credentials: *archiveS3Cr, S3Region: getenv("COMEBYE_ARCHIVE_S3_REGION"), S3Plaintext: getenv("COMEBYE_ARCHIVE_S3_PLAINTEXT"),
		ClaudeDir: getenv("COMEBYE_ARCHIVE_CLAUDE_DIR"), PiDir: getenv("COMEBYE_ARCHIVE_PI_DIR"),
	}
	if acfg.Configured() {
		var err error
		if arch, err = acfg.OpenDeferred(); err != nil {
			log.Fatalf("archive: %v", err)
		}
		log.Printf("archive: final transcript copies go to %s", *archiveDir+*archiveS3)
	}
	if err := releaseFlagError(*noAuth, *releaseRepo, *releaseHost, *releaseUp, *releaseSim, *addr, !*ledgerOn, strings.TrimSpace(*hubURL) != ""); err != nil {
		log.Fatal(err)
	}
	deployCmd, err := releaseDeployCmd(*releaseDeploy, *releaseRepo, *releaseHost, *releaseSim)
	if err != nil {
		log.Fatal(err)
	}
	if len(deployCmd) > 0 {
		log.Printf("release: deploy command loaded: %s (%d argument(s) not shown)", filepath.Base(deployCmd[0]), len(deployCmd)-1)
	}
	var relCfg server.ReleaseConfig
	switch {
	case *releaseSim:
		relCfg = server.ReleaseConfig{Tool: &release.Simulated{}, Upgrade: true, Simulated: true, Runner: server.SimulatedUpgrade{}}
		log.Printf("release: SIMULATED (canned results, nothing runs)")
	default:
		relCfg = server.ReleaseConfig{Upgrade: *releaseUp, DryRun: *releaseDry, DeployCmd: deployCmd, DeployLabel: strings.TrimSpace(*releaseDepLbl)}
		if h := strings.TrimSpace(*releaseHost); h != "" {
			// The hub asks that peer to tag; it must be a peer we know now.
			if err := releaseHostError(h, *hostName, fl.HasPeer); err != nil {
				log.Fatal(err)
			}
			relCfg.Host = h
			log.Printf("release: the app tags releases through the peer %s (dry run here %v; the peer has its own flag)", h, *releaseDry)
		}
		if r := strings.TrimSpace(*releaseRepo); r != "" {
			repo := expandHome(r)
			if st, err := os.Stat(filepath.Join(repo, ".git")); err != nil || st == nil {
				log.Fatalf("-release-repo %q: not a git checkout", repo)
			}
			relCfg.Tool = &release.Script{Source: releaseScript, Repo: repo}
			log.Printf("release: the app may tag releases from %s (dry run %v)", repo, *releaseDry)
		}
		if *releaseUp {
			log.Printf("release: the app may upgrade this host (dry run %v)", *releaseDry)
		}
	}
	if relCfg.Upgrade {
		if exe, err := os.Executable(); err == nil {
			relCfg.Bin = exe
		}
	}
	srv := server.New(server.Config{
		Release:        relCfg,
		Version:        buildVersion(),
		Archive:        arch,
		ArchiveSources: acfg.Sources(),
		RecallIndex:    (&archive.Config{Index: *recallIndex}).IndexPath(),
		Addr:           *addr,
		AllowLogins:    logins,
		// Checked again by ListenAndServe, so a library caller cannot
		// end up open by accident either.
		InsecureNoAuth: *noAuth,
		AllowHosts:     server.SplitHosts(allowHosts...),
		PublicURL:      strings.TrimRight(strings.TrimSpace(*publicURL), "/"),
		PagesURL:       strings.TrimRight(strings.TrimSpace(*pagesURL), "/"),
		ReadLines:      1000,
		Push:           pushSvc,
		Presence:       presence,
		AutoTodo:       autoCfg,
		Fleet:          fl,
		// Images sent from the app, for agents on this host to read.
		UploadDir:   filepath.Join(expandHome(*stateDir), "uploads"),
		LaunchRoots: launchRootList(*launchOn, launchRoots),
		// Workers' worktrees: <root>/<repo>/<branch>.
		WorktreeRoot: expandHome(*worktreeRoot),
		// The handoff ledger, briefing, roles and handoff-template.md.
		StateDir:        expandHome(*stateDir),
		RepoHealth:      repoHealth,
		NoLedger:        !*ledgerOn,
		Upstream:        upstream,
		AgentToken:      agentToken,
		CoordinatorDir:  expandHome(*coordDir),
		CoordinatorArgs: parseKindArgs(coordArgs),
		DefaultKind:     checkKind("-default-kind", strings.ToLower(strings.TrimSpace(*defaultKind))),
		AllowedKinds:    parseKinds("-allowed-kinds", allowedKinds),
		AllowCap:        allowCap,
		WhoIs:           localAPI,
		SelfRegister:    *selfRegister,
		HubOnly:         *hubOnly,
		LockErr:         lock.Err,
	}, client, manager)
	if allowCap != "" {
		log.Printf("tagged peers admitted through Tailscale Serve with capability %s (agent role only; self-registration %v); Serve needs --accept-app-caps=%s", allowCap, *selfRegister, allowCap)
	}
	if *announce {
		go announceToHub(upstream, localAPI, *hostName, *announceURL)
	}
	go srv.RunLedger(context.Background())
	go srv.RunAutoTodo(context.Background())
	go srv.RunRepoHealth(context.Background())
	go srv.RunBriefSchedule(context.Background())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Print("shutting down")
		srv.ShuttingDown()
		lock.Release()
		os.Exit(0)
	}()

	if *healthAddr != "" {
		go func() {
			hs := &http.Server{Addr: *healthAddr, Handler: srv.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
			log.Printf("health probes (/healthz, /readyz only) on http://%s", *healthAddr)
			if err := hs.ListenAndServe(); err != nil {
				log.Fatalf("-health-addr: %v", err)
			}
		}()
	}
	if *pagesListen != "" {
		if strings.TrimSpace(*pagesURL) == "" {
			log.Fatalf("-pages-listen needs -pages-url: the app frames pages from that origin")
		}
		go func() { log.Fatalf("pages listener: %v", srv.ListenPages(*pagesListen)) }()
	}
	if err := srv.ListenAndServe(); err != nil {
		lock.Release()
		log.Fatal(err)
	}
}

// waitForHerdr blocks until herdr answers on its socket, or fatals when
// the socket exists but herdr cannot be reached (a misconfiguration, not
// a boot race). sleep paces the retries (tests shrink it).
func waitForHerdr(client *herdr.Client, sockPath string, sleep func(time.Duration)) {
	backoff := time.Second
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := client.ListAgents(ctx)
		cancel()
		if err == nil {
			return
		}
		if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
			log.Fatalf("cannot reach herdr at %s: %v", sockPath, err)
		}
		log.Printf("waiting for herdr at %s (it starts when you connect remotely): %v", sockPath, err)
		sleep(backoff)
		if backoff *= 2; backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}

// hostList is a repeatable list flag (-allow-host, -launch-root); it
// starts with its environment variable and each value may be
// comma-separated.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }

func (h *hostList) Set(v string) error {
	*h = append(*h, v)
	return nil
}

// parseKindArgs turns -coordinator-args values (KIND=ARGS, repeatable or
// ';'-separated) into extra arguments per agent kind. A malformed entry is
// fatal: a silently dropped flag would start agents unexpectedly.
func parseKindArgs(values hostList) map[string][]string {
	out := map[string][]string{}
	for _, v := range values {
		for _, entry := range strings.Split(v, ";") {
			if entry = strings.TrimSpace(entry); entry == "" {
				continue
			}
			kind, args, ok := strings.Cut(entry, "=")
			kind = strings.TrimSpace(kind)
			if !ok || kind == "" {
				log.Fatalf("-coordinator-args %q: want KIND=ARGS, e.g. pi=--auto", entry)
			}
			out[kind] = append(out[kind], strings.Fields(args)...)
		}
	}
	return out
}

// parseKinds turns a comma-separated kind list flag into kinds; one herdr
// does not know is fatal, like a malformed -coordinator-args.
func parseKinds(flagName string, values hostList) []string {
	var out []string
	for _, v := range values {
		for _, k := range strings.Split(v, ",") {
			if k = checkKind(flagName, strings.ToLower(strings.TrimSpace(k))); k != "" && !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

func checkKind(flagName, k string) string {
	if k != "" && !slices.Contains(herdr.AgentKinds, k) {
		log.Fatalf("%s: %q is not an agent kind (%s)", flagName, k, strings.Join(herdr.AgentKinds, ", "))
	}
	return k
}

// launchRootList expands the -launch-root values (default ~/code); nil
// when launching is off.
func launchRootList(on bool, roots hostList) []string {
	if !on {
		return nil
	}
	var out []string
	for _, v := range roots {
		for _, r := range strings.Split(v, ",") {
			if r = strings.TrimSpace(r); r != "" {
				out = append(out, filepath.Clean(expandHome(r)))
			}
		}
	}
	if len(out) == 0 {
		out = []string{expandHome("~/code")}
	}
	var abs []string
	for _, r := range out {
		if filepath.IsAbs(r) {
			abs = append(abs, r)
		} else {
			log.Printf("ignoring -launch-root %q: not an absolute path", r)
		}
	}
	return abs
}

func splitLogins(s string) []string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// getenv reads a COMEBYE_* variable, falling back to its pre-rename
// HERDRPHONE_* name (logged once) so existing env files keep working.
func getenv(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	legacy := "HERDRPHONE_" + strings.TrimPrefix(key, "COMEBYE_")
	v := os.Getenv(legacy)
	if v != "" {
		if _, seen := warnedLegacy.LoadOrStore(legacy, true); !seen {
			log.Printf("%s is deprecated; rename it to %s", legacy, key)
		}
	}
	return v
}

var warnedLegacy sync.Map

func envOr(key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		log.Printf("ignoring %s=%q: not a boolean", key, v)
	}
	return def
}

// defaultStateDir is $XDG_STATE_HOME/comebye, else
// ~/.local/state/comebye.
// version is stamped by release builds (-ldflags "-X main.version=v1.2.3").
var version = ""

// buildVersion reports the release version, else the module version that
// go install recorded, else "dev" for a plain checkout build.
func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// defaultSocketPath is the -sock default: COMEBYE_SOCK, then herdr's own
// HERDR_SOCKET_PATH (set inside herdr panes, so a comebye started there
// finds that herdr), then herdr's default location.
func defaultSocketPath() string {
	if v := getenv("COMEBYE_SOCK"); v != "" {
		return v
	}
	return envOrRaw("HERDR_SOCKET_PATH", herdr.DefaultSocketPath)
}

// envOrRaw is envOr for variables outside the COMEBYE_* namespace.
func envOrRaw(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "comebye")
	}
	return "~/.local/state/comebye"
}

// migrateStateDir moves the pre-rename state directory (a sibling named
// herdr-phone) to dir when dir does not exist yet, so the VAPID key and push
// subscriptions carry over and phones keep receiving notifications.
func migrateStateDir(dir string) {
	legacy := filepath.Join(filepath.Dir(dir), "herdr-phone")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		return
	}
	if fi, err := os.Stat(legacy); err != nil || !fi.IsDir() {
		return
	}
	if err := os.Rename(legacy, dir); err != nil {
		log.Printf("state dir: could not move %s to %s: %v", legacy, dir, err)
		return
	}
	log.Printf("state dir: moved %s to %s", legacy, dir)
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("ignoring %s=%q: not a duration", key, v)
	}
	return def
}

func envInt(key string, def int) int {
	if v := getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("ignoring %s=%q: not an integer", key, v)
	}
	return def
}

func isLoopbackListen(addr string) bool {
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

// releaseFlagError refuses the release flags in combinations that would
// leave the buttons open. They act as the user with the user's credentials,
// so they are never offered without an identity allowlist: with
// -insecure-no-auth anyone who reaches the port (and the CSRF token, which
// then protects nothing) could tag or upgrade. -release-simulate runs
// nothing, so it may go with -insecure-no-auth (the browser tests do), but
// not with the real flags, and only on loopback.
func releaseFlagError(noAuth bool, repo, host string, upgrade, simulate bool, addr string, noLedger, hasHub bool) error {
	repo, host = strings.TrimSpace(repo), strings.TrimSpace(host)
	real := repo != "" || host != "" || upgrade
	switch {
	case noAuth && real:
		return errors.New("-release-repo / -release-host / -release-upgrade need -ts-allow-login: they cannot be combined with -insecure-no-auth")
	case repo != "" && host != "":
		return errors.New("-release-repo and -release-host together are ambiguous: tag from a clone here, or through a peer's clone, not both")
	case host != "" && (noLedger || hasHub):
		return errors.New("-release-host is for the hub (it keeps the ledger and has no -hub): the peer that owns the clone sets -release-repo")
	case repo != "" && noLedger && !hasHub:
		// As for -release-upgrade below: a host with no ledger is a peer, and
		// the hub-call route to its clone is open on it.
		return errors.New("-release-repo with -ledger=false needs -hub: a host with no ledger is a peer, tagging for its hub")
	case upgrade && noLedger && !hasHub:
		// A host with no ledger is a peer (the hub's upgrade route is open on
		// it); with no hub to ask it, nothing should be reaching that route.
		return errors.New("-release-upgrade with -ledger=false needs -hub: a host with no ledger is a peer, upgraded by its hub")
	case simulate && (real || !isLoopbackListen(addr)):
		return errors.New("-release-simulate is for demos and tests: not with -release-repo, -release-host or -release-upgrade, and only on a loopback address")
	}
	return nil
}

// releaseDeployCmd parses -release-deploy-cmd. The command runs as this
// server's user on the user's click, so it must name its program by absolute
// path (no PATH lookup), runs without a shell, and belongs on the host that
// owns the release clone: not on a hub that tags through -release-host (the
// peer holds it), and not with -release-simulate.
func releaseDeployCmd(spec, repo, host string, simulate bool) ([]string, error) {
	argv := strings.Fields(spec)
	switch {
	case len(argv) == 0:
		return nil, nil
	case strings.TrimSpace(repo) == "":
		return nil, errors.New("-release-deploy-cmd goes with -release-repo: the host that tags runs the deploy (on a hub that uses -release-host, set it on that peer)")
	case strings.TrimSpace(host) != "":
		return nil, errors.New("-release-deploy-cmd and -release-host together: the peer that owns the clone runs the deploy; set it there")
	case simulate:
		return nil, errors.New("-release-deploy-cmd cannot be combined with -release-simulate")
	case !filepath.IsAbs(argv[0]):
		return nil, fmt.Errorf("-release-deploy-cmd: the program %q must be an absolute path", argv[0])
	}
	return argv, nil
}

// releaseHostError refuses a -release-host that is not a peer this hub knows
// (a typo, this host itself, a peer not added yet): the button would
// otherwise fail at the first click.
func releaseHostError(host, local string, hasPeer func(string) bool) error {
	if !fleet.ValidName(host) || host == local || !hasPeer(host) {
		return fmt.Errorf("-release-host %q: not a peer of this hub (add the peer first; this host is %s)", host, local)
	}
	return nil
}

// parseAutoTodo reads -auto-todo: a comma list of detectors, or off.
func parseAutoTodo(spec string, blockedAfter, questionAfter time.Duration) (server.AutoTodoConfig, error) {
	var cfg server.AutoTodoConfig
	for _, k := range strings.Split(spec, ",") {
		switch k = strings.ToLower(strings.TrimSpace(k)); k {
		case "", "off", "none":
		case "blocked":
			if blockedAfter <= 0 {
				return cfg, fmt.Errorf("-auto-todo-blocked-after must be positive")
			}
			cfg.Blocked = blockedAfter
		case "question":
			if questionAfter <= 0 {
				return cfg, fmt.Errorf("-auto-todo-question-after must be positive")
			}
			cfg.Question = questionAfter
		default:
			return cfg, fmt.Errorf("unknown detector %q (want blocked, question, or off)", k)
		}
	}
	return cfg, nil
}
