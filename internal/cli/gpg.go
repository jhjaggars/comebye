package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jhjaggars/comebye/internal/gpgunlock"
	"github.com/jhjaggars/comebye/internal/passkey"
)

// gpgHost is this host's GPG unlock state. The lifetime is fixed at 8 hours;
// COMEBYE_GPG_LIFETIME (a Go duration) exists only so tests can shorten it.
func gpgHost(e *Env) *gpgunlock.Host {
	h := &gpgunlock.Host{Dir: StateDir(e.Getenv), GPG: &gpgunlock.GPG{}}
	if d, err := time.ParseDuration(e.Getenv("COMEBYE_GPG_LIFETIME")); err == nil && d > 0 {
		h.Lifetime = d
	}
	if e.Now != nil {
		h.Now = e.Now
	}
	return h
}

// cmdGPG is `comebye gpg setup|status|unlock|lock|reset|complete|watch`.
func cmdGPG(e *Env, args []string) error {
	if len(args) == 0 {
		return usageErr("want a subcommand: setup, status, unlock, lock, reset, complete or watch")
	}
	rest := args[1:]
	switch args[0] {
	case "setup":
		return gpgSetup(e, rest)
	case "status":
		return gpgStatus(e, rest)
	case "unlock":
		return gpgUnlock(e, rest)
	case "complete":
		return gpgComplete(e, rest)
	case "lock":
		return gpgLock(e, rest)
	case "reset":
		return gpgReset(e, rest)
	case "watch":
		return gpgWatch(e, rest)
	}
	return usageErr("unknown gpg subcommand %q (setup, status, unlock, lock, reset, complete, watch)", args[0])
}

func needGPG(h *gpgunlock.Host) error {
	if !h.GPG.Available() {
		return errors.New("gpg, gpg-connect-agent and gpgconf are needed on this host")
	}
	return nil
}

type hubPasskeys struct {
	RPID     string `json:"rp_id"`
	Passkeys []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		PublicKey   string `json:"public_key"`
		Fingerprint string `json:"fingerprint"`
	} `json:"passkeys"`
}

// gpgSetup: configure the agent, find the key's signing keygrips, show the
// hub's passkeys with fingerprints computed here, and pin them once the user
// says they match what the app shows. The hub's list is never trusted later.
func gpgSetup(e *Env, args []string) error {
	fs := flag.NewFlagSet("gpg setup", flag.ContinueOnError)
	key := fs.String("key", "", "the GPG key's fingerprint (required)")
	host := fs.String("host", "", "this host's name (default: the hostname)")
	yes := fs.Bool("yes", false, "pin the hub's passkeys without asking (you compared the fingerprints yourself)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || *key == "" {
		return usageErr("usage: comebye gpg setup --key FPR [--host NAME] [--yes]")
	}
	h := gpgHost(e)
	if err := needGPG(h); err != nil {
		return err
	}
	name := *host
	if name == "" {
		if name, err = e.Hostname(); err != nil {
			return err
		}
		name = strings.ToLower(strings.SplitN(name, ".", 2)[0])
	}
	grips, err := h.GPG.Keygrips(*key)
	if err != nil {
		return err
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	var hp hubPasskeys
	if err := c.call("GET", "/api/passkeys", nil, nil, &hp); err != nil {
		return err
	}
	if len(hp.Passkeys) == 0 || hp.RPID == "" {
		return errors.New("the hub has no passkey yet: add one in the app (Settings, Passkeys), then run setup again")
	}
	fmt.Fprintf(e.Stdout, "The hub's passkeys, for %s:\n", hp.RPID)
	pinned := gpgunlock.Pinned{RPID: hp.RPID}
	for _, p := range hp.Passkeys {
		fp := passkey.Fingerprint(p.PublicKey) // computed here, not the hub's word
		if fp == "" {
			return fmt.Errorf("passkey %q has an unreadable key", p.Name)
		}
		if _, err := passkey.ParsePublicKey(p.PublicKey); err != nil {
			return fmt.Errorf("passkey %q: %v", p.Name, err)
		}
		fmt.Fprintf(e.Stdout, "  %-24s %s\n", p.Name, fp)
		pinned.Passkeys = append(pinned.Passkeys, gpgunlock.PinnedKey{ID: p.ID, Name: p.Name, PublicKey: p.PublicKey})
	}
	if !*yes {
		fmt.Fprint(e.Stdout, "Do these fingerprints match the app (Settings, Passkeys)? This host will trust only these. [y/N] ")
		line, _ := bufio.NewReader(e.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("not confirmed: nothing was changed")
		}
	}
	if err := h.GPG.ConfigureAgent(gpgLifetime(h)); err != nil {
		return err
	}
	if err := h.Setup(gpgunlock.Config{Host: name, FPR: *key, Keygrips: grips}, pinned); err != nil {
		return err
	}
	hfp, _ := h.HostFingerprint()
	fmt.Fprintf(e.Stdout, "Set up GPG unlock on %s for key %s (%d signing keygrip(s)).\nHost key fingerprint: %s\nNext: run `comebye gpg unlock`, then approve it in the app and type the passphrase once.\n", name, *key, len(grips), hfp)
	return nil
}

func gpgLifetime(h *gpgunlock.Host) time.Duration {
	if h.Lifetime > 0 {
		return h.Lifetime
	}
	return gpgunlock.DefaultLifetime
}

func gpgStatus(e *Env, args []string) error {
	fs := flag.NewFlagSet("gpg status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	h := gpgHost(e)
	if err := needGPG(h); err != nil {
		return err
	}
	st, err := h.Status()
	if errors.Is(err, gpgunlock.ErrNotSetUp) {
		if *asJSON {
			return jsonOut(e, st)
		}
		fmt.Fprintln(e.Stdout, "GPG unlock is not set up on this host (comebye gpg setup --key FPR).")
		return nil
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return jsonOut(e, st)
	}
	state := "locked"
	if !st.Locked {
		state = "unlocked"
		if st.UnlockedTo != nil {
			state += " until " + st.UnlockedTo.Local().Format("15:04")
		}
	}
	fmt.Fprintf(e.Stdout, "GPG on %s: %s\nkey %s, %d passkey(s) pinned, passphrase %s\n", st.Host, state, st.Key, st.Passkeys,
		map[bool]string{true: "stored (sealed)", false: "not stored yet"}[st.HasBlob])
	return nil
}

// gpgUnlock files the request with the hub and says where to approve it. The
// answer comes back through `comebye gpg complete` (the hub relay, a later
// slice, calls it).
func gpgUnlock(e *Env, args []string) error {
	fs := flag.NewFlagSet("gpg unlock", flag.ContinueOnError)
	force := fs.Bool("force", false, "ask even when the key is already unlocked")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	h := gpgHost(e)
	if err := needGPG(h); err != nil {
		return err
	}
	cfg, err := h.Config()
	if err != nil {
		return err
	}
	if !*force {
		if locked, err := h.GPG.Locked(cfg.Keygrips); err == nil && !locked {
			fmt.Fprintln(e.Stdout, "GPG is already unlocked.")
			return nil
		}
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	rp := h.Pinned().RPID
	id, req, err := h.NewRequest(rp, !h.HasBlob())
	if err != nil {
		return err
	}
	var out struct {
		Approval struct {
			ID string `json:"id"`
		} `json:"approval"`
	}
	err = c.call("POST", "/api/approvals", nil, map[string]any{
		"action": "gpg-unlock", "host": cfg.Host, "subject": "Unlock GPG key " + cfg.FPR, "request": json.RawMessage(req),
	}, &out)
	if err != nil {
		return err
	}
	if *asJSON {
		return jsonOut(e, map[string]any{"request": id, "approval": out.Approval.ID})
	}
	fmt.Fprintf(e.Stdout, "Asked for Face ID on %s: approval %s (request %s). It is valid for %d s.\n", cfg.Host, out.Approval.ID, id, int(gpgunlock.RequestTTL.Seconds()))
	return nil
}

// gpgComplete hands the host the app's answer to a request (JSON, from a
// file or stdin) and unlocks if it checks out.
func gpgComplete(e *Env, args []string) error {
	fs := flag.NewFlagSet("gpg complete", flag.ContinueOnError)
	answer := fs.String("answer", "-", "the answer's JSON: a file, or - for standard input")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: comebye gpg complete REQUEST_ID [--answer FILE|-]")
	}
	var r io.Reader = e.Stdin
	if *answer != "-" {
		f, err := os.Open(*answer)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	var a gpgunlock.Answer
	if err := json.NewDecoder(io.LimitReader(r, 64<<10)).Decode(&a); err != nil {
		return fmt.Errorf("the answer is not valid JSON: %v", err)
	}
	h := gpgHost(e)
	if err := needGPG(h); err != nil {
		return err
	}
	st, err := h.Complete(pos[0], a)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "Unlocked until %s.\n", st.ExpiresAt.Local().Format("15:04"))
	return nil
}

func gpgLock(e *Env, args []string) error {
	pos, err := parse(flag.NewFlagSet("gpg lock", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	h := gpgHost(e)
	if err := needGPG(h); err != nil {
		return err
	}
	if err := h.Lock(); err != nil {
		return err
	}
	fmt.Fprintln(e.Stdout, "GPG is locked.")
	return nil
}

func gpgReset(e *Env, args []string) error {
	fs := flag.NewFlagSet("gpg reset", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	h := gpgHost(e)
	if err := needGPG(h); err != nil {
		return err
	}
	if !*yes {
		fmt.Fprint(e.Stdout, "Delete the stored passphrase blob, the pinned passkeys and the host key, and lock GPG? You still know the passphrase; run setup again after. [y/N] ")
		line, _ := bufio.NewReader(e.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("not confirmed: nothing was changed")
		}
	}
	if err := h.Reset(); err != nil {
		return err
	}
	fmt.Fprintln(e.Stdout, "GPG unlock is reset on this host.")
	return nil
}

// gpgWatch runs the lifetime timer in the foreground: it ends an unlock at
// its expiry and, on start, at once if it already passed.
func gpgWatch(e *Env, args []string) error {
	pos, err := parse(flag.NewFlagSet("gpg watch", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	h := gpgHost(e)
	if err := needGPG(h); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h.Run(ctx, func(err error) { fmt.Fprintln(e.Stderr, "gpg watch:", err) })
	return nil
}
