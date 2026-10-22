package agenttoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	tok, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 || strings.Trim(tok, "0123456789abcdef") != "" {
		t.Fatalf("token %q: want 32 random bytes as hex", tok)
	}
	fi, err := os.Stat(Path(dir))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode %v", di.Mode())
	}
	// Reused across restarts.
	again, err := LoadOrCreate(dir)
	if err != nil || again != tok {
		t.Fatalf("second load = %q, %v", again, err)
	}
	if r, err := Read(dir); err != nil || r != tok {
		t.Fatalf("Read = %q, %v", r, err)
	}
	// A file left world-readable is tightened.
	os.Chmod(Path(dir), 0o644)
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(Path(dir)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("not tightened: %v", fi.Mode())
	}
	// A malformed file is replaced; Read refuses it.
	os.WriteFile(Path(dir), []byte("short\n"), 0o600)
	if _, err := Read(dir); err == nil {
		t.Fatal("malformed token read")
	}
	fresh, err := LoadOrCreate(dir)
	if err != nil || fresh == tok || len(fresh) != 64 {
		t.Fatalf("replaced = %q, %v", fresh, err)
	}
	if _, err := LoadOrCreate(""); err == nil {
		t.Fatal("no dir accepted")
	}
}

func TestEqual(t *testing.T) {
	tok := strings.Repeat("ab", 32)
	if !Equal(tok, tok) || Equal(tok[:63]+"c", tok) || Equal("", tok) || Equal("", "") || Equal(tok, "") {
		t.Fatal("Equal")
	}
}
