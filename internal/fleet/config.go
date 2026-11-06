// Package fleet aggregates several comebye instances into one view.
//
// Every host runs its own comebye against its local herdr socket. One
// of them, the hub, is configured with the others as peers: it keeps a
// long-lived subscription to each peer's /api/events stream, merges the
// peers' agents into its own broadcasts (so a browser still needs one SSE
// stream), proxies the per-agent API calls to the owning peer, and sends
// notifications for peer agents through the hub's own channels.
//
// Hub-to-peer requests go through each peer's Tailscale Serve URL, which
// attaches the identity of the hub device's owner, so each peer's existing
// -ts-allow-login admits the hub without any shared secret.
package fleet

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Peer is one configured peer instance.
type Peer struct {
	// Name is the host slug used in agent IDs and URLs.
	Name string
	// URLs are the peer's addresses, tried in order: usually one
	// Tailscale Serve URL, optionally followed by fallbacks (a MagicDNS
	// name plus a tailnet IP, or a second name) for when a rename or a
	// stale Serve config breaks the first one.
	URLs []*url.URL
	// Source is SourceStatic (-peers) or SourceDynamic (the registry); the
	// provenance fields below are set for dynamic peers.
	Source  string
	AddedBy string
	AddedAt time.Time
	Tag     string
	// Instance is the peer's incarnation id (see Record.Instance); static
	// peers have none.
	Instance string
}

// Where a peer came from.
const (
	SourceStatic  = "static"
	SourceDynamic = "dynamic"
)

// Addrs lists the peer's addresses, " "-separated, for logs.
func (p Peer) Addrs() string {
	parts := make([]string, len(p.URLs))
	for i, u := range p.URLs {
		parts[i] = u.String()
	}
	return strings.Join(parts, " ")
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName reports whether s is a usable host name: a lowercase DNS label
// ([a-z0-9-], at most 63 characters, no leading or trailing dash).
func ValidName(s string) bool { return nameRE.MatchString(s) }

// DefaultHostName is the local instance's default name: the machine's
// short hostname as a valid label, else "local".
func DefaultHostName() string {
	h, err := os.Hostname()
	if err != nil {
		return "local"
	}
	if n := sanitizeName(h); n != "" {
		return n
	}
	return "local"
}

// sanitizeName turns the first DNS label of h into a valid name.
func sanitizeName(h string) string {
	label, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(h)), ".")
	var b strings.Builder
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	if !ValidName(s) {
		return ""
	}
	return s
}

// ParseOptions adjusts ParsePeers.
type ParseOptions struct {
	// LocalName is the local instance's name, which a peer may not reuse.
	LocalName string
	// AllowHTTPLoopback admits plain-http peers on 127.0.0.1, [::1] or
	// localhost. TEST ONLY: it exists so two instances can be wired
	// together on one machine without Tailscale Serve. Real peers must be
	// https Tailscale Serve URLs, which is what carries the hub's identity.
	AllowHTTPLoopback bool
}

// ParsePeers parses a comma-separated list of "name=url[|url...]"
// entries. The name is optional ("https://box.tailnet.ts.net" is named
// "box"). A peer may list more than one address (a MagicDNS name plus a
// tailnet IP, or two names), separated by "|": they are tried in order
// and the first that answers is used.
func ParsePeers(spec string, opt ParseOptions) ([]Peer, error) {
	var out []Peer
	names := map[string]bool{}
	urls := map[string]bool{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, raw := "", entry
		if i := strings.Index(entry, "="); i >= 0 && !strings.Contains(entry[:i], "://") {
			name, raw = strings.TrimSpace(entry[:i]), strings.TrimSpace(entry[i+1:])
		}
		var addrs []*url.URL
		for _, a := range strings.Split(raw, "|") {
			a = strings.TrimSpace(a)
			if a == "" {
				return nil, fmt.Errorf("peer %q: empty address (addresses are separated by |)", entry)
			}
			u, err := parsePeerURL(a, opt.AllowHTTPLoopback)
			if err != nil {
				return nil, fmt.Errorf("peer %q: %w", entry, err)
			}
			if urls[u.String()] {
				return nil, fmt.Errorf("peer %q: duplicate URL %s", entry, u)
			}
			urls[u.String()] = true
			addrs = append(addrs, u)
		}
		if name == "" {
			name = sanitizeName(addrs[0].Hostname())
			if name == "" {
				return nil, fmt.Errorf("peer %q: cannot derive a name from the host; use name=URL", entry)
			}
		}
		name = strings.ToLower(name)
		if !ValidName(name) {
			return nil, fmt.Errorf("peer %q: name %q must be a lowercase DNS label (a-z, 0-9, -)", entry, name)
		}
		if name == opt.LocalName {
			return nil, fmt.Errorf("peer %q: name %q is the local host's name (-host-name)", entry, name)
		}
		if names[name] {
			return nil, fmt.Errorf("peer %q: duplicate name %q", entry, name)
		}
		names[name] = true
		out = append(out, Peer{Name: name, URLs: addrs})
	}
	return out, nil
}

// AddrOptions constrains the addresses of a peer added at runtime.
type AddrOptions struct {
	// AllowHTTPLoopback: see ParseOptions. TEST ONLY.
	AllowHTTPLoopback bool
	// Domains, when non-empty, restricts every address to a tailnet host:
	// a name under one of these suffixes (".ts.net"), or - as a fallback
	// address - a tailnet IP (100.64.0.0/10, fd7a:115c:a1e0::/48).
	Domains []string
}

var tailnetV4 = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
var tailnetV6 = net.IPNet{IP: net.ParseIP("fd7a:115c:a1e0::"), Mask: net.CIDRMask(48, 128)}

// tailnetHost reports whether host is a tailnet name or address.
func tailnetHost(host string, domains []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		return tailnetV4.Contains(ip) || tailnetV6.Contains(ip)
	}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			continue
		}
		if !strings.HasPrefix(d, ".") {
			d = "." + d
		}
		if strings.HasSuffix(host, d) && len(host) > len(d) {
			return true
		}
	}
	return false
}

// ParseAddrs validates a peer's addresses (https, no path, no duplicates)
// and, with opt.Domains, that each is a tailnet host.
func ParseAddrs(raw []string, opt AddrOptions) ([]*url.URL, error) {
	if len(raw) == 0 {
		return nil, errors.New("at least one address is required")
	}
	if len(raw) > 4 {
		return nil, errors.New("at most 4 addresses per peer")
	}
	var out []*url.URL
	seen := map[string]bool{}
	for _, a := range raw {
		u, err := parsePeerURL(strings.TrimSpace(a), opt.AllowHTTPLoopback)
		if err != nil {
			return nil, fmt.Errorf("address %q: %w", a, err)
		}
		if len(opt.Domains) > 0 && !(opt.AllowHTTPLoopback && isLoopbackHost(u.Hostname())) && !tailnetHost(u.Hostname(), opt.Domains) {
			return nil, fmt.Errorf("address %q: not a tailnet host (want a name under %s, or a tailnet IP): a peer receives the hub's requests and is trusted with its agents", a, strings.Join(opt.Domains, ", "))
		}
		if seen[u.String()] {
			return nil, fmt.Errorf("duplicate address %s", u)
		}
		seen[u.String()] = true
		out = append(out, u)
	}
	return out, nil
}

func parsePeerURL(raw string, allowHTTPLoopback bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return nil, errors.New("want an absolute URL such as https://box.tailnet.ts.net")
	case u.User != nil:
		return nil, errors.New("credentials in the URL are not allowed")
	case u.Path != "" && u.Path != "/", u.RawQuery != "", u.Fragment != "", u.ForceQuery:
		return nil, errors.New("the URL must not have a path, query or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !allowHTTPLoopback || !isLoopbackHost(u.Hostname()) {
			return nil, errors.New("peers must use https (their Tailscale Serve URL)")
		}
	default:
		return nil, fmt.Errorf("unsupported scheme %q: peers must use https", u.Scheme)
	}
	return &url.URL{Scheme: strings.ToLower(u.Scheme), Host: strings.ToLower(u.Host)}, nil
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
