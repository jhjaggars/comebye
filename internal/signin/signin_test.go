package signin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeHome puts the fake claude first on PATH and a fresh HOME, with the
// fault mode (if any) in HOME/fake-mode.
func fakeHome(t *testing.T, mode string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", mustAbs(t, "testdata")+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Things the child must not see.
	t.Setenv("COMEBYE_AGENT_TOKEN", "TOKEN-SENTINEL")
	t.Setenv("COMEBYE_STATE_DIR", "/nonexistent-state")
	t.Setenv("DISPLAY", ":99")
	t.Setenv("ANTHROPIC_API_KEY", "KEY-SENTINEL")
	if mode != "" {
		if err := os.WriteFile(filepath.Join(home, "fake-mode"), []byte(mode), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func mustAbs(t *testing.T, p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newMgr(t *testing.T, cfg Config) *Manager {
	t.Helper()
	m := NewManager(cfg)
	t.Cleanup(m.Close)
	return m
}

func readPID(t *testing.T, home, name string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func alive(pid int) bool {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		s := string(b)
		if i := strings.LastIndex(s, ") "); i >= 0 && len(s) > i+2 {
			return s[i+2] != 'Z'
		}
	}
	return syscall.Kill(pid, 0) == nil
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
}

func waitState(t *testing.T, m *Manager, id string, want ...string) Snapshot {
	t.Helper()
	var snap Snapshot
	for i := 0; i < 400; i++ {
		var err error
		snap, err = m.Get(id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		for _, w := range want {
			if snap.State == w {
				return snap
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("state stuck at %q, want %v", snap.State, want)
	return snap
}

func TestHappyPath(t *testing.T) {
	home := fakeHome(t, "")
	m := newMgr(t, Config{})
	snap, err := m.Start(context.Background(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != StateWaitingCode || !snap.CodeInput {
		t.Fatalf("snap = %+v", snap)
	}
	if !strings.HasPrefix(snap.URL, "https://claude.com/cai/oauth/authorize?code=true&client_id=fake") || strings.ContainsAny(snap.URL, "\x1b\x07") {
		t.Fatalf("url = %q", snap.URL)
	}
	got, err := m.SendCode(snap.ID, "GOOD-CODE")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateChecking {
		t.Fatalf("after code: %+v", got)
	}
	done := waitState(t, m, snap.ID, StateSignedIn, StateFailed)
	if done.State != StateSignedIn || done.Method != "claude.ai" || done.URL != "" {
		t.Fatalf("done = %+v", done)
	}
	// The group is gone, the background child included.
	waitDead(t, readPID(t, home, "fake-pid"))
	waitDead(t, readPID(t, home, "fake-child-pid"))
}

func TestWrongCode(t *testing.T) {
	fakeHome(t, "")
	m := newMgr(t, Config{})
	snap, _ := m.Start(context.Background(), "claude")
	if _, err := m.SendCode(snap.ID, "WRONG"); err != nil {
		t.Fatal(err)
	}
	done := waitState(t, m, snap.ID, StateSignedIn, StateFailed)
	if done.State != StateFailed || done.Reason != ReasonCodeRejected {
		t.Fatalf("done = %+v", done)
	}
}

func TestExitZeroWithoutLogin(t *testing.T) {
	fakeHome(t, "exit0")
	m := newMgr(t, Config{})
	snap, _ := m.Start(context.Background(), "claude")
	m.SendCode(snap.ID, "GOOD-CODE")
	done := waitState(t, m, snap.ID, StateSignedIn, StateFailed)
	if done.State != StateFailed || done.Reason != ReasonCodeRejected {
		t.Fatalf("exit 0 without login must fail: %+v", done)
	}
}

func TestHangAfterCodeTimesOutAndKillsGroup(t *testing.T) {
	home := fakeHome(t, "hang")
	m := newMgr(t, Config{ResultWait: 400 * time.Millisecond})
	snap, _ := m.Start(context.Background(), "claude")
	m.SendCode(snap.ID, "GOOD-CODE")
	done := waitState(t, m, snap.ID, StateSignedIn, StateFailed)
	if done.Reason != ReasonDidNotFinish {
		t.Fatalf("done = %+v", done)
	}
	waitDead(t, readPID(t, home, "fake-pid"))
	waitDead(t, readPID(t, home, "fake-child-pid"))
}

func TestCodeWaitTimeout(t *testing.T) {
	home := fakeHome(t, "")
	m := newMgr(t, Config{CodeWait: 300 * time.Millisecond})
	snap, _ := m.Start(context.Background(), "claude")
	done := waitState(t, m, snap.ID, StateFailed)
	if done.Reason != ReasonTimedOut {
		t.Fatalf("done = %+v", done)
	}
	waitDead(t, readPID(t, home, "fake-pid"))
	waitDead(t, readPID(t, home, "fake-child-pid"))
	if _, err := m.SendCode(snap.ID, "GOOD-CODE"); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("code after timeout: %v", err)
	}
}

func TestCancelKillsGroup(t *testing.T) {
	home := fakeHome(t, "")
	m := newMgr(t, Config{})
	snap, _ := m.Start(context.Background(), "claude")
	got, err := m.Cancel(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed || got.Reason != ReasonCancelled {
		t.Fatalf("cancel = %+v", got)
	}
	waitDead(t, readPID(t, home, "fake-pid"))
	waitDead(t, readPID(t, home, "fake-child-pid"))
	// Cancelling an ended one removes it.
	if _, err := m.Cancel(snap.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(snap.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestEndedSessionDeletedAfterKeepFor(t *testing.T) {
	fakeHome(t, "")
	m := newMgr(t, Config{KeepFor: 200 * time.Millisecond})
	snap, _ := m.Start(context.Background(), "claude")
	m.SendCode(snap.ID, "GOOD-CODE")
	waitState(t, m, snap.ID, StateSignedIn)
	time.Sleep(500 * time.Millisecond)
	if _, err := m.Get(snap.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session should be gone: %v", err)
	}
}

func TestSecondStartIsBusy(t *testing.T) {
	fakeHome(t, "")
	m := newMgr(t, Config{})
	first, _ := m.Start(context.Background(), "claude")
	_, err := m.Start(context.Background(), "claude")
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Session.ID != first.ID || busy.Session.State != StateWaitingCode {
		t.Fatalf("second start: %v", err)
	}
}

func TestHostLimit(t *testing.T) {
	fakeHome(t, "")
	two := Claude
	two.Name = "claude2"
	three := Claude
	three.Name = "claude3"
	m := newMgr(t, Config{Drivers: map[string]Driver{"claude": Claude, "claude2": two, "claude3": three}})
	for _, a := range []string{"claude", "claude2"} {
		if _, err := m.Start(context.Background(), a); err != nil {
			t.Fatalf("start %s: %v", a, err)
		}
	}
	if _, err := m.Start(context.Background(), "claude3"); !errors.Is(err, ErrTooMany) {
		t.Fatalf("third start: %v", err)
	}
}

func TestUnknownAgent(t *testing.T) {
	m := newMgr(t, Config{})
	if _, err := m.Start(context.Background(), "nope"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatal(err)
	}
}

func TestBadCodeNeverReachesTheProcess(t *testing.T) {
	fakeHome(t, "")
	m := newMgr(t, Config{})
	snap, _ := m.Start(context.Background(), "claude")
	for _, c := range []string{"", "a\nb", "a b", "a\x1bb", "a\x03", "é", strings.Repeat("a", 513), "a;b", "a/b", "a\r"} {
		if _, err := m.SendCode(snap.ID, c); !errors.Is(err, ErrBadCode) {
			t.Errorf("code %q: err = %v", c, err)
		}
	}
	if got, _ := m.Get(snap.ID); got.State != StateWaitingCode {
		t.Fatalf("a refused code changed the state: %+v", got)
	}
	// 512 bytes of the allowed set is fine.
	if !ValidCode(strings.Repeat("aZ0_#.~-", 64)) {
		t.Error("512 allowed bytes refused")
	}
}

func TestNoURLAndBadURL(t *testing.T) {
	for _, mode := range []string{"nourl", "badurl", "httpurl"} {
		t.Run(mode, func(t *testing.T) {
			home := fakeHome(t, mode)
			m := newMgr(t, Config{URLWait: 500 * time.Millisecond})
			_, err := m.Start(context.Background(), "claude")
			var fe *FailError
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v", err)
			}
			want := ReasonUnexpectedText
			if mode == "nourl" {
				want = ReasonDidNotFinish
			}
			if fe.Reason != want {
				t.Fatalf("reason = %q, want %q", fe.Reason, want)
			}
			waitDead(t, readPID(t, home, "fake-pid"))
			waitDead(t, readPID(t, home, "fake-child-pid"))
		})
	}
}

func TestNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	m := newMgr(t, Config{})
	_, err := m.Start(context.Background(), "claude")
	var fe *FailError
	if !errors.As(err, &fe) || fe.Reason != ReasonNotInstalled {
		t.Fatalf("err = %v", err)
	}
	// The slot is free again.
	if len(m.sessions) != 0 {
		t.Fatal("a failed start left a session")
	}
}

func TestChildEnvironmentIsScrubbed(t *testing.T) {
	home := fakeHome(t, "")
	m := newMgr(t, Config{})
	snap, _ := m.Start(context.Background(), "claude")
	m.Cancel(snap.ID)
	b, err := os.ReadFile(filepath.Join(home, "fake-env"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	for _, bad := range []string{"TOKEN-SENTINEL", "KEY-SENTINEL", "COMEBYE", "DISPLAY", "ANTHROPIC", "/nonexistent-state"} {
		if strings.Contains(env, bad) {
			t.Errorf("child environment has %q:\n%s", bad, env)
		}
	}
	for _, want := range []string{"HOME=" + home, "TERM=dumb", "BROWSER=/bin/false", "PATH="} {
		if !strings.Contains(env, want) {
			t.Errorf("child environment lacks %q:\n%s", want, env)
		}
	}
}

func TestCloseEndsSessions(t *testing.T) {
	home := fakeHome(t, "")
	m := NewManager(Config{})
	snap, _ := m.Start(context.Background(), "claude")
	m.Close()
	waitDead(t, readPID(t, home, "fake-pid"))
	waitDead(t, readPID(t, home, "fake-child-pid"))
	if got, _ := m.Get(snap.ID); got.State != StateFailed {
		t.Fatalf("after close: %+v", got)
	}
	if _, err := m.Start(context.Background(), "claude"); err == nil {
		t.Fatal("a closed manager started a sign-in")
	}
}

func TestEchoModeDoesNotLeakTheCode(t *testing.T) {
	fakeHome(t, "echo")
	m := newMgr(t, Config{})
	snap, _ := m.Start(context.Background(), "claude")
	code := "SENTINEL-CODE-9f3a"
	m.SendCode(snap.ID, code)
	done := waitState(t, m, snap.ID, StateFailed, StateSignedIn)
	if strings.Contains(done.Message+done.Reason+done.URL+done.Method, code) {
		t.Fatalf("code in snapshot: %+v", done)
	}
}

func TestURLParsing(t *testing.T) {
	raw := "Opening browser to sign in\u2026\r\nIf the browser didn't open, visit: \x1b]8;;https://claude.com/a?b=1&c=2\x07https://claude.com/a?b=1&c=2\x1b]8;;\x07\r\n\x1b[94mhttps://claude.com/a?b=1&c=2\x1b[39m\r\n"
	u, ok := FindURL(raw)
	if !ok || u != "https://claude.com/a?b=1&c=2" {
		t.Fatalf("url = %q, %v", u, ok)
	}
	if _, ok := FindURL("visit: https://claude.com/a?b"); ok {
		t.Error("an incomplete link was taken")
	}
	for u, want := range map[string]bool{
		"https://claude.com/x": true, "https://platform.claude.com/x": true, "https://CLAUDE.ai/x": true,
		"http://claude.com/x": false, "https://evil.example/x": false, "https://claude.com.evil.example/x": false,
		"https://user@claude.com/x": false, "https://claude.com:8443/x": false, "javascript:alert(1)": false,
	} {
		if got := Claude.URLAllowed(u); got != want {
			t.Errorf("URLAllowed(%q) = %v, want %v", u, got, want)
		}
	}
}
