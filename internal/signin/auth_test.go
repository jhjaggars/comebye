package signin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func noKey(string) string { return "" }

func TestAuthStateCachesAndRefreshesAfterSignIn(t *testing.T) {
	home := fakeHome(t, "")
	ck := &clock{t: time.Unix(1000, 0)}
	m := newMgr(t, Config{Now: ck.now, LookupEnv: noKey})
	ctx := context.Background()

	if got := m.AuthState(ctx, []string{"claude"})["claude"]; got.State != AuthSignedOut || !got.CanSignIn {
		t.Fatalf("fresh host: %+v", got)
	}
	// The credential appears behind the cache's back: still the cached answer.
	ok := filepath.Join(home, ".claude", "fake-ok")
	if err := os.MkdirAll(filepath.Dir(ok), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ok, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ck.t = ck.t.Add(59 * time.Second)
	if got := m.AuthState(ctx, []string{"claude"})["claude"]; got.State != AuthSignedOut {
		t.Fatalf("inside the 60 s the answer is cached: %+v", got)
	}
	ck.t = ck.t.Add(2 * time.Second)
	if got := m.AuthState(ctx, []string{"claude"})["claude"]; got.State != AuthSignedIn || got.Method != "claude.ai" {
		t.Fatalf("after 60 s it is probed again: %+v", got)
	}
}

func TestAuthStateRefreshesRightAfterASignIn(t *testing.T) {
	fakeHome(t, "")
	ck := &clock{t: time.Unix(1000, 0)}
	m := newMgr(t, Config{Now: ck.now, LookupEnv: noKey})
	ctx := context.Background()
	if got := m.AuthState(ctx, []string{"claude"})["claude"]; got.State != AuthSignedOut {
		t.Fatalf("before: %+v", got)
	}
	snap, err := m.Start(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SendCode(snap.ID, "GOOD-CODE"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, snap.ID, StateSignedIn)
	// No time has passed: the sign-in itself dropped the cached answer.
	if got := m.AuthState(ctx, []string{"claude"})["claude"]; got.State != AuthSignedIn {
		t.Fatalf("after the sign-in: %+v", got)
	}
}

func TestAuthStateKeysAndUnknown(t *testing.T) {
	fakeHome(t, "")
	m := newMgr(t, Config{LookupEnv: noKey})
	got := m.AuthState(context.Background(), []string{"pi", "codex"})
	if p := got["pi"]; p.State != AuthSignedIn || p.Method != MethodKey || p.CanSignIn {
		t.Fatalf("pi: %+v", p)
	}
	if c := got["codex"]; c.State != AuthUnknown || c.CanSignIn {
		t.Fatalf("codex has no sign-in here: %+v", c)
	}
	// claude on an API key shows a key, with nothing to sign in to.
	m2 := newMgr(t, Config{LookupEnv: func(k string) string {
		if k == "ANTHROPIC_API_KEY" {
			return "x"
		}
		return ""
	}})
	if c := m2.AuthState(context.Background(), []string{"claude"})["claude"]; c.State != AuthSignedIn || c.Method != MethodKey || c.CanSignIn {
		t.Fatalf("claude with a key: %+v", c)
	}
}

func TestAuthStateUnparseableIsUnknown(t *testing.T) {
	fakeHome(t, "")
	m := newMgr(t, Config{LookupEnv: noKey, Drivers: map[string]Driver{"claude": {Name: "claude", Command: []string{"claude"}, Status: []string{"true"}}}})
	if c := m.AuthState(context.Background(), []string{"claude"})["claude"]; c.State != AuthUnknown || !c.CanSignIn {
		t.Fatalf("%+v", c)
	}
}
