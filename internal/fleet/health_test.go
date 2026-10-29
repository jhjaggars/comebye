package fleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// classifyError sorts a failure into a class with a message that names
// the fix. The cases mirror the Oct 25 incident: a renamed peer (dns),
// a stale tailscale serve (tls, internal error alert), a certificate
// that does not match a tailnet IP (tls, by-name caveat), a peer that
// is down (tcp) and one that rejects the hub (http).
func TestClassifyError(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "laharl-1.tailnet.ts.net", IsNotFound: true}
	cases := []struct {
		name     string
		err      error
		host     string
		class    string
		mentions []string
	}{
		{
			name: "dns", err: &url.Error{Op: "Get", URL: "https://laharl-1.tailnet.ts.net/api/events", Err: dnsErr},
			class: ClassDNS, mentions: []string{"cannot resolve laharl-1.tailnet.ts.net", "renamed", "-peers"},
		},
		{
			name: "tls internal error alert",
			err: &url.Error{Op: "Get", URL: "https://laharl-1.tailnet.ts.net/api/events",
				Err: &net.OpError{Op: "remote handshake", Err: tls.AlertError(80)}},
			class:    ClassTLS,
			mentions: []string{"internal error alert", "tailscale serve reset", "tailscale serve --bg 8741"},
		},
		{
			name: "tls certificate not valid for name",
			err: &url.Error{Op: "Get", URL: "https://100.101.102.103/api/events",
				Err: &tls.CertificateVerificationError{Err: hostnameError(t, "kube.tailnet.ts.net")}},
			host: "100.101.102.103:443", class: ClassTLS,
			mentions: []string{"issued for its name, not its IP", "MagicDNS name"},
		},
		{
			name: "tls unknown authority",
			err: &url.Error{Op: "Get", URL: "https://kube.tailnet.ts.net/api/events",
				Err: errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")},
			class: ClassTLS, mentions: []string{"TLS handshake with kube.tailnet.ts.net failed", "tailscale serve"},
		},
		{
			name: "tcp refused",
			err: &url.Error{Op: "Get", URL: "https://kube.tailnet.ts.net/api/events",
				Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
			class: ClassTCP, mentions: []string{"cannot connect to kube.tailnet.ts.net", "comebye running"},
		},
		{
			name: "timeout", err: context.DeadlineExceeded, class: ClassTCP, mentions: []string{"timeout"},
		},
		{
			name: "http", err: statusError(http.StatusUnauthorized), class: ClassHTTP,
			mentions: []string{"HTTP 401", "-ts-allow-login"},
		},
		{
			name: "other", err: errors.New("stream closed by peer"), class: "",
			mentions: []string{"stream closed by peer"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			class, msg := classifyError(c.err, c.host)
			if class != c.class {
				t.Errorf("class = %q, want %q (msg %q)", class, c.class, msg)
			}
			for _, want := range c.mentions {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q lacks %q", msg, want)
				}
			}
		})
	}
}

// hostnameError builds the x509 error the TLS client reports when the
// peer's certificate (issued for name) does not match the IP tried.
func hostnameError(t *testing.T, name string) error {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return x509.HostnameError{Certificate: cert, Host: "100.101.102.103"}
}

// The hub probes every peer with GET /api/whoami at startup and every
// ProbeEvery, and a down peer's shown error becomes the probe's
// classified result with the class and down-since time kept.
func TestHealthProbe(t *testing.T) {
	fp := newFakePeer(t, agent("wS:p1", "fixer", herdr.StatusIdle))
	// A peer whose TLS handshake fails, like a stale tailscale serve.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{}")
	}))
	defer tlsSrv.Close()
	tlsURL, _ := url.Parse(tlsSrv.URL)

	opts := fastOpts(nil)
	opts.ProbeEvery = 50 * time.Millisecond
	f := startFleet(t, opts, Peer{Name: "kube", URLs: []*url.URL{fp.url()}}, Peer{Name: "stale", URLs: []*url.URL{tlsURL}})

	// The healthy peer is probed: /api/whoami was asked, and its host
	// status stays ok with the address recorded.
	waitFor(t, "whoami probe", func() bool { return len(fp.requestsTo("/api/whoami")) > 0 })
	h := hostByName(f, "kube")
	if h.Status != StatusOK || h.Class != "" || h.Addr == "" {
		t.Fatalf("kube host = %+v", h)
	}

	// The TLS-broken peer goes down and the probe says tls, with a fix.
	waitFor(t, "stale peer down", func() bool { return hostByName(f, "stale").Status == StatusDown })
	waitFor(t, "classified", func() bool { return hostByName(f, "stale").Class == ClassTLS })
	h = hostByName(f, "stale")
	if !strings.Contains(h.Error, "tailscale serve") || h.Since == nil {
		t.Fatalf("stale host = %+v", h)
	}

	// The probe keeps running: several whoami requests over time.
	waitFor(t, "periodic probes", func() bool { return len(fp.requestsTo("/api/whoami")) >= 3 })
	n := len(fp.requestsTo("/api/whoami"))
	time.Sleep(150 * time.Millisecond)
	if got := len(fp.requestsTo("/api/whoami")); got < n {
		t.Fatalf("probes stopped: %d then %d", n, got)
	}
}

// A peer with two addresses falls back to the second when the first
// fails at the transport level, and reports which one answered. An HTTP
// answer (any status) does not fall through: only DNS, TCP and TLS do.
func TestPeerAddressFallback(t *testing.T) {
	fp := newFakePeer(t, agent("wS:p1", "fixer", herdr.StatusIdle))
	deadURL, _ := url.Parse("https://no-such-host.invalid")

	f := startFleet(t, fastOpts(nil),
		Peer{Name: "kube", URLs: []*url.URL{deadURL, fp.url()}})
	waitFor(t, "connected through the fallback", func() bool {
		h := hostByName(f, "kube")
		return h.Status == StatusOK && len(f.Agents()) == 1
	})
	h := hostByName(f, "kube")
	if h.Addr != fp.url().Host {
		t.Fatalf("answered address = %q, want the fake peer's %q", h.Addr, fp.url().Host)
	}

	// The proxy path also falls back, and reports which address answered
	// in the JSON error when both fail.
	w := httptest.NewRecorder()
	f.ProxyHost(w, httptest.NewRequest("GET", "/x", nil), "kube", "launch-options")
	if w.Code != http.StatusOK {
		t.Fatalf("proxy through fallback = %d %s", w.Code, w.Body)
	}

	gone := Peer{Name: "gone", URLs: []*url.URL{deadURL}}
	f2 := New(nil, "desk", []Peer{gone}, fastOpts(nil))
	w = httptest.NewRecorder()
	f2.ProxyHost(w, httptest.NewRequest("GET", "/x", nil), "gone", "launch-options")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "no such host") {
		t.Fatalf("both addresses failing = %d %s", w.Code, w.Body)
	}

	// A peer that answers HTTP (even an error status) is used as is: no
	// fallthrough past an answer.
	teapot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer teapot.Close()
	tURL, _ := url.Parse(teapot.URL)
	fp2 := newFakePeer(t)
	f3 := startFleet(t, fastOpts(nil), Peer{Name: "tp", URLs: []*url.URL{tURL, fp2.url()}})
	waitFor(t, "tp down on its HTTP answer", func() bool { return hostByName(f3, "tp").Status == StatusDown })
	h3 := hostByName(f3, "tp")
	if h3.Addr != tURL.Host {
		t.Fatalf("first answering address = %q", h3.Addr)
	}
	if h3.Class != ClassHTTP {
		t.Fatalf("teapot class = %q, want http", h3.Class)
	}
	if n := len(fp2.requestsTo("/api/events")); n != 0 {
		t.Fatalf("fell through an HTTP answer: %d event requests", n)
	}
}

// A POST that falls back to a later address still carries its body: each
// attempt gets a fresh reader.
func TestFallbackPostKeepsBody(t *testing.T) {
	fp := newFakePeer(t)
	deadURL, _ := url.Parse("https://no-such-host.invalid")
	f := New(nil, "desk", []Peer{{Name: "kube", URLs: []*url.URL{deadURL, fp.url()}}}, fastOpts(nil))
	body := `{"text":"hello"}`
	w := httptest.NewRecorder()
	f.Proxy(w, httptest.NewRequest("POST", "/x", strings.NewReader(body)), "kube", "wS:p1", "prompt")
	if w.Code != http.StatusConflict {
		t.Fatalf("prompt through fallback = %d %s", w.Code, w.Body)
	}
	reqs := fp.requestsTo("/prompt")
	if len(reqs) != 1 || reqs[0].body != body {
		t.Fatalf("forwarded prompt = %+v", reqs)
	}
}

// The down-since time survives reconnect attempts and probe refreshes,
// and is reset when the peer comes back.
func TestDownSinceKept(t *testing.T) {
	fp := newFakePeer(t, agent("wS:p1", "fixer", herdr.StatusIdle))
	f := startFleet(t, fastOpts(nil), Peer{Name: "kube", URLs: []*url.URL{fp.url()}})
	waitFor(t, "up", func() bool { return hostByName(f, "kube").Status == StatusOK })

	fp.goSilent(true)
	waitFor(t, "down", func() bool { return hostByName(f, "kube").Status == StatusDown })
	first := hostByName(f, "kube").Since
	if first == nil {
		t.Fatal("no down-since")
	}
	time.Sleep(60 * time.Millisecond)
	if h := hostByName(f, "kube"); h.Status != StatusDown {
		t.Fatalf("flapped: %+v", h)
	}
	if h := hostByName(f, "kube"); !h.Since.Equal(*first) {
		t.Fatalf("down-since moved: %v then %v", first, h.Since)
	}

	fp.goSilent(false)
	waitFor(t, "back up", func() bool { return hostByName(f, "kube").Status == StatusOK })
	if h := hostByName(f, "kube"); h.Since == nil || !h.Since.After(*first) {
		t.Fatalf("down-since not reset on recovery: %+v", h)
	}
}
