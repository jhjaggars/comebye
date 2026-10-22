// Package agenttoken manages the local agent token: a random secret in the
// state directory that the comebye CLI sends to the local server, which
// then treats the request as coming from an agent (the restricted "agent"
// role) rather than from the user.
//
// The token keeps a well-behaved coordinator inside the workflow; it is
// not a boundary against a local process with a shell, which can read the
// file (it runs as the same user) or forge the user's identity header on
// the loopback port. See SECURITY.md.
package agenttoken

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the token's file in the state directory.
const FileName = "agent-token"

// tokenLen is the hex length of a token (32 random bytes).
const tokenLen = 64

// Path is the token file in dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// valid reports whether s looks like a token this package wrote.
func valid(s string) bool {
	if len(s) != tokenLen {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// LoadOrCreate returns the token in dir, creating it (32 random bytes as
// hex, mode 0600; dir created 0700) when missing or unreadable as a token.
// An existing file whose mode is wider than 0600 is tightened.
func LoadOrCreate(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("no state directory")
	}
	p := Path(dir)
	if tok, err := Read(dir); err == nil {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(p, 0o600); err != nil {
				return "", fmt.Errorf("tighten %s: %w", p, err)
			}
		}
		return tok, nil
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errMalformed) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, tokenLen/2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	tmp, err := os.CreateTemp(dir, ".agent-token-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	return tok, nil
}

var errMalformed = errors.New("malformed agent token")

// Read returns the token in dir.
func Read(dir string) (string, error) {
	b, err := os.ReadFile(Path(dir))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if !valid(tok) {
		return "", fmt.Errorf("%s: %w", Path(dir), errMalformed)
	}
	return tok, nil
}

// Equal compares a presented token with the expected one in constant time
// (an empty expected token matches nothing).
func Equal(got, want string) bool {
	if want == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
