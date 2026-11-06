package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keygen, run, verify, index, recall and delete, end to end on fixture dirs.
func TestArchiveAndRecallCommands(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
	d := t.TempDir()
	sid := "33333333-3333-4333-8333-333333333333"
	sess := filepath.Join(d, "claude", "-proj", sid+".jsonl")
	os.MkdirAll(filepath.Dir(sess), 0o755)
	os.WriteFile(sess, []byte(`{"type":"ai-title","sessionId":"`+sid+`","aiTitle":"Plan the october migration"}`+"\n"+
		`{"type":"user","sessionId":"`+sid+`","cwd":"/w/proj","gitBranch":"main","timestamp":"2026-10-07T08:00:00Z","message":{"role":"user","content":"remember the zebra migration"}}`+"\n"), 0o644)
	os.Chtimes(sess, mtime(), mtime())
	ledger := filepath.Join(d, "ledger.json")
	os.WriteFile(ledger, []byte(`{"entries":[]}`), 0o644)

	run := func(args ...string) (string, error) {
		var out, errb bytes.Buffer
		env := &Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Getenv: func(k string) string { return "" }}
		if args[0] == "recall" {
			args = append(args[:2:2], append([]string{"--index", filepath.Join(d, "idx.db")}, args[2:len(args)-2]...)...)
		} else {
			args = append(args[:2:2], append([]string{"--encryption", "age", "--dir", filepath.Join(d, "store"), "--identity", filepath.Join(d, "keys", "identity.key"),
				"--claude-dir", filepath.Join(d, "claude"), "--pi-dir", filepath.Join(d, "pi"), "--index", filepath.Join(d, "idx.db"), "--host", "laharl"}, args[2:]...)...)
		}
		if st := Run(args, env); st != 0 {
			return out.String() + errb.String(), fmt.Errorf("exit %d", st)
		}
		return out.String(), nil
	}
	if out, err := run("archive", "keygen", "--out", filepath.Join(d, "keys")); err != nil {
		t.Fatal(out)
	}
	rcpt, _ := os.ReadFile(filepath.Join(d, "keys", "recipient.pub"))
	os.Setenv("X", "")
	runR := func(args ...string) (string, error) {
		return run(append(args, "--recipient", strings.TrimSpace(string(rcpt)))...)
	}
	if out, err := runR("archive", "run"); err != nil || !strings.Contains(out, "1 segments written") {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := runR("archive", "verify"); err != nil || !strings.Contains(out, "1 match their archive") {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := runR("archive", "index", "--ledger", ledger); err != nil || !strings.Contains(out, "1 indexed") {
		t.Fatalf("%v %s", err, out)
	}
	out, err := runR("recall", "search", "zebra", "--since", "2026-10-01", "--until", "2026-11-01")
	if err != nil || !strings.Contains(out, "33333333") || !strings.Contains(out, "october migration") || strings.Contains(out, "nothing found") {
		t.Fatalf("%v %s", err, out)
	}
	if out, err = runR("recall", "ask", "we once planned the zebra migration, what happened"); err != nil || !strings.Contains(out, "october migration") || !strings.Contains(out, "[t-") || !strings.Contains(out, "outcome:") {
		t.Fatalf("%v %s", err, out)
	}
	if out, _ = runR("recall", "list", "--since", "2026-11-01"); !strings.Contains(out, "nothing found") {
		t.Fatalf("%s", out)
	}
	if out, err = runR("archive", "delete", "3333"); err != nil || !strings.Contains(out, "would delete") {
		t.Fatalf("%v %s", err, out)
	}
	if out, err = runR("archive", "delete", "3333", "--yes"); err != nil || !strings.Contains(out, "deleted claude") {
		t.Fatalf("%v %s", err, out)
	}
	if out, _ = runR("recall", "search", "zebra"); !strings.Contains(out, "nothing found") {
		t.Fatalf("deleted session still found: %s", out)
	}
	if out, _ = runR("archive", "run"); !strings.Contains(out, "0 segments written") {
		t.Fatalf("deleted session archived again: %s", out)
	}
}

func mtime() (t time.Time) { return time.Now().Add(-time.Hour) }

// The plaintext mode end to end: explicit choice, no key, verify warns about
// loose permissions, a mixed mode is refused, and rewrap converts to age.
func TestArchivePlaintextModeCommands(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
	d := t.TempDir()
	sid := "55555555-5555-4555-8555-555555555555"
	sess := filepath.Join(d, "claude", "-proj", sid+".jsonl")
	os.MkdirAll(filepath.Dir(sess), 0o755)
	os.WriteFile(sess, []byte(`{"type":"user","sessionId":"`+sid+`","cwd":"/w/proj","gitBranch":"main","timestamp":"2026-10-07T08:00:00Z","message":{"role":"user","content":"plain mode zebra"}}`+"\n"), 0o644)
	os.Chtimes(sess, mtime(), mtime())
	store := filepath.Join(d, "store")
	run := func(args ...string) (string, string, error) {
		var out, errb bytes.Buffer
		env := &Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Getenv: func(k string) string { return "" }}
		full := append(args[:2:2], append([]string{"--claude-dir", filepath.Join(d, "claude"), "--pi-dir", filepath.Join(d, "pi"), "--index", filepath.Join(d, "i", "idx.db"), "--host", "laharl"}, args[2:]...)...)
		if args[0] == "recall" {
			full = args
		}
		if st := Run(full, env); st != 0 {
			return out.String(), errb.String(), fmt.Errorf("exit %d", st)
		}
		return out.String(), errb.String(), nil
	}
	if _, e, err := run("archive", "run", "--dir", store); err == nil || !strings.Contains(e, "explicitly") {
		t.Fatalf("a missing mode must be refused: %v %s", err, e)
	}
	if out, e, err := run("archive", "run", "--dir", store, "--encryption", "none"); err != nil || !strings.Contains(out, "1 segments written") {
		t.Fatalf("%v %s %s", err, out, e)
	}
	if _, e, err := run("archive", "run", "--dir", store, "--encryption", "age", "--recipient", "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"); err == nil {
		t.Fatalf("a mixed mode must be refused: %s", e)
	}
	os.Chmod(store, 0o755)
	out, e, err := run("archive", "verify", "--dir", store, "--encryption", "none")
	if err != nil || !strings.Contains(out, "1 match their archive") || !strings.Contains(e, "group or others") || !strings.Contains(e, "plaintext") {
		t.Fatalf("%v %s | %s", err, out, e)
	}
	os.Chmod(store, 0o700)
	if _, e, _ = run("archive", "verify", "--dir", store, "--encryption", "none"); strings.Contains(e, "group or others") {
		t.Fatalf("a private dir was warned about: %s", e)
	}
	if out, e, err = run("archive", "index", "--dir", store, "--encryption", "none", "--ledger", "none"); err != nil || !strings.Contains(out, "1 indexed") {
		t.Fatalf("%v %s %s", err, out, e)
	}
	if out, _, err = run("recall", "search", "zebra", "--index", filepath.Join(d, "i", "idx.db")); err != nil || strings.Contains(out, "nothing found") {
		t.Fatalf("%v %s", err, out)
	}
	// Convert to age; the new archive verifies with its identity.
	if _, _, err = run("archive", "keygen", "--out", filepath.Join(d, "keys")); err != nil {
		t.Fatal(err)
	}
	store2 := filepath.Join(d, "store2")
	if out, e, err = run("archive", "rewrap", "--dir", store, "--encryption", "none", "--to-dir", store2, "--to-encryption", "age", "--to-recipient", filepath.Join(d, "keys", "recipient.pub")); err != nil || !strings.Contains(out, "none to age") {
		t.Fatalf("%v %s %s", err, out, e)
	}
	if out, e, err = run("archive", "verify", "--dir", store2, "--encryption", "age", "--identity", filepath.Join(d, "keys", "identity.key"), "--recipient", filepath.Join(d, "keys", "recipient.pub")); err != nil || !strings.Contains(out, "1 match their archive") {
		t.Fatalf("%v %s %s", err, out, e)
	}
}

// A worker's sessions live under a host other than this machine's, and every
// command that names a session reaches them with --host.
func TestArchiveCommandsReachAnotherHostWithHostFlag(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
	d := t.TempDir()
	t.Setenv("HOME", d)
	sid := "2026-11-06T10-00-00-000Z_abcd"
	sess := filepath.Join(d, "pi", "--data-w--", sid+".jsonl")
	os.MkdirAll(filepath.Dir(sess), 0o755)
	body := `{"type":"session","version":3,"id":"abcd","timestamp":"2026-11-06T10:00:00Z","cwd":"/data/w"}` + "\n" + `{"partial`
	os.WriteFile(sess, []byte(body), 0o644)
	run := func(args ...string) (string, string, error) {
		var out, errb bytes.Buffer
		env := &Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Getenv: func(k string) string { return "" }}
		if st := Run(args, env); st != 0 {
			return out.String(), errb.String(), fmt.Errorf("exit %d", st)
		}
		return out.String(), errb.String(), nil
	}
	store := filepath.Join(d, "store")
	host := "w1.i-0abc"
	common := []string{"--dir", store, "--encryption", "none", "--pi-dir", filepath.Join(d, "pi"), "--claude-dir", filepath.Join(d, "claude"), "--index", filepath.Join(d, "i", "idx.db")}
	at := func(sub string, h string, more ...string) (string, string, error) {
		a := append([]string{"archive", sub}, common...)
		if h != "" {
			a = append(a, "--host", h)
		}
		return run(append(a, more...)...)
	}
	if out, e, err := at("run", host, "--final"); err != nil || !strings.Contains(out, "1 segments written") {
		t.Fatalf("%v %s %s", err, out, e)
	}
	// Without --host it is not this machine's session, and the error says what to do.
	if _, e, err := at("restore", "laharl", "abcd", "--to", filepath.Join(d, "nope")); err == nil || !strings.Contains(e, "--host") {
		t.Fatalf("%v %s", err, e)
	}
	to := filepath.Join(d, "restored")
	if out, e, err := at("restore", host, "abcd", "--to", to); err != nil {
		t.Fatalf("%v %s %s", err, out, e)
	}
	got, err := os.ReadFile(filepath.Join(to, "--data-w--", sid+".jsonl"))
	if err != nil || string(got) != body {
		t.Fatalf("restored %q, %v (--final must keep the unterminated last line)", got, err)
	}
	if out, e, err := at("summary", host, "abcd", "a summary"); err != nil {
		t.Fatalf("%v %s %s", err, out, e)
	}
	if out, e, err := at("index", host, "--ledger", "none"); err != nil || !strings.Contains(out, "1 indexed") {
		t.Fatalf("%v %s %s", err, out, e)
	}
	out, e, err := run("recall", "list", "--index", filepath.Join(d, "i", "idx.db"), "--host", "w1")
	if err != nil || !strings.Contains(out, host) || !strings.Contains(out, "a summary") {
		t.Fatalf("recall must show the host and the summary: %v %s %s", err, out, e)
	}
	if out, e, err := at("delete", host, "abcd"); err != nil || !strings.Contains(out, "would delete") {
		t.Fatalf("%v %s %s", err, out, e)
	}
	if out, e, err := at("delete", host, "abcd", "--yes"); err != nil || !strings.Contains(out, "deleted") {
		t.Fatalf("%v %s %s", err, out, e)
	}
	if out, e, err := at("verify", host); err != nil || !strings.Contains(out, "0 not archived") {
		t.Fatalf("a deleted session is not a missing one: %v %s %s", err, out, e)
	}
}

// Two hosts in one store: each builds its own index of both, and the commands
// answer for both hosts. All through the CLI.
func TestAllHostsIfChangedAndVerifyCommands(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
	d := t.TempDir()
	store := filepath.Join(d, "store")
	session := func(host, sid, cwd, branch, start, text string) {
		t.Helper()
		p := filepath.Join(d, host, "claude", "-p", sid+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o755)
		line := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"gitBranch":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n", sid, cwd, branch, start, text)
		os.WriteFile(p, []byte(line), 0o644)
		os.Chtimes(p, mtime(), mtime())
	}
	as := func(host string, args ...string) (string, string, error) {
		var out, errb bytes.Buffer
		env := &Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Getenv: func(k string) string { return "" }}
		full := append(args[:2:2], append([]string{"--dir", store, "--encryption", "none", "--host", host,
			"--claude-dir", filepath.Join(d, host, "claude"), "--pi-dir", filepath.Join(d, host, "pi"),
			"--index", filepath.Join(d, host, "state", "index.db")}, args[2:]...)...)
		if st := Run(full, env); st != 0 {
			return out.String(), errb.String(), fmt.Errorf("exit %d", st)
		}
		return out.String(), errb.String(), nil
	}
	recall := func(host string, args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		env := &Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Getenv: func(k string) string { return "" }}
		full := append(args[:2:2], append([]string{"--index", filepath.Join(d, host, "state", "index.db")}, args[2:]...)...)
		if st := Run(full, env); st != 0 {
			t.Fatalf("%v: %s", args, errb.String())
		}
		return out.String()
	}
	session("laharl", "aaaaaaaa-1111-4111-8111-111111111111", "/w/.worktrees/app/feat-span", "feat/span", "2026-10-02T08:00:00Z", "the second half with a zebra")
	session("hub", "bbbbbbbb-2222-4222-8222-222222222222", "/w/.worktrees/app/feat-span", "feat/span", "2026-10-01T08:00:00Z", "the first half with a zebra")
	session("hub", "cccccccc-3333-4333-8333-333333333333", "/w/code/solo", "main", "2026-10-03T08:00:00Z", "only on the hub")
	for _, h := range []string{"laharl", "hub"} {
		if out, e, err := as(h, "archive", "run"); err != nil {
			t.Fatalf("%s: %v %s %s", h, err, out, e)
		}
	}
	for args, want := range map[string]string{
		"archive index --if-changed":       "needs --all-hosts",
		"archive index --all-hosts --pull": "--pull are gone",
		"archive index --publish":          "--pull are gone",
		"archive verify --all-hosts":       "needs --no-source",
	} {
		if _, e, err := as("laharl", strings.Fields(args)...); err == nil || !strings.Contains(e, want) {
			t.Errorf("%s: %v %s", args, err, e)
		}
	}
	tick := func(host string) string {
		t.Helper()
		out, e, err := as(host, "archive", "index", "--all-hosts", "--if-changed", "--ledger", "none", "--git=false")
		if err != nil {
			t.Fatalf("%s: %v %s %s", host, err, out, e)
		}
		return out
	}
	for _, h := range []string{"laharl", "hub"} {
		if out := tick(h); !strings.Contains(out, "3 sessions in 2 threads") {
			t.Errorf("%s: %s", h, out)
		}
		if out := tick(h); !strings.Contains(out, "nothing changed") {
			t.Errorf("%s, nothing new: %s", h, out)
		}
	}
	out, _, err := as("hub", "archive", "status")
	if err != nil || !strings.Contains(out, "last checked") || !strings.Contains(out, "3 sessions on") {
		t.Fatalf("%v %s", err, out)
	}
	// A new session on one host shows up on the other's next tick.
	session("laharl", "dddddddd-4444-4444-8444-444444444444", "/w/code/more", "main", "2026-10-04T08:00:00Z", "a later one")
	if out, e, err := as("laharl", "archive", "run"); err != nil {
		t.Fatalf("%v %s %s", err, out, e)
	}
	if out := tick("hub"); !strings.Contains(out, "1 indexed") {
		t.Errorf("%s", out)
	}

	// Each row names its own host, whichever host answers.
	list := recall("hub", "recall", "list")
	if !strings.Contains(list, "aaaaaaaa  laharl") || !strings.Contains(list, "bbbbbbbb  hub") {
		t.Errorf("no host column:\n%s", list)
	}
	if s := recall("hub", "recall", "search", "zebra", "--host", "laharl"); !strings.Contains(s, "aaaaaaaa  laharl") || strings.Contains(s, "bbbbbbbb") {
		t.Errorf("search --host:\n%s", s)
	}
	threads := recall("hub", "recall", "threads", "--host", "laharl")
	if !strings.Contains(threads, "2 sessions on hub,laharl") {
		t.Errorf("threads --host:\n%s", threads)
	}
	if s := recall("hub", "recall", "threads", "--host", "lah"); !strings.Contains(s, "nothing found") {
		t.Errorf("a part of a host name should not match:\n%s", s)
	}

	out, e, err := as("hub", "archive", "verify", "--all-hosts", "--no-source")
	if err != nil || !strings.Contains(out, "2 hosts [hub laharl]") || !strings.Contains(out, "4 streams, 4 segments") || !strings.Contains(out, "0 bad, 0 gaps, 0 overlaps") {
		t.Fatalf("%v %s %s", err, out, e)
	}
	if out, _, err = as("hub", "archive", "verify", "--no-source"); err != nil || !strings.Contains(out, "1 hosts [hub]") {
		t.Fatalf("%v %s", err, out)
	}
	// A damaged segment is a fault,
	// and the command fails.
	var seg string
	filepath.WalkDir(filepath.Join(store, "v1", "laharl"), func(p string, de os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".seg") {
			seg = p
		}
		return nil
	})
	os.Chmod(seg, 0o600)
	if err := os.WriteFile(seg, []byte("nonsense"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, e, err = as("hub", "archive", "verify", "--all-hosts", "--no-source"); err == nil || !strings.Contains(e, "fault:") {
		t.Fatalf("%v %s", err, e)
	}
}
