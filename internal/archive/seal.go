package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
)

// Segments are encrypted with age (filippo.io/age): each segment is an age
// file, so `age -d -i identity.key` can read one after the 5-byte segment
// prefix is stripped, and the archive is not tied to a Comebye-only format.
//
// Recipients: any number, of two kinds: native age keys ("age1...") and
// SSH public keys ("ssh-ed25519 ...", "ssh-rsa ..."). A segment is
// decryptable by the identity of any one recipient it was written for. To
// add a recipient later (for example a shared homelab identity), add it to
// the recipient list: only segments written afterwards include it; older
// segments stay readable with the original identity, or can be re-wrapped
// with `comebye archive rewrap` (see docs/archive.md).
//
// Identities: native age identities ("AGE-SECRET-KEY-1...", one or many in
// a file, so old and new can sit side by side) or an unencrypted OpenSSH
// private key. Passphrase-protected SSH keys and plugin identities
// (age-plugin-yubikey, ...) are not wired in yet.

// Encryption modes. The mode is chosen explicitly in the configuration and
// recorded in the archive (see EnsureMode); an archive is never silently a
// mixture.
const (
	ModeAge  = "age"  // every segment is an age file
	ModeNone = "none" // segments are zstd only: the archive must live only where something else encrypts it
)

// Sealer encrypts a segment's bytes.
type Sealer interface {
	Seal(plain []byte) ([]byte, error)
	// Mode is ModeAge or ModeNone; it picks the segment magic.
	Mode() string
}

// Opener decrypts them.
type Opener interface {
	Open(sealed []byte) ([]byte, error)
	Mode() string
}

// NoneSealer is the no-encryption mode: segments hold the same header and
// zstd bytes, without the age layer.
type NoneSealer struct{}

func (NoneSealer) Seal(plain []byte) ([]byte, error) { return plain, nil }
func (NoneSealer) Open(b []byte) ([]byte, error)     { return b, nil }
func (NoneSealer) Mode() string                      { return ModeNone }

// GenerateIdentity returns a new native age identity (secret) and its
// recipient (public).
func GenerateIdentity() (identity, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return id.String(), id.Recipient().String(), nil
}

// ParseRecipients reads recipients from a literal list (comma or newline
// separated) or, when spec names a file, from that file (one per line, #
// comments allowed). It rejects an empty list.
func ParseRecipients(spec string) ([]age.Recipient, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("archive: no recipients")
	}
	text := spec
	if !strings.HasPrefix(spec, "age1") && !strings.HasPrefix(spec, "ssh-") && !strings.Contains(spec, "\n") && !strings.Contains(spec, ",") {
		b, err := os.ReadFile(expand(spec))
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	var out []age.Recipient
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' }) {
		line := strings.TrimSpace(part)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "age1"):
			r, err := age.ParseX25519Recipient(line)
			if err != nil {
				return nil, fmt.Errorf("archive: bad age recipient: %w", err)
			}
			out = append(out, r)
		case strings.HasPrefix(line, "ssh-"):
			r, err := agessh.ParseRecipient(line)
			if err != nil {
				return nil, fmt.Errorf("archive: bad ssh recipient: %w", err)
			}
			out = append(out, r)
		default:
			return nil, fmt.Errorf("archive: unsupported recipient %q (want age1... or ssh-...)", clip(line, 20))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("archive: no recipients found")
	}
	return out, nil
}

type ageSealer struct{ rcpts []age.Recipient }

// NewSealer encrypts to every recipient in spec (see ParseRecipients).
func NewSealer(spec string) (Sealer, error) {
	r, err := ParseRecipients(spec)
	if err != nil {
		return nil, err
	}
	return &ageSealer{r}, nil
}

func (s *ageSealer) Mode() string { return ModeAge }

func (s *ageSealer) Seal(plain []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, s.rcpts...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type ageOpener struct{ ids []age.Identity }

// ParseIdentities reads identities from text (a file's contents).
func ParseIdentities(text string) ([]age.Identity, error) {
	ids, err := age.ParseIdentities(strings.NewReader(text))
	if err == nil && len(ids) > 0 {
		return ids, nil
	}
	if strings.Contains(text, "PRIVATE KEY") {
		id, serr := agessh.ParseIdentity([]byte(text))
		if serr != nil {
			return nil, fmt.Errorf("archive: ssh identity: %w (passphrase-protected SSH keys are not supported yet)", serr)
		}
		return []age.Identity{id}, nil
	}
	if err == nil {
		err = errors.New("no identities found")
	}
	return nil, fmt.Errorf("archive: %w", err)
}

// NewOpener decrypts with any of the identities in text.
func NewOpener(identityText string) (Opener, error) {
	ids, err := ParseIdentities(identityText)
	if err != nil {
		return nil, err
	}
	return &ageOpener{ids}, nil
}

func (o *ageOpener) Mode() string { return ModeAge }

func (o *ageOpener) Open(sealed []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(sealed), o.ids...)
	if err != nil {
		return nil, fmt.Errorf("archive: cannot decrypt (wrong identity, or the object was altered): %w", err)
	}
	return io.ReadAll(r)
}

// RecipientsOf returns the public recipients of the native identities in
// text (for a config that has only the identity).
func RecipientsOf(identityText string) (string, error) {
	ids, err := ParseIdentities(identityText)
	if err != nil {
		return "", err
	}
	var out []string
	for _, id := range ids {
		if x, ok := id.(*age.X25519Identity); ok {
			out = append(out, x.Recipient().String())
		}
	}
	if len(out) == 0 {
		return "", errors.New("archive: the identity has no native age recipient; pass the public key explicitly")
	}
	return strings.Join(out, ","), nil
}
