// Package fleet aggregates several herdr-phone instances into one view.
//
// Every host runs its own herdr-phone against its local herdr socket. One
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
)

// Peer is one configured peer instance.
type Peer struct {
	// Name is the host slug used in agent IDs and URLs.
	Name string
	// URL is the peer's base URL (scheme://host[:port], no path).
	URL *url.URL
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

// ParsePeers parses a comma-separated list of "name=https://host" entries.
// The name is optional ("https://box.tailnet.ts.net" is named "box").
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
		u, err := parsePeerURL(raw, opt.AllowHTTPLoopback)
		if err != nil {
			return nil, fmt.Errorf("peer %q: %w", entry, err)
		}
		if name == "" {
			name = sanitizeName(u.Hostname())
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
		if urls[u.String()] {
			return nil, fmt.Errorf("peer %q: duplicate URL %s", entry, u)
		}
		names[name], urls[u.String()] = true, true
		out = append(out, Peer{Name: name, URL: u})
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
