package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// vapidFile holds the VAPID private key (PKCS#8 PEM) in the state dir.
const vapidFile = "vapid.pem"

// DefaultSubject is the fallback VAPID contact when no usable email is
// known. It is a public https URL, which Apple's push service accepts.
const DefaultSubject = "https://github.com/jhjaggars/herdr-phone"

// LoadOrCreateVAPID reads the VAPID key from dir, generating and saving a
// new P-256 key (mode 0600) on first run. Changing the key invalidates
// every existing browser subscription.
func LoadOrCreateVAPID(dir string) (*ecdsa.PrivateKey, error) {
	path := filepath.Join(dir, vapidFile)
	b, err := os.ReadFile(path)
	if err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: no PEM block", path)
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok || ek.Curve != elliptic.P256() {
			return nil, fmt.Errorf("%s: not a P-256 ECDSA key", path)
		}
		return ek, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := writeFileAtomic(path, data); err != nil {
		return nil, err
	}
	return key, nil
}

// publicKeyBytes is the uncompressed (65-byte) P-256 point, the form
// browsers expect as applicationServerKey and in the VAPID k= parameter.
func publicKeyBytes(key *ecdsa.PrivateKey) ([]byte, error) {
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	return pub.Bytes(), nil
}

// ValidateSubject checks a VAPID sub claim. RFC 8292 wants a mailto: or
// https: contact URI; Apple's push service answers 403 BadJwtToken for
// anything else, and in practice also for unroutable contacts such as
// admin@localhost, so those are rejected here up front.
func ValidateSubject(sub string) error {
	switch {
	case strings.HasPrefix(sub, "mailto:"):
		addr := strings.TrimPrefix(sub, "mailto:")
		if addr != strings.TrimSpace(addr) || strings.ContainsAny(addr, "<> ") {
			return fmt.Errorf("push subject %q: use mailto:user@example.com with no spaces or angle brackets", sub)
		}
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("push subject %q: %v", sub, err)
		}
		_, domain, _ := strings.Cut(addr, "@")
		if !publicDomain(domain) {
			return fmt.Errorf("push subject %q: the email domain must be a public DNS name", sub)
		}
		return nil
	case strings.HasPrefix(sub, "https://"):
		u, err := url.Parse(sub)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("push subject %q: invalid URL", sub)
		}
		if !publicDomain(u.Hostname()) {
			return fmt.Errorf("push subject %q: the host must be a public DNS name", sub)
		}
		return nil
	}
	return fmt.Errorf("push subject %q: must start with mailto: or https://", sub)
}

// publicDomain is a heuristic for "a contact a push service will accept":
// a dotted DNS name that is not localhost, an IP, or a private suffix.
func publicDomain(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return false
	}
	for _, s := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa", ".ts.net", ".test", ".invalid", ".example"} {
		if strings.HasSuffix("."+host, s) {
			return false
		}
	}
	return true
}

// DefaultSubjectFor picks the VAPID contact: mailto:<login> for the first
// allowlisted Tailscale login that is a usable email address (GitHub-auth
// tailnets have logins like user@github, which is not), else
// DefaultSubject.
func DefaultSubjectFor(logins []string) string {
	for _, l := range logins {
		if s := "mailto:" + l; ValidateSubject(s) == nil {
			return s
		}
	}
	return DefaultSubject
}
