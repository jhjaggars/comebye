package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func asst(model string, in, cc, cr int, side bool) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":%t,"message":{"model":%q,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":9}}}`, side, model, in, cc, cr)
}

func write(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLastContext(t *testing.T) {
	p := write(t,
		asst("claude-opus", 1, 100, 1000, false),
		`{"type":"user","message":{"content":"hi"}}`,
		asst("claude-opus", 2, 500, 150000, false),
		asst("claude-haiku", 3, 9, 9, true), // a subagent: skipped
		asst("<synthetic>", 0, 0, 0, false), // made up by the client: skipped
		`{"type":"system","subtype":"turn_duration"}`,
	)
	c, ok, err := LastContext(p)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if c.Tokens != 150502 || c.Model != "claude-opus" {
		t.Fatalf("got %+v", c)
	}
}

func TestLastContextNone(t *testing.T) {
	p := write(t, `{"type":"user","message":{"content":"hi"}}`)
	if _, ok, err := LastContext(p); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if _, _, err := LastContext(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing file is an error")
	}
}

// The last assistant line may lie far before the end of a big file.
func TestLastContextFarFromEnd(t *testing.T) {
	big := `{"type":"user","message":{"content":"` + strings.Repeat("x", 300<<10) + `"}}`
	p := write(t, asst("m", 1, 2, 3, false), big, big, big)
	c, ok, _ := LastContext(p)
	if !ok || c.Tokens != 6 {
		t.Fatalf("got %+v ok=%v", c, ok)
	}
}

func TestFileContains(t *testing.T) {
	filler := strings.Repeat("a", 3<<20)
	p := write(t, filler+"NEEDLE-123"+filler)
	if ok, err := FileContains(p, []byte("NEEDLE-123")); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, _ := FileContains(p, []byte("NEEDLE-124")); ok {
		t.Fatal("false positive")
	}
	// across the 1 MB read boundary
	q := write(t, strings.Repeat("b", (1<<20)-4)+"SPLIT-MARK"+"c")
	if ok, _ := FileContains(q, []byte("SPLIT-MARK")); !ok {
		t.Fatal("missed a needle that spans two reads")
	}
}
