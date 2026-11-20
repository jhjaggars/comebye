package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two real comebye processes, a hub and a peer, each with its own state dir,
// HOME and fake herdr, and the built CLI signing in on the peer through the
// hub with the fake claude: a wrong code, then the right one.
func TestSigninRelayTwoProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts two comebye processes")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp("", "cb-signin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	bin := func(name, pkg string) string {
		out := filepath.Join(d, name)
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
		return out
	}
	comebye, herdr := bin("comebye", "."), bin("fake-herdr", "./tools/devserver")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	port := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	peerHome := filepath.Join(d, "peer-home")
	os.MkdirAll(peerHome, 0o700)
	fakeClaude := filepath.Join(root, "internal", "signin", "testdata")
	// The scrubbed environment of each process: its own HOME and a fake claude first on PATH.
	baseEnv := func(home string) []string {
		return []string{"HOME=" + home, "PATH=" + fakeClaude + string(os.PathListSeparator) + os.Getenv("PATH"),
			"COMEBYE_SCRATCH_DIR=" + filepath.Join(d, "scratch-"+filepath.Base(home)), "COMEBYE_TEST_INSECURE_PEERS=1"}
	}
	logs := map[string]*lockedBuf{}
	start := func(name string, env []string, bin string, args ...string) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = env
		out := &lockedBuf{}
		logs[name] = out
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cancel()
			cmd.Wait()
			if t.Failed() {
				t.Logf("%s log:\n%s", name, out.String())
			}
		})
	}
	waitFor := func(what string, ok func() bool) {
		for i := 0; i < 200; i++ {
			if ok() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	up := func(addr string) func() bool {
		return func() bool {
			r, err := http.Get("http://" + addr + "/api/agents")
			if err != nil {
				return false
			}
			r.Body.Close()
			return r.StatusCode == 200
		}
	}
	fileExists := func(p string) func() bool { return func() bool { _, err := os.Stat(p); return err == nil } }

	peerAddr, hubAddr := port(), port()
	peerSock, hubSock := filepath.Join(d, "p.sock"), filepath.Join(d, "h.sock")
	start("peer herdr", baseEnv(peerHome), herdr, "-sock", peerSock, "-dir", filepath.Join(d, "ptx"))
	waitFor("the peer's herdr", fileExists(peerSock))
	start("peer", baseEnv(peerHome), comebye, "-sock", peerSock, "-addr", peerAddr, "-insecure-no-auth", "-test-mode", "-push=false",
		"-state-dir", filepath.Join(d, "peer-state"), "-host-name", "peerbox", "-ledger=false")
	waitFor("the peer", up(peerAddr))
	hubHome := filepath.Join(d, "hub-home")
	os.MkdirAll(hubHome, 0o700)
	start("hub herdr", baseEnv(hubHome), herdr, "-sock", hubSock, "-dir", filepath.Join(d, "htx"))
	waitFor("the hub's herdr", fileExists(hubSock))
	start("hub", baseEnv(hubHome), comebye, "-sock", hubSock, "-addr", hubAddr, "-insecure-no-auth", "-test-mode", "-push=false",
		"-state-dir", filepath.Join(d, "hub-state"), "-host-name", "hubbox", "-peers", "peerbox=http://"+peerAddr)
	waitFor("the hub", up(hubAddr))

	// The CLI is the built binary, in its own state dir, aimed at the hub.
	signin := func(stdin string) (int, string, string) {
		cmd := exec.Command(comebye, "signin", "claude", "--host", "peerbox")
		cmd.Env = append(baseEnv(hubHome), "COMEBYE_URL=http://"+hubAddr, "COMEBYE_STATE_DIR="+filepath.Join(d, "cli-state"), "COMEBYE_LOGIN=me@example.com")
		cmd.Stdin = strings.NewReader(stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, out.String(), errb.String()
	}
	const wrong = "WRONG-CODE-7c1e"
	code, out, errs := signin(wrong + "\n")
	if code != 1 || !strings.Contains(errs, "https://claude.com/") {
		t.Fatalf("wrong code: exit %d out=%q err=%q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(peerHome, ".claude", "fake-ok")); err == nil {
		t.Fatal("signed in on a wrong code")
	}
	code, out, errs = signin("GOOD-CODE\n")
	if code != 0 || !strings.Contains(out, "signed in on peerbox") {
		t.Fatalf("good code: exit %d out=%q err=%q", code, out, errs)
	}
	// It happened on the peer's HOME, and on no other.
	if _, err := os.Stat(filepath.Join(peerHome, ".claude", "fake-ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hubHome, ".claude")); err == nil {
		t.Error("the hub's HOME was touched")
	}
	// Neither code is in either process's log, hub or peer.
	for name, l := range logs {
		for _, sentinel := range []string{wrong, "GOOD-CODE"} {
			if strings.Contains(l.String(), sentinel) {
				t.Errorf("%s is in the %s log", sentinel, name)
			}
		}
	}
	// Neither code is on disk on either side.
	for _, sentinel := range []string{wrong, "GOOD-CODE"} {
		filepath.Walk(d, func(p string, fi os.FileInfo, err error) error {
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > 8<<20 || p == comebye || p == herdr {
				return nil
			}
			if b, _ := os.ReadFile(p); strings.Contains(string(b), sentinel) && filepath.Base(p) != "fake-env" {
				t.Errorf("%s is in %s", sentinel, p)
			}
			return nil
		})
	}
}

// lockedBuf is a process's output, read while the process still writes.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
