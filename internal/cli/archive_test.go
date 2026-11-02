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
			args = append(args[:2:2], append([]string{"--dir", filepath.Join(d, "store"), "--identity", filepath.Join(d, "keys", "identity.key"),
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
