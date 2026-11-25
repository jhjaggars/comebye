package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// The viewer (#136 slice 4) tells the app's users apart without telling it who they are. The app saves the last lists
// it read in the browser (IndexedDB, per origin); each record carries the viewer it was read for, and the app wipes
// them when /api/version answers a different one: another Tailscale login on the same browser profile never sees the
// previous one's lists. It is an HMAC of the login under a key only the hub holds, cut to 16 hex digits: stable for a
// login across restarts (the key is kept in the state directory), different between logins, and the login cannot be
// read back from it. It is hygiene between accounts, not a security boundary: whoever can open the browser profile
// can read what is saved there.
const viewerKeyFile = "viewer.key"

// openViewerKey reads the hub's viewer key from dir, or makes one and keeps it there. Without a state directory, or if
// it cannot be written, the key lasts as long as the process: a restart then reads as a new viewer, and the app only
// clears what it saved.
func openViewerKey(dir string) []byte {
	if dir != "" {
		p := filepath.Join(dir, viewerKeyFile)
		if b, err := os.ReadFile(p); err == nil && len(b) == 32 {
			return b
		}
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			if err := os.WriteFile(filepath.Join(dir, viewerKeyFile), k, 0o600); err != nil {
				log.Printf("viewer key: %v (it lasts until a restart)", err)
			}
		}
	}
	return k
}

// viewerOf is the caller's viewer value: 16 hex digits of HMAC-SHA256(key, login).
func (s *Server) viewerOf(r *http.Request) string {
	m := hmac.New(sha256.New, s.viewerKey)
	m.Write([]byte("comebye-viewer\x00" + callerLogin(r)))
	return hex.EncodeToString(m.Sum(nil))[:16]
}
