package cli

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/agenttoken"
)

// retryEnv is an Env whose local Comebye is at addr, with a token, and
// whose waits are recorded (and passed to onSleep) instead of slept.
func retryEnv(t *testing.T, addr string, onSleep func(time.Duration)) (*Env, *[]time.Duration) {
	t.Helper()
	dir := t.TempDir()
	if _, err := agenttoken.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"COMEBYE_URL": "http://" + addr, "COMEBYE_STATE_DIR": dir}
	var slept []time.Duration
	e := &Env{
		Getenv:   func(k string) string { return env[k] },
		Hostname: func() (string, error) { return "h", nil },
		Sleep: func(d time.Duration) {
			slept = append(slept, d)
			if onSleep != nil {
				onSleep(d)
			}
		},
	}
	return e, &slept
}

// freeAddr is a loopback address nothing listens on (yet).
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// serveOn starts h on addr and stops it at the end of the test.
func serveOn(t *testing.T, addr string, h http.Handler) *http.Server {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return srv
}

// counter answers {} and counts the requests it got, by "METHOD path".
type counter struct {
	mu  sync.Mutex
	got map[string]int
}

func (c *counter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if c.got == nil {
		c.got = map[string]int{}
	}
	c.got[r.Method+" "+r.URL.Path]++
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{}`))
}

func (c *counter) n(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[k]
}

// A POST made while the server is down (refused) is sent again once it is
// back, and reaches it exactly once.
func TestRetryRefusedThenUp(t *testing.T) {
	addr := freeAddr(t)
	var srv counter
	started := false
	e, slept := retryEnv(t, addr, func(time.Duration) {
		if !started { // the restart ends during the first wait
			started = true
			serveOn(t, addr, &srv)
		}
	})
	c, err := newClient(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.call("POST", "/api/report", nil, map[string]string{"text": "done"}, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if n := srv.n("POST /api/report"); n != 1 {
		t.Fatalf("the server got the POST %d times, want 1", n)
	}
	if len(*slept) != 1 || (*slept)[0] != retryFirstGap {
		t.Fatalf("waits %v, want one of %v", *slept, retryFirstGap)
	}
}

// A server that never comes back: the CLI gives up after about retryFor of
// waits, with the old message.
func TestRetryGivesUp(t *testing.T) {
	e, slept := retryEnv(t, freeAddr(t), nil)
	c, err := newClient(e)
	if err != nil {
		t.Fatal(err)
	}
	err = c.call("GET", "/api/agents", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot reach Comebye") {
		t.Fatalf("err %v", err)
	}
	var total time.Duration
	for _, d := range *slept {
		if d > retryMaxGap {
			t.Fatalf("a wait of %v is over the cap %v", d, retryMaxGap)
		}
		total += d
	}
	if total > retryFor || total < retryFor-retryMaxGap {
		t.Fatalf("waited %v in all (%v), want about %v", total, *slept, retryFor)
	}
}

// resetter reads a request, then drops the connection without an answer
// (a server killed mid-request) the first `drops` times; then it answers.
type resetter struct {
	drops int32
	got   atomic.Int32
}

func (rs *resetter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := rs.got.Add(1)
	if n <= rs.drops {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetLinger(0) // a reset, not a clean close
			}
			conn.Close()
		}
		return
	}
	w.Write([]byte(`{}`))
}

// A POST the server may have read before it went down is never sent again:
// it could be done twice.
func TestRetryNoResendAfterReset(t *testing.T) {
	addr := freeAddr(t)
	rs := &resetter{drops: 1}
	serveOn(t, addr, rs)
	e, slept := retryEnv(t, addr, nil)
	c, err := newClient(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.call("POST", "/api/todos", nil, map[string]string{"text": "x"}, nil); err == nil {
		t.Fatal("a POST cut off mid-request succeeded")
	}
	if n := rs.got.Load(); n != 1 {
		t.Fatalf("the server got the POST %d times, want 1", n)
	}
	if len(*slept) != 0 {
		t.Fatalf("waited %v before giving up on a POST", *slept)
	}
}

// A GET cut off the same way is a read: it is sent again.
func TestRetryGetAfterReset(t *testing.T) {
	addr := freeAddr(t)
	rs := &resetter{drops: 1}
	serveOn(t, addr, rs)
	e, slept := retryEnv(t, addr, nil)
	c, err := newClient(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.call("GET", "/api/agents", nil, nil, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if n := rs.got.Load(); n != 2 {
		t.Fatalf("the server got the GET %d times, want 2", n)
	}
	if len(*slept) != 1 {
		t.Fatalf("waits %v, want 1", *slept)
	}
}

// An answer, even an error status, is final.
func TestRetryNotOnAnswer(t *testing.T) {
	addr := freeAddr(t)
	var got atomic.Int32
	serveOn(t, addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		http.Error(w, `{"code":"unavailable","message":"starting"}`, http.StatusServiceUnavailable)
	}))
	e, slept := retryEnv(t, addr, nil)
	c, _ := newClient(e)
	if err := c.call("GET", "/api/agents", nil, nil, nil); err == nil {
		t.Fatal("a 503 succeeded")
	}
	if got.Load() != 1 || len(*slept) != 0 {
		t.Fatalf("got %d requests and waits %v, want 1 and none", got.Load(), *slept)
	}
}

// The real thing: a server that answers, goes down and comes back on the
// same address a moment later (an upgrade's restart), with real waits. A
// command made in the gap reaches the new server once.
func TestRetryThroughRestart(t *testing.T) {
	addr := freeAddr(t)
	var before, after counter
	old := serveOn(t, addr, &before)
	dir := t.TempDir()
	if _, err := agenttoken.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"COMEBYE_URL": "http://" + addr, "COMEBYE_STATE_DIR": dir}
	e := &Env{Getenv: func(k string) string { return env[k] }, Hostname: func() (string, error) { return "h", nil }}
	c, err := newClient(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.call("GET", "/api/whoami", nil, nil, nil); err != nil {
		t.Fatalf("before the restart: %v", err)
	}
	old.Close() // the restart begins
	back := make(chan struct{})
	go func() {
		time.Sleep(700 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
			close(back)
			return
		}
		srv := &http.Server{Handler: &after}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
		close(back)
	}()
	if err := c.call("POST", "/api/report", nil, map[string]string{"text": "final"}, nil); err != nil {
		t.Fatalf("during the restart: %v", err)
	}
	<-back
	if before.n("POST /api/report") != 0 || after.n("POST /api/report") != 1 {
		t.Fatalf("POSTs: old server %d, new %d; want 0 and 1", before.n("POST /api/report"), after.n("POST /api/report"))
	}
}
