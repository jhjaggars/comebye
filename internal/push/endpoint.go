package push

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// pushHosts are the push services of the browsers we expect: Chrome,
// Edge, Samsung Internet, Brave, Opera (FCM), Firefox (Mozilla autopush),
// Safari / iOS (Apple), and legacy Edge (WNS). An endpoint must be one of
// these hosts or a subdomain. The server makes outbound POSTs to whatever
// endpoint a client registers, so an allowlist turns "an allowlisted user
// can make the server POST anywhere" (SSRF) into "can POST to a push
// service", on top of the https-only and public-IP checks.
var pushHosts = []string{
	"fcm.googleapis.com",
	"android.googleapis.com",
	"push.services.mozilla.com", // updates.push.services.mozilla.com
	"push.apple.com",            // web.push.apple.com
	"notify.windows.com",        // *.notify.windows.com
}

// MaxEndpointLen bounds a stored endpoint URL.
const MaxEndpointLen = 2048

// ValidateEndpoint checks a subscription endpoint before it is stored: an
// absolute https URL on the default port, no credentials, and a host on
// the push-service allowlist (which also rules out IP literals, localhost
// and tailnet names).
func ValidateEndpoint(raw string) error {
	if len(raw) > MaxEndpointLen {
		return errors.New("endpoint too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("endpoint: %v", err)
	}
	if u.Scheme != "https" {
		return errors.New("endpoint must be https")
	}
	if u.User != nil {
		return errors.New("endpoint must not carry credentials")
	}
	if p := u.Port(); p != "" && p != "443" {
		return errors.New("endpoint must use the default https port")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return errors.New("endpoint has no host")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return errors.New("endpoint must not be an IP address")
	}
	for _, h := range pushHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("endpoint host %q is not a known push service", host)
}

// publicAddr reports whether ip is a globally routable unicast address,
// excluding loopback, private (RFC 1918 / ULA), link-local, CGNAT (which
// covers Tailscale's 100.64.0.0/10), and unspecified ranges.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT, Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"), // Tailscale ULA (also covered by IsPrivate)
}

// safeDialControl refuses connections to non-public addresses after DNS
// resolution, so a push-service name that resolves (or is rebound) to an
// internal address cannot be used to reach it.
func safeDialControl(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("push: refusing to dial %q: %v", address, err)
	}
	if !publicAddr(ap.Addr()) {
		return fmt.Errorf("push: refusing to dial non-public address %s", ap.Addr())
	}
	return nil
}

// newHTTPClient is the client used for push service requests: public
// addresses only, no redirects (a push service never redirects, and a
// redirect would bypass the endpoint check), bounded timeouts, and no
// proxy from the environment.
func newHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: safeDialControl}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// audience is the VAPID aud claim: the endpoint's origin.
func audience(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}
