package opencode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/transcript"
	"github.com/jhjaggars/comebye/internal/usage"
)

// testdata/export.json is the start of a real `opencode export` (1.19.5).
func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/export.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConvertReadsLikeAPiSession(t *testing.T) {
	out, id, cwd, err := Convert(fixture(t))
	if err != nil || id != "ses_e2e77bc70ffeS3b6deA1gr5A20" || !strings.Contains(cwd, "scratchpad/repo") {
		t.Fatalf("%q %q %v", id, cwd, err)
	}
	path := filepath.Join(t.TempDir(), id+".jsonl")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := transcript.Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var user, assistant, tool int
	var sawResult, sawFirst bool
	for _, m := range tr.Messages {
		switch m.Kind {
		case "user":
			user++
			sawFirst = sawFirst || strings.Contains(m.Text, "Line two")
		case "assistant":
			assistant++
		case "tool":
			tool++
			if m.Tool == "bash" && strings.Contains(m.Result, "ssh_config") {
				sawResult = true
			}
		}
	}
	if user < 1 || assistant < 1 || tool < 1 || !sawResult || !sawFirst {
		t.Errorf("user %d assistant %d tool %d result %v", user, assistant, tool, sawResult)
	}
}

func TestConvertUsageAndRejectedTool(t *testing.T) {
	out, _, _, _ := Convert(fixture(t))
	var calls, in, cr int64
	for _, l := range bytes.Split(out, []byte("\n")) {
		s, _, ok := usage.ParsePi(l)
		if !ok {
			t.Fatalf("bad line %s", l)
		}
		if s != nil {
			calls++
			in += s.Tok.In
			cr += s.Tok.CR
			if !strings.HasPrefix(s.Model, "opencode/") {
				t.Errorf("model %q", s.Model)
			}
		}
	}
	if calls < 2 || in == 0 || cr == 0 {
		t.Errorf("usage: calls %d in %d cacheRead %d", calls, in, cr)
	}
}

func TestConvertStopsAtAnUnfinishedMessage(t *testing.T) {
	doc := `{"info":{"id":"ses_abcdefgh12","directory":"/x","time":{"created":1}},"messages":[
 {"info":{"id":"m1","role":"user","time":{"created":2}},"parts":[{"type":"text","text":"hi"}]},
 {"info":{"id":"m2","role":"assistant","modelID":"m","providerID":"p","time":{"created":3}},"parts":[{"type":"text","text":"partial"}]},
 {"info":{"id":"m3","role":"user","time":{"created":4}},"parts":[{"type":"text","text":"later"}]}]}`
	out, _, _, err := Convert([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(out, []byte("\n")); n != 2 || bytes.Contains(out, []byte("partial")) || bytes.Contains(out, []byte("later")) {
		t.Errorf("want header + first user only, got:\n%s", out)
	}
}

func TestMirrorSyncIsAppendOnly(t *testing.T) {
	raw := fixture(t)
	calls := 0
	m := &Mirror{Dir: t.TempDir(), MinGap: -1, Export: func(context.Context, string) ([]byte, error) { calls++; return raw, nil }}
	id := "ses_e2e77bc70ffeS3b6deA1gr5A20"
	p, err := m.Sync(context.Background(), id)
	if err != nil || p == "" || m.Path(id) != p {
		t.Fatalf("%q %v", p, err)
	}
	if !strings.Contains(p, "scratchpad-repo") {
		t.Errorf("path %q", p)
	}
	first, _ := os.ReadFile(p)
	// A later export that is shorter (a session reverted) never shortens the copy.
	short := bytes.Replace(raw, []byte(`"messages"`), []byte(`"messages":[],"x"`), 1)
	m.Export = func(context.Context, string) ([]byte, error) { return short, nil }
	if _, err := m.Sync(context.Background(), id); err != nil {
		t.Logf("sync of a reverted export: %v", err)
	}
	if now, _ := os.ReadFile(p); !bytes.Equal(now, first) {
		t.Error("the mirror was rewritten to something that is not an extension of it")
	}
	if _, err := m.Sync(context.Background(), "../../etc/passwd"); err == nil {
		t.Error("an id that is not a session id must be refused")
	}
}

func TestMirrorMinGap(t *testing.T) {
	calls := 0
	m := &Mirror{Dir: t.TempDir(), Export: func(context.Context, string) ([]byte, error) { calls++; return fixture(t), nil }}
	for i := 0; i < 3; i++ {
		if _, err := m.Sync(context.Background(), "ses_e2e77bc70ffeS3b6deA1gr5A20"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("exports %d, want 1 within the gap", calls)
	}
}

// The resolver and the mirror must agree on where the mirror is by default.
func TestDefaultDirAgreesWithTheResolver(t *testing.T) {
	t.Setenv("COMEBYE_STATE_DIR", t.TempDir())
	if DefaultDir() != transcript.DefaultOpencodeDir() {
		t.Errorf("%q vs %q", DefaultDir(), transcript.DefaultOpencodeDir())
	}
}

// The question tool's card previews the question, not the arguments' JSON.
func TestQuestionToolCardPreviewsTheQuestion(t *testing.T) {
	doc := `{"info":{"id":"ses_abcdefgh12","directory":"/x","time":{"created":1}},"messages":[
 {"info":{"id":"m1","role":"user","time":{"created":2}},"parts":[{"type":"text","text":"ask"}]},
 {"info":{"id":"m2","role":"assistant","modelID":"m","providerID":"p","time":{"created":3,"completed":4}},"parts":[
  {"type":"tool","tool":"question","callID":"c1","state":{"status":"completed","input":{"questions":[{"question":"Which fruit?","options":[{"label":"Apple"}]}]},"output":"You chose Apple."}},
  {"type":"tool","tool":"read","callID":"c2","state":{"status":"completed","input":{"filePath":"/x/a.go"},"output":"x"}}]}]}`
	out, _, _, err := Convert([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := transcript.Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range tr.Messages {
		if m.Kind == "tool" {
			got[m.Tool] = m.Args
		}
	}
	if got["question"] != "Which fruit?" || got["read"] != "/x/a.go" {
		t.Errorf("card previews: %v", got)
	}
}

// A mirror written by an older converter (its earlier lines differ) keeps its
// lines and gains only the new ones.
func TestMirrorKeepsEarlierLinesWhenTheConverterChanged(t *testing.T) {
	raw := fixture(t)
	full, _, _, _ := Convert(raw)
	lines := bytes.SplitAfter(full, []byte("\n"))
	old := append([]byte(nil), lines[0]...)
	old = append(old, []byte(`{"type":"message","id":"older","message":{"role":"user","content":[{"type":"text","text":"older converter"}]}}`+"\n")...)
	m := &Mirror{Dir: t.TempDir(), MinGap: -1, Export: func(context.Context, string) ([]byte, error) { return raw, nil }}
	id := "ses_e2e77bc70ffeS3b6deA1gr5A20"
	p := filepath.Join(m.Dir, Slug("/tmp/claude-1000/-home-jhjaggars--worktrees-comebye-feat-opencode-support/7efd0587-0fbb-4d11-9f48-7f2a1c01ea02/scratchpad/repo"), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sync(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !bytes.HasPrefix(got, old) || !bytes.HasSuffix(got, lines[len(lines)-2]) || len(got) <= len(old) {
		t.Errorf("mirror: %d bytes, old %d", len(got), len(old))
	}
}
