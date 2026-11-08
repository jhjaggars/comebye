package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/stt"
)

func sttEnv(t *testing.T, serverBody string, wantSum string) (*Env, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(serverBody)) }))
	t.Cleanup(srv.Close)
	if wantSum == "" {
		s := sha256.Sum256([]byte(serverBody))
		wantSum = hex.EncodeToString(s[:])
	}
	old := sttFiles
	sttFiles = func() []stt.File {
		return []stt.File{{Path: "ort/rt.wasm", URL: srv.URL + "/rt.wasm", SHA256: wantSum, Size: int64(len(serverBody))}}
	}
	t.Cleanup(func() { sttFiles = old })
	state := t.TempDir()
	var out, errb bytes.Buffer
	e := &Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), HTTP: srv.Client(),
		Getenv: func(k string) string { return map[string]string{"COMEBYE_STATE_DIR": state}[k] }}
	return e, &out, &errb, state
}

func TestSTTFetchChecksumMismatchIsRefused(t *testing.T) {
	e, out, errb, state := sttEnv(t, "tampered runtime", strings.Repeat("ab", 32))
	if code := run(e, "stt", "fetch"); code != 1 {
		t.Fatalf("exit %d, want 1\nout: %s\nerr: %s", code, out, errb)
	}
	if !strings.Contains(errb.String(), "refused") || !strings.Contains(errb.String(), "ort/rt.wasm") {
		t.Fatalf("stderr: %s", errb)
	}
	if _, err := os.Stat(filepath.Join(stt.Dir(state), "ort", "rt.wasm")); !os.IsNotExist(err) {
		t.Fatalf("installed a file that failed its checksum: %v", err)
	}
}

func TestSTTFetchInstallsAndPrintsLicences(t *testing.T) {
	e, out, errb, state := sttEnv(t, "good runtime", "")
	if code := run(e, "stt", "fetch"); code != 0 {
		t.Fatalf("exit %d\nout: %s\nerr: %s", code, out, errb)
	}
	if b, _ := os.ReadFile(filepath.Join(stt.Dir(state), "ort", "rt.wasm")); string(b) != "good runtime" {
		t.Fatalf("installed %q", b)
	}
	for _, want := range []string{"Whisper", "MIT", "Apache-2.0", "sha256 matches", "is installed in"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestSTTStatus(t *testing.T) {
	e, out, _, _ := sttEnv(t, "x", "")
	// Real list: nothing is installed in a fresh state dir.
	sttFiles = stt.Files
	if code := run(e, "stt", "status"); code != 1 || !strings.Contains(out.String(), "not installed") || !strings.Contains(out.String(), "comebye stt fetch") {
		t.Fatalf("exit %d: %s", code, out)
	}
	out.Reset()
	if code := run(e, "stt", "status", "--json"); code != 1 || !strings.Contains(out.String(), `"installed":false`) {
		t.Fatalf("json: %d %s", code, out)
	}
	if code := run(e, "stt", "bogus"); code != 2 {
		t.Fatalf("bogus: %d", code)
	}
}
