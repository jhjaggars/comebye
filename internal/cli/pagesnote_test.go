package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pages note takes its details from standard input or from the text given,
// and never opens a file: a path is just text.
func TestPagesNoteContextIsNeverAFile(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-FILE-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, e, _, errb := newFake(t)
	f.answers["POST /api/pages/pg_1/note"] = answer{code: 201, body: `{"todo":{"id":"t1"}}`}
	post := func() map[string]any {
		var body map[string]any
		json.Unmarshal([]byte(f.find("POST", "/api/pages/pg_1/note").body), &body)
		return body
	}

	e.Stdin = strings.NewReader("from stdin")
	if code := run(e, "pages", "note", "pg_1", "finding", "--context", "-"); code != 0 {
		t.Fatalf("stdin: %d %s", code, errb)
	}
	if post()["details"] != "from stdin" {
		t.Fatalf("stdin body = %v", post())
	}

	if code := run(e, "pages", "note", "pg_1", "finding", "--context", secret); code != 0 {
		t.Fatalf("path: %d %s", code, errb)
	}
	d, _ := post()["details"].(string)
	if strings.Contains(d, "TOP-SECRET") || d != secret {
		t.Fatalf("a path must be sent as text, not read: %q", d)
	}

	e.Stdin = strings.NewReader(strings.Repeat("x", 5000))
	if code := run(e, "pages", "note", "pg_1", "finding", "--context", "-"); code == 0 {
		t.Fatal("5000 bytes of details should be refused")
	}
}
