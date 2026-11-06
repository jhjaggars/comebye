// comebye is a small web app for driving herdr agents from a phone.
//
// It talks to herdr's local socket API, serves a phone-friendly UI, and is
// meant to be exposed only through Tailscale Serve (tailnet-only HTTPS).
// See README.md for deployment.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/jhjaggars/comebye/internal/archive"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhjaggars/comebye/internal/agenttoken"
	"github.com/jhjaggars/comebye/internal/cli"
	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/push"
	"github.com/jhjaggars/comebye/internal/server"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/tsauth"
)

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
		allowHosts    = hostList{getenv("COMEBYE_ALLOW_HOSTS")}
		launchRoots   = hostList{envOr("COMEBYE_LAUNCH_ROOTS", "")}
		coordArgs     = hostList{getenv("COMEBYE_COORDINATOR_ARGS")}
		archiveDir    = flag.String("archive-dir", getenv("COMEBYE_ARCHIVE_DIR"), "keep a final copy of an agent's transcript here when it is removed (see `comebye archive`); empty disables")
		archiveS3     = flag.String("archive-s3", getenv("COMEBYE_ARCHIVE_S3"), "keep those copies in this bucket instead: https://HOST/BUCKET (see `comebye archive`)")
		archiveS3Cr   = flag.String("archive-s3-credentials", getenv("COMEBYE_ARCHIVE_S3_CREDENTIALS"), "mode-0600 file of key_id= and secret_key= lines for -archive-s3")
		archiveEnc    = flag.String("archive-encryption", getenv("COMEBYE_ARCHIVE_ENCRYPTION"), "the archive's mode: age or none (plaintext; only where something else encrypts the copy); required with -archive-dir or -archive-s3")
		archiveRcpt   = flag.String("archive-recipient", getenv("COMEBYE_ARCHIVE_RECIPIENT"), "the archive's recipients: age1... and/or ssh-... public keys, or a file of them")
		recallIndex   = flag.String("recall-index", getenv("COMEBYE_RECALL_INDEX"), "the transcript index the app searches, read-only (default ~/.local/state/comebye/recall/index.db; see `comebye archive index`)")
		showVersion   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Var(&coordArgs, "coordinator-args", "extra arguments the app's Coordinator preset passes to an agent kind, as KIND=ARGS (ARGS split on spaces; repeatable, or ';'-separated), e.g. pi=--auto for a Pi install with a permission gate that has --auto")
	flag.Var(&launchRoots, "launch-root", "directory inside which the app may start new agents (repeatable or comma-separated; default ~/code); roots that do not exist are ignored")
	flag.Var(&allowHosts, "allow-host", "extra Host name the server answers to besides localhost, loopback IPs and *.ts.net (repeatable or comma-separated); other Hosts are refused against DNS rebinding")
	flag.Parse()
	if *showVersion {
		fmt.Println("comebye", buildVersion())
		return
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
	client := herdr.NewClient(sockPath)
	// Wait for herdr when it is not up yet: after a reboot herdr's server
	// starts only when the user connects remotely, so its socket may
	// appear minutes or hours later. Waiting here (instead of exiting)
	// keeps systemd from crash-looping comebye into its start limit
	// before that happens. A socket that exists but cannot be reached is
	// a real problem and still fails fast.
	waitForHerdr(client, sockPath, time.Sleep)

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
	if *stateDir == defaultStateDir() {
		migrateStateDir(expandHome(*stateDir))
	}
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
	fl := fleet.New(manager, *hostName, peers, fleet.Options{Version: buildVersion(), HubMode: *hubMode})
	fl.AttachStore(peerStore, addrOpt)
	go fl.Run(context.Background())
	go func() {
		if err := manager.Run(context.Background()); err != nil {
			log.Printf("state manager stopped: %v", err)
		}
	}()

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
	srv := server.New(server.Config{
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
		ReadLines:      1000,
		Push:           pushSvc,
		Presence:       presence,
		Fleet:          fl,
		// Images sent from the app, for agents on this host to read.
		UploadDir:   filepath.Join(expandHome(*stateDir), "uploads"),
		LaunchRoots: launchRootList(*launchOn, launchRoots),
		// Workers' worktrees: <root>/<repo>/<branch>.
		WorktreeRoot: expandHome(*worktreeRoot),
		// The handoff ledger, briefing, roles and handoff-template.md.
		StateDir:        expandHome(*stateDir),
		NoLedger:        !*ledgerOn,
		Upstream:        upstream,
		AgentToken:      agentToken,
		CoordinatorDir:  expandHome(*coordDir),
		CoordinatorArgs: parseKindArgs(coordArgs),
		AllowCap:        allowCap,
		WhoIs:           localAPI,
		SelfRegister:    *selfRegister,
	}, client, manager)
	if allowCap != "" {
		log.Printf("tagged peers admitted through Tailscale Serve with capability %s (agent role only; self-registration %v); Serve needs --accept-app-caps=%s", allowCap, *selfRegister, allowCap)
	}
	if *announce {
		go announceToHub(upstream, localAPI, *hostName, *announceURL)
	}
	go srv.RunLedger(context.Background())
	go srv.RunBriefSchedule(context.Background())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Print("shutting down")
		os.Exit(0)
	}()

	if err := srv.ListenAndServe(); err != nil {
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
