// Package tsauth identifies tailnet callers with tailscaled's LocalAPI.
//
// Tailscale Serve attaches a login only for user-owned nodes; a tagged
// node (tag:worker) has no login. For those, the hub listens on its
// tailnet address and asks tailscaled "who is this connection?" (WhoIs)
// for the connection's source address. WireGuard authenticates that
// address, and a process on the hub cannot make a TCP connection arrive
// from another node's tailnet IP, so unlike a header the answer cannot be
// forged from the hub's loopback.
//
// A grant's app capability identifies such a node too. Tailscale Serve
// forwards the capabilities named by --accept-app-caps in a request header
// (HeaderAppCaps), for tagged nodes as well, and WhoIs reports them in its
// CapMap.
package tsauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultSocket is tailscaled's LocalAPI socket on Linux.
const DefaultSocket = "/var/run/tailscale/tailscaled.sock"

// Identity is a tailnet node as tailscaled reports it.
type Identity struct {
	// Node is the node's MagicDNS name: lowercase, no trailing dot
	// (worker-1.tailnet.ts.net).
	Node string
	// Tags are the node's ACL tags (tag:worker); empty for a user-owned node.
	Tags []string
	// Caps are the app capabilities the node holds toward this node, from
	// the tailnet policy's grants: capability name to its values.
	Caps map[string]json.RawMessage
}

// HasCap reports whether the node holds the app capability (exact name;
// the values are not looked at).
func (i Identity) HasCap(name string) bool {
	_, ok := i.Caps[name]
	return name != "" && ok
}

// HasTag reports whether the node carries tag.
func (i Identity) HasTag(tag string) bool {
	for _, t := range i.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// Resolver answers WhoIs for a connection's remote address ("ip:port").
type Resolver interface {
	WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
}

// ErrUnknown means tailscaled does not know the address (not a tailnet
// node, or loopback).
var ErrUnknown = errors.New("not a tailnet node")

// LocalAPI talks to tailscaled over its Unix socket. WhoIs answers are
// cached for a short time per IP.
type LocalAPI struct {
	Socket string
	// TTL is how long a WhoIs answer is reused (default 30s). Tag and grant
	// changes by the tailnet admin take effect after it.
	TTL time.Duration

	once   sync.Once
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cached
}

type cached struct {
	id  Identity
	exp time.Time
}

func (l *LocalAPI) init() {
	l.once.Do(func() {
		sock := l.Socket
		if sock == "" {
			sock = DefaultSocket
		}
		l.client = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
				DisableKeepAlives: true,
			},
		}
		l.cache = map[string]cached{}
	})
}

func (l *LocalAPI) get(ctx context.Context, path string, q url.Values) ([]byte, int, error) {
	l.init()
	u := "http://local-tailscaled.sock/localapi/v0/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("tailscaled: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return b, resp.StatusCode, err
}

// WhoIs identifies the node a connection came from. remoteAddr is a
// net.Conn / http.Request RemoteAddr.
func (l *LocalAPI) WhoIs(ctx context.Context, remoteAddr string) (Identity, error) {
	l.init()
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return Identity{}, ErrUnknown
	}
	ip = ip.Unmap()
	if !Tailnet(ip) {
		return Identity{}, ErrUnknown
	}
	key := ip.String()
	ttl := l.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	l.mu.Lock()
	if c, ok := l.cache[key]; ok && time.Now().Before(c.exp) {
		l.mu.Unlock()
		return c.id, nil
	}
	l.mu.Unlock()
	b, code, err := l.get(ctx, "whois", url.Values{"addr": {key}})
	if err != nil {
		return Identity{}, err
	}
	if code != http.StatusOK {
		return Identity{}, ErrUnknown
	}
	var w struct {
		Node struct {
			Name string
			Tags []string
		}
		CapMap map[string]json.RawMessage
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return Identity{}, fmt.Errorf("tailscaled whois: %w", err)
	}
	id := Identity{Node: strings.ToLower(strings.TrimSuffix(w.Node.Name, ".")), Tags: w.Node.Tags, Caps: w.CapMap}
	if id.Node == "" {
		return Identity{}, ErrUnknown
	}
	l.mu.Lock()
	if len(l.cache) > 256 {
		l.cache = map[string]cached{}
	}
	l.cache[key] = cached{id, time.Now().Add(ttl)}
	l.mu.Unlock()
	return id, nil
}

// Self reports this node's MagicDNS name and tailnet addresses.
func (l *LocalAPI) Self(ctx context.Context) (name string, ips []netip.Addr, err error) {
	b, code, err := l.get(ctx, "status", url.Values{"peers": {"false"}})
	if err != nil {
		return "", nil, err
	}
	if code != http.StatusOK {
		return "", nil, fmt.Errorf("tailscaled status: HTTP %d", code)
	}
	var s struct {
		Self struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return "", nil, err
	}
	for _, a := range s.Self.TailscaleIPs {
		if ip, err := netip.ParseAddr(a); err == nil {
			ips = append(ips, ip)
		}
	}
	return strings.ToLower(strings.TrimSuffix(s.Self.DNSName, ".")), ips, nil
}

var (
	v4 = netip.MustParsePrefix("100.64.0.0/10")
	v6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// Tailnet reports whether ip is in Tailscale's address ranges.
func Tailnet(ip netip.Addr) bool { return v4.Contains(ip) || v6.Contains(ip) }

// NormalizeTags parses a comma-separated tag list (tag:worker,tag:vm).
func NormalizeTags(spec string) ([]string, error) {
	var out []string
	for _, t := range strings.Split(spec, ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "tag:") || len(t) == 4 || strings.ContainsAny(t, " \t") {
			return nil, fmt.Errorf("tag %q: want tag:<name> (tag:worker)", t)
		}
		out = append(out, t)
	}
	return out, nil
}

// HeaderAppCaps is the header in which Tailscale Serve forwards a caller's
// app capabilities, when its handler was set up with --accept-app-caps.
// The value is a JSON object of capability name to an array of values;
// non-ASCII values are RFC 2047 encoded. Serve removes a client's copy.
const HeaderAppCaps = "Tailscale-App-Capabilities"

// HeaderHasCap reports whether a HeaderAppCaps value holds the capability.
// Anything malformed holds nothing.
func HeaderHasCap(value, name string) bool {
	if value == "" || name == "" {
		return false
	}
	dec, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return false
	}
	var caps map[string]json.RawMessage
	if err := json.Unmarshal([]byte(dec), &caps); err != nil {
		return false
	}
	_, ok := caps[name]
	return ok
}

// capNameRE is the form Tailscale requires of an app capability's name.
var capNameRE = regexp.MustCompile(`^([\pL\pN-]+\.)+[\pL\pN-]+/[\pL\pN-/]+$`)

// NormalizeCap checks an app capability's name; empty means none.
func NormalizeCap(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name != "" && !capNameRE.MatchString(name) {
		return "", fmt.Errorf("capability %q: want {domain}/{name} (example.com/cap/peer)", name)
	}
	return name, nil
}
