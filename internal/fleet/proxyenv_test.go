package fleet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// socks5 is a minimal SOCKS5 server (no auth, CONNECT only) that records
// what the client asked for and connects every name to target.
type socks5 struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	asked  []string // "domain:name:port" or "ip:addr:port"
}

func newSocks5(t *testing.T, target string) *socks5 {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socks5{ln: ln, target: target}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *socks5) serve(c net.Conn) {
	defer c.Close()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	io.CopyN(io.Discard, c, int64(hdr[1])) // methods
	c.Write([]byte{5, 0})                  // no authentication
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
		return
	}
	var addr, kind string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(c, b)
		addr, kind = net.IP(b).String(), "ip"
	case 3:
		n := make([]byte, 1)
		io.ReadFull(c, n)
		b := make([]byte, n[0])
		io.ReadFull(c, b)
		addr, kind = string(b), "domain"
	case 4:
		b := make([]byte, 16)
		io.ReadFull(c, b)
		addr, kind = net.IP(b).String(), "ip"
	default:
		return
	}
	pb := make([]byte, 2)
	io.ReadFull(c, pb)
	s.mu.Lock()
	s.asked = append(s.asked, fmt.Sprintf("%s:%s:%d", kind, addr, binary.BigEndian.Uint16(pb)))
	s.mu.Unlock()
	up, err := net.Dial("tcp", s.target)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func (s *socks5) URL(scheme string) string { return scheme + "://" + s.ln.Addr().String() }

func (s *socks5) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// hubServer is a TLS "hub" and an upstream whose transport trusts it.
func hubServer(t *testing.T, env map[string]string) (*httptest.Server, *Upstream) {
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(hub.Close)
	up, err := NewUpstream("https://hub.tail1234.ts.net:"+portOf(hub), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := up.client.Transport.(*http.Transport)
	tr.Proxy = upstreamProxy(func(k string) string { return env[k] })
	pool := x509.NewCertPool()
	pool.AddCert(hub.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "example.com"}
	return hub, up
}

func portOf(s *httptest.Server) string {
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "https://"))
	return p
}

func call(t *testing.T, up *Upstream) {
	t.Helper()
	status, _, data, err := up.Do(context.Background(), UpstreamRequest{Method: http.MethodGet, Segments: []string{"api", "whoami"}})
	if err != nil || status != 200 || !strings.Contains(string(data), "/api/whoami") {
		t.Fatalf("status %d body %q err %v", status, data, err)
	}
}

// The hub link goes through a SOCKS5 proxy from ALL_PROXY or HTTPS_PROXY,
// and the proxy is asked for the hub's NAME (it resolves it: socks5h).
func TestUpstreamThroughSOCKS5(t *testing.T) {
	for _, tc := range []struct{ name, key, scheme string }{
		{"ALL_PROXY socks5h", "ALL_PROXY", "socks5h"},
		{"ALL_PROXY socks5", "ALL_PROXY", "socks5"},
		{"all_proxy lower case", "all_proxy", "socks5h"},
		{"HTTPS_PROXY socks5h", "HTTPS_PROXY", "socks5h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			hub, up := hubServer(t, env)
			sx := newSocks5(t, strings.TrimPrefix(hub.URL, "https://"))
			env[tc.key] = sx.URL(tc.scheme)
			call(t, up)
			got := sx.requests()
			if len(got) != 1 || got[0] != "domain:hub.tail1234.ts.net:"+portOf(hub) {
				t.Fatalf("the proxy was asked for %v", got)
			}
		})
	}
}

func TestUpstreamWithoutProxyGoesDirectOrFails(t *testing.T) {
	// No proxy configured: a tailnet name that does not resolve here fails
	// by itself (the point of the proxy).
	env := map[string]string{}
	_, up := hubServer(t, env)
	if _, _, _, err := up.Do(context.Background(), UpstreamRequest{Method: http.MethodGet, Segments: []string{"x"}}); err == nil {
		t.Fatal("reached an unresolvable name with no proxy")
	}
}

func TestUpstreamHTTPProxyConnect(t *testing.T) {
	env := map[string]string{}
	hub, up := hubServer(t, env)
	var mu sync.Mutex
	var gotHost string
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "connect only", 405)
			return
		}
		mu.Lock()
		gotHost = r.Host
		mu.Unlock()
		dst, err := net.Dial("tcp", strings.TrimPrefix(hub.URL, "https://"))
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		w.WriteHeader(200)
		c, buf, _ := w.(http.Hijacker).Hijack()
		// The server flushes the 200 before it stops its background read, so
		// the client's first TLS bytes can already sit in buf's reader: read
		// the tunnel through buf, not c, or they are lost ("hub unreachable").
		go io.Copy(dst, buf.Reader)
		io.Copy(c, dst)
		c.Close()
		dst.Close()
	}))
	t.Cleanup(px.Close)
	env["HTTPS_PROXY"] = px.URL
	call(t, up)
	mu.Lock()
	defer mu.Unlock()
	if gotHost != "hub.tail1234.ts.net:"+portOf(hub) {
		t.Fatalf("CONNECT %q", gotHost)
	}
}

func TestUpstreamProxyRules(t *testing.T) {
	get := func(env map[string]string, raw string) (string, error) {
		u, _ := url.Parse(raw)
		p, err := upstreamProxy(func(k string) string { return env[k] })(&http.Request{URL: u})
		if p == nil {
			return "", err
		}
		return p.String(), err
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		url  string
		want string
		fail bool
	}{
		{"none", nil, "https://hub.ts.net", "", false},
		{"all", map[string]string{"ALL_PROXY": "socks5h://127.0.0.1:1055"}, "https://hub.ts.net", "socks5://127.0.0.1:1055", false},
		{"https wins over all", map[string]string{"ALL_PROXY": "socks5h://127.0.0.1:1055", "HTTPS_PROXY": "http://p:3128"}, "https://hub.ts.net", "http://p:3128", false},
		{"http_proxy is not used for https", map[string]string{"HTTP_PROXY": "http://p:3128"}, "https://hub.ts.net", "", false},
		{"http target uses http_proxy", map[string]string{"HTTP_PROXY": "http://p:3128", "ALL_PROXY": "socks5://s:1"}, "http://hub.ts.net", "http://p:3128", false},
		{"bare host:port is http", map[string]string{"ALL_PROXY": "127.0.0.1:3128"}, "https://hub.ts.net", "http://127.0.0.1:3128", false},
		{"loopback bypasses", map[string]string{"ALL_PROXY": "socks5://s:1"}, "http://127.0.0.1:9", "", false},
		{"localhost bypasses", map[string]string{"ALL_PROXY": "socks5://s:1"}, "http://localhost:9", "", false},
		{"no_proxy suffix", map[string]string{"ALL_PROXY": "socks5://s:1", "NO_PROXY": "other.org, .ts.net"}, "https://hub.ts.net", "", false},
		{"no_proxy other host", map[string]string{"ALL_PROXY": "socks5://s:1", "NO_PROXY": "other.org"}, "https://hub.ts.net", "socks5://s:1", false},
		{"no_proxy star", map[string]string{"ALL_PROXY": "socks5://s:1", "NO_PROXY": "*"}, "https://hub.ts.net", "", false},
		{"no_proxy port mismatch", map[string]string{"ALL_PROXY": "socks5://s:1", "NO_PROXY": "hub.ts.net:444"}, "https://hub.ts.net", "socks5://s:1", false},
		{"no_proxy cidr", map[string]string{"ALL_PROXY": "socks5://s:1", "NO_PROXY": "100.64.0.0/10"}, "https://100.80.1.2", "", false},
		{"bad scheme fails closed", map[string]string{"ALL_PROXY": "ftp://s:1"}, "https://hub.ts.net", "", true},
		{"garbage fails closed", map[string]string{"ALL_PROXY": "socks5://"}, "https://hub.ts.net", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := get(tc.env, tc.url)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("got %q, %v; want %q (fail %v)", got, err, tc.want, tc.fail)
			}
		})
	}
	if _, err := get(map[string]string{"ALL_PROXY": "ftp://user:pw@s:1"}, "https://hub.ts.net"); err == nil || strings.Contains(err.Error(), "pw") {
		t.Fatalf("credentials leaked or no error: %v", err)
	}
}

// The default client of NewUpstream reads the real environment.
func TestNewUpstreamReadsTheEnvironment(t *testing.T) {
	up, err := NewUpstream("https://hub.tail1234.ts.net", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALL_PROXY", "socks5h://127.0.0.1:1055")
	t.Setenv("NO_PROXY", "")
	req, _ := http.NewRequest("GET", "https://hub.tail1234.ts.net/api/x", nil)
	p, err := up.client.Transport.(*http.Transport).Proxy(req)
	if err != nil || p == nil || p.Host != "127.0.0.1:1055" {
		t.Fatalf("%v %v", p, err)
	}
}

// The hub's calls to its peers still ignore the proxy environment.
func TestHubToPeerClientIgnoresProxy(t *testing.T) {
	t.Setenv("ALL_PROXY", "socks5h://127.0.0.1:1055")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	o := Options{}
	o.defaults()
	if o.Client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the hub-to-peer client has a proxy")
	}
}
