// comebye is a small web app for driving herdr agents from a phone.
//
// It talks to herdr's local socket API, serves a phone-friendly UI, and is
// meant to be exposed only through Tailscale Serve (tailnet-only HTTPS).
// See README.md for deployment.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/push"
	"github.com/jhjaggars/comebye/internal/server"
	"github.com/jhjaggars/comebye/internal/state"
)

func main() {
	var (
		addr         = flag.String("addr", "127.0.0.1:8741", "listen address (keep loopback; use Tailscale Serve for TLS)")
		sock         = flag.String("sock", herdr.DefaultSocketPath, "herdr API socket path")
		tsAllow      = flag.String("ts-allow-login", getenv("COMEBYE_TS_ALLOW_LOGIN"), "comma-separated Tailscale logins admitted via Tailscale Serve identity headers (e.g. you@example.com)")
		ntfyBase     = flag.String("ntfy-url", envOr("COMEBYE_NTFY_URL", "https://ntfy.sh"), "ntfy root URL; empty disables ntfy")
		ntfyTopic    = flag.String("ntfy-topic", getenv("COMEBYE_NTFY_TOPIC"), "ntfy topic (optional fallback channel); empty disables ntfy")
		ntfyClick    = flag.String("ntfy-click", getenv("COMEBYE_NTFY_CLICK"), "URL opened when an ntfy notification is tapped (the Serve URL)")
		ntfyThottle  = flag.Duration("ntfy-throttle", 0, "deprecated alias for -notify-throttle")
		ntfyExcerpt  = flag.Int("ntfy-excerpt", envInt("COMEBYE_NTFY_EXCERPT", 0), "max characters of the agent's last message to include in ntfy notifications; 0 (default) sends none, since ntfy.sh topics are public")
		throttle     = flag.Duration("notify-throttle", 30*time.Second, "minimum interval between repeat notifications per agent and kind (Web Push and ntfy)")
		stateDir     = flag.String("state-dir", envOr("COMEBYE_STATE_DIR", defaultStateDir()), "directory for the VAPID key and push subscriptions (created mode 0700)")
		pushOn       = flag.Bool("push", envBool("COMEBYE_PUSH", true), "enable Web Push notifications")
		pushSubject  = flag.String("push-subject", getenv("COMEBYE_PUSH_SUBJECT"), "VAPID contact (mailto:you@example.com or an https:// URL); default mailto:<first -ts-allow-login that is an email>, else the project URL")
		pushExcerpt  = flag.Int("push-excerpt", envInt("COMEBYE_PUSH_EXCERPT", 200), "max characters of the agent's last message in Web Push notifications (end-to-end encrypted); 0 sends none")
		hostName     = flag.String("host-name", envOr("COMEBYE_HOST_NAME", fleet.DefaultHostName()), "this host's name in agent IDs and URLs (lowercase DNS label)")
		peersSpec    = flag.String("peers", getenv("COMEBYE_PEERS"), "make this instance a hub over other comebye instances: comma-separated name=https://host.tailnet.ts.net (name defaults to the first DNS label)")
		noAuth       = flag.Bool("insecure-no-auth", false, "serve without -ts-allow-login: every device that reaches the port (your whole tailnet through Serve) can drive your agents")
		launchOn     = flag.Bool("launch", envBool("COMEBYE_LAUNCH", true), "let the app start new agents in directories under -launch-root")
		worktreeRoot = flag.String("worktree-root", envOr("COMEBYE_WORKTREE_ROOT", "~/.worktrees"), "where handed-off workers get their git worktrees (<root>/<repo>/<branch>)")
		allowHosts   = hostList{getenv("COMEBYE_ALLOW_HOSTS")}
		launchRoots  = hostList{envOr("COMEBYE_LAUNCH_ROOTS", "")}
	)
	flag.Var(&launchRoots, "launch-root", "directory inside which the app may start new agents (repeatable or comma-separated; default ~/code); roots that do not exist are ignored")
	flag.Var(&allowHosts, "allow-host", "extra Host name the server answers to besides localhost, loopback IPs and *.ts.net (repeatable or comma-separated); other Hosts are refused against DNS rebinding")
	flag.Parse()
	if *ntfyThottle > 0 {
		*throttle = *ntfyThottle
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
	if len(peers) > 0 {
		if len(logins) == 0 && !insecureTestPeers {
			log.Fatalf("-peers needs -ts-allow-login: a hub drives every peer, so it must not be open to the whole tailnet")
		}
		if insecureTestPeers {
			log.Printf("WARNING: COMEBYE_TEST_INSECURE_PEERS=1 (test only): http loopback peers and no-allowlist hubs are allowed")
		}
		for _, p := range peers {
			log.Printf("hub: peer %s at %s", p.Name, p.URL)
		}
	}

	sockPath := expandHome(*sock)
	client := herdr.NewClient(sockPath)

	// Fail fast if herdr is not reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := client.ListAgents(ctx); err != nil {
		cancel()
		log.Fatalf("cannot reach herdr at %s: %v", sockPath, err)
	}
	cancel()

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
	// Avoid a typed-nil interface: a nil *push.Service is "no pusher".
	if pushSvc != nil {
		manager.SetPush(pushSvc, presence)
	} else {
		manager.SetPush(nil, presence)
	}
	// The fleet merges this host with its peers (none on a plain
	// instance) and owns the browser broadcasts. Peer notifications share
	// the local dispatcher: same channels, throttle and presence.
	manager.Dispatcher().ShowHost = len(peers) > 0
	fl := fleet.New(manager, *hostName, peers, fleet.Options{})
	go fl.Run(context.Background())
	go func() {
		if err := manager.Run(context.Background()); err != nil {
			log.Printf("state manager stopped: %v", err)
		}
	}()

	srv := server.New(server.Config{
		Addr:        *addr,
		AllowLogins: logins,
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
		// The handoff ledger and handoff-template.md.
		StateDir: expandHome(*stateDir),
	}, client, manager)
	go srv.RunLedger(context.Background())

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

// hostList is a repeatable list flag (-allow-host, -launch-root); it
// starts with its environment variable and each value may be
// comma-separated.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }

func (h *hostList) Set(v string) error {
	*h = append(*h, v)
	return nil
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

func envInt(key string, def int) int {
	if v := getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("ignoring %s=%q: not an integer", key, v)
	}
	return def
}
