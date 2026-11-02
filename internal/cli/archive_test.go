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
