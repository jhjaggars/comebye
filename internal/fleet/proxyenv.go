package fleet

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// upstreamProxy picks the proxy for a call to the hub from the environment:
// HTTPS_PROXY (or HTTP_PROXY for a plain-http hub), then ALL_PROXY; NO_PROXY
// excludes hosts; loopback is never proxied. The proxy URL may be http,
// https, socks5 or socks5h. Go always hands a socks5 proxy the host NAME and
// lets the proxy resolve it, so socks5 and socks5h behave alike: that is what
// a host whose only route to the tailnet is tailscaled's SOCKS5 listener
// (ALL_PROXY=socks5h://127.0.0.1:1055) needs, since it has no DNS for
// tailnet names.
//
// Only the peer-to-hub link uses this. The hub's calls to its peers never go
// through a proxy (see Options.Client). A malformed proxy setting fails the
// call rather than silently going direct.
func upstreamProxy(getenv func(string) string) func(*http.Request) (*url.URL, error) {
	pick := func(names ...string) string {
		for _, n := range names {
			for _, k := range []string{n, strings.ToLower(n)} {
				if v := strings.TrimSpace(getenv(k)); v != "" {
					return v
				}
			}
		}
		return ""
	}
	return func(req *http.Request) (*url.URL, error) {
		host := req.URL.Hostname()
		if host == "" || isLoopbackHost(host) || noProxy(pick("NO_PROXY"), host, req.URL.Port()) {
			return nil, nil
		}
		raw := pick("ALL_PROXY")
		if req.URL.Scheme == "https" {
			raw = firstNonEmpty(pick("HTTPS_PROXY"), raw)
		} else {
			raw = firstNonEmpty(pick("HTTP_PROXY"), raw)
		}
		if raw == "" {
			return nil, nil
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw // as curl and Go read a bare host:port
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.Hostname() == "" {
			return nil, fmt.Errorf("proxy setting %q is not a URL such as socks5h://127.0.0.1:1055", redact(raw))
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("proxy scheme %q is not supported (use http, https, socks5 or socks5h)", u.Scheme)
		}
		if u.Scheme == "socks5h" {
			// net/http knows socks5 and resolves the name at the proxy for it.
			c := *u
			c.Scheme = "socks5"
			return &c, nil
		}
		return u, nil
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// redact drops credentials from a URL for an error message.
func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		u.User = url.User("***")
		return u.String()
	}
	return raw
}

// noProxy follows the usual NO_PROXY rules: "*" matches everything; an entry
// matches the host itself or any subdomain (a leading dot or "*." is
// allowed); an IP or CIDR entry matches that address; host:port entries
// need the port to match.
func noProxy(list, host, port string) bool {
	host = strings.ToLower(host)
	ip := net.ParseIP(host)
	for _, e := range strings.Split(list, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if e == "*" {
			return true
		}
		if _, cidr, err := net.ParseCIDR(e); err == nil {
			if ip != nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		if h, p, err := net.SplitHostPort(e); err == nil {
			if p != port {
				continue
			}
			e = h
		}
		e = strings.TrimPrefix(strings.TrimPrefix(e, "*"), ".")
		if host == e || strings.HasSuffix(host, "."+e) {
			return true
		}
	}
	return false
}

// upstreamTransport is the hub link's transport: the fleet's defaults plus
// the proxy from the environment.
func upstreamTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = upstreamProxy(os.Getenv)
	return tr
}
