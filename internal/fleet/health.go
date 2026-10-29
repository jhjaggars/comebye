package fleet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Health probes. The event stream owns a peer's up/down status; these
// probes own the *why*. Every ProbeEvery (and once at startup) the hub
// asks each peer GET /api/whoami over TLS and classifies the failure when
// it does not answer, so a peer that re-registered in the tailnet (Oct 25:
// laharl became laharl-1 while its `tailscale serve` kept a stale config)
// shows "dns" or "tls" with a fix, not a bare "no such host".

// Failure classes reported in state.HostStatus.Class.
const (
	ClassDNS  = "dns"  // the name does not resolve (a Tailscale rename?)
	ClassTCP  = "tcp"  // no connection (refused, unreachable, timeout)
	ClassTLS  = "tls"  // the TLS handshake failed (stale tailscale serve?)
	ClassHTTP = "http" // the peer answered but not usefully (401, ...)
)

// peerHTTPError is a non-2xx answer from a peer (statusError's type),
// classified as ClassHTTP.
type peerHTTPError struct{ code int }

func (e peerHTTPError) Error() string { return statusMessage(e.code) }

// classifyError sorts one transport-level failure into a class and a
// message that names the likely fix. host is the address that failed; an
// IP address gets the certificate-by-name caveat, because Tailscale
// Serve's certificate is issued for the peer's DNS name.
func classifyError(err error, host string) (class, msg string) {
	if host == "" {
		// Pull the address out of the wrapped request error.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			if u, e := url.Parse(uerr.URL); e == nil {
				host = u.Host
			}
		}
	}
	var httpErr peerHTTPError
	if errors.As(err, &httpErr) {
		return ClassHTTP, httpErr.Error()
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassDNS, fmt.Sprintf("cannot resolve %s (%v); the peer may have been renamed in the tailnet or left it - update the hub's -peers", host, dnsErr.Err)
	}
	if isTLSError(err) {
		return ClassTLS, tlsMessage(err, host)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return ClassTCP, fmt.Sprintf("cannot connect to %s (%v); is comebye running there, reached through its tailscale serve?", host, opErr.Err)
	}
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return ClassTCP, "no answer from " + host + " (timeout)"
	}
	return "", err.Error()
}

// isTLSError reports whether err is a TLS handshake or certificate
// failure, however deeply it is wrapped.
func isTLSError(err error) bool {
	var recordErr tls.RecordHeaderError
	var certErr *tls.CertificateVerificationError
	var alertErr tls.AlertError
	var hostErr x509.HostnameError
	var opErr *net.OpError
	switch {
	case errors.As(err, &recordErr),
		errors.As(err, &certErr),
		errors.As(err, &alertErr),
		errors.As(err, &hostErr):
		return true
	case errors.As(err, &opErr):
		return opErr.Op == "remote handshake"
	}
	// Some verifiers only see the text (a proxy in front of the peer, an
	// older Go): trust the standard prefixes.
	msg := err.Error()
	return strings.Contains(msg, "tls: ") || strings.Contains(msg, "x509: ")
}

// alertInternalError is crypto/tls's unexported "internal error" alert
// (80): what a peer whose `tailscale serve` config is stale presents
// during the handshake.
const alertInternalError = tls.AlertError(80)

// tlsMessage explains a TLS failure, with the two causes that bit us:
// a stale `tailscale serve` (the peer answers TLS with an internal error
// alert because its config names a hostname that no longer exists) and a
// certificate that does not match the address tried (Serve's certificate
// is per DNS name, so a tailnet IP as a fallback address cannot verify).
func tlsMessage(err error, host string) string {
	var hostErr x509.HostnameError
	var alertErr tls.AlertError
	detail := err.Error()
	bare := host
	if h, _, e := net.SplitHostPort(host); e == nil {
		bare = h
	}
	switch {
	case errors.As(err, &hostErr):
		if ip := net.ParseIP(bare); ip != nil {
			return fmt.Sprintf("TLS handshake with %s failed: %v; the peer's certificate is issued for its name, not its IP - use the MagicDNS name as the peer's address", host, hostErr)
		}
	case errors.As(err, &alertErr):
		if alertErr == alertInternalError {
			return fmt.Sprintf("TLS handshake with %s failed: the server presented an internal error alert; its tailscale serve config is probably stale - on the peer run: tailscale serve reset && tailscale serve --bg 8741", host)
		}
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return fmt.Sprintf("TLS handshake with %s failed: %s; check the peer's tailscale serve (a re-registration can leave it stale)", host, detail)
}

// probe asks the peer GET /api/whoami and records the classified result.
// It never changes the peer's up/down status - the event stream owns
// that - it refreshes why a down peer is down, so the reason stays true
// (and named) for as long as the peer is unreachable.
func (p *peer) probe(ctx context.Context) {
	o := &p.f.opts
	ctx, cancel := context.WithTimeout(ctx, o.ProbeTimeout)
	defer cancel()
	resp, addr, err := p.attempt(func(base *url.URL) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointOn(base, "api", "whoami").String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)
		return o.Client.Do(req)
	})
	class, msg := "", ""
	switch {
	case err != nil:
		class, msg = classifyError(err, addr)
	case resp.StatusCode == http.StatusOK:
		resp.Body.Close()
		msg = fmt.Sprintf("the event stream is down, but the peer answers /api/whoami at %s", addr)
	default:
		class = ClassHTTP
		resp.Body.Close()
		msg = statusMessage(resp.StatusCode)
	}
	p.noteProbe(class, msg)
}

// noteProbe records a probe result: when the peer is down, the result
// becomes the shown error (the probe is more recent - and more specific -
// than whatever the stream last saw); when it is up, it is only logged.
func (p *peer) noteProbe(class, msg string) {
	changed := false
	p.mu.Lock()
	if p.status == StatusDown && p.lastErr != msg {
		p.lastErr, p.class = msg, class
		changed = true
	}
	p.mu.Unlock()
	if changed {
		p.f.opts.Logf("peer %s: health check: %s", p.name, msg)
		p.f.broadcast()
	}
}

// probeLoop probes every peer once at startup and then every
// ProbeEvery, until ctx is done.
func (f *Fleet) probeLoop(ctx context.Context) {
	interval := f.opts.ProbeEvery
	f.probeAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.probeAll(ctx)
		}
	}
}

// probeAll probes every peer at once.
func (f *Fleet) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range f.peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.probe(ctx)
		}()
	}
	wg.Wait()
}
