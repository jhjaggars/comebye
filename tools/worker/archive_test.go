package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- a small S3 ----

type miniS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func startS3(t *testing.T) (*miniS3, *httptest.Server) {
	m := &miniS3{objs: map[string][]byte{}}
	s := httptest.NewServer(m)
	t.Cleanup(s.Close)
	return m, s
}

func (m *miniS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/bucket"), "/")
	switch {
	case r.Method == "GET" && r.URL.Query().Get("list-type") == "2":
		var out struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []struct {
				Key  string
				Size int
			}
		}
		var keys []string
		for k := range m.objs {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.Contents = append(out.Contents, struct {
				Key  string
				Size int
			}{k, len(m.objs[k])})
		}
		xml.NewEncoder(w).Encode(out)
	case r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		m.objs[key] = b
		sum := md5.Sum(b)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	case r.Method == "GET" || r.Method == "HEAD":
		b, ok := m.objs[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		sum := md5.Sum(b)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.Write(b)
	case r.Method == "DELETE":
		delete(m.objs, key)
		w.WriteHeader(204)
	}
}

func (m *miniS3) keys(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// writeArchiveEnv points the config at a bucket. HOME moves too, because the
// archiver's lock files live under it.
func writeArchiveEnv(t *testing.T, c *Config, url string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	creds := filepath.Join(c.StateDir, "..", "s3-creds")
	if err := os.WriteFile(creds, []byte("key_id=k\nsecret_key=s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("# the timer's settings\nCOMEBYE_ARCHIVE_S3=%s/bucket\nCOMEBYE_ARCHIVE_S3_CREDENTIALS=%q\nCOMEBYE_ARCHIVE_S3_PLAINTEXT=homelab\nCOMEBYE_ARCHIVE_ENCRYPTION=none\nCOMEBYE_HOST_NAME=laharl\n", url, creds)
	if err := os.WriteFile(c.ArchiveEnv, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- tars from a worker ----

type tarEntry struct {
	name, body, link string
	typ              byte
	mod              time.Time
}

func tgz(t *testing.T, es ...tarEntry) string {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range es {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mod := e.mod
		if mod.IsZero() {
			mod = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o777, Size: int64(len(e.body)), Linkname: e.link, ModTime: mod}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return b.String()
}

const piStream = ".pi/agent/sessions/--data-worktrees-x--/2026-11-06T10-00-00-000Z_abc.jsonl"

func sessionBody() string {
	return `{"type":"session","version":3,"id":"abc","timestamp":"2026-11-06T10:00:00Z","cwd":"/data/worktrees/x"}` + "\n"
}

func TestExtractSessionsKeepsAWorkerInsideItsDirectory(t *testing.T) {
	for name, tc := range map[string]struct {
		es   []tarEntry
		want string
	}{
		"parent":       {[]tarEntry{{name: ".pi/agent/sessions/../../../../evil"}}, "refusing"},
		"inner parent": {[]tarEntry{{name: ".claude/projects/a/../../../evil"}}, "refusing"},
		"absolute":     {[]tarEntry{{name: "/etc/evil"}}, "refusing"},
		"other file":   {[]tarEntry{{name: ".ssh/authorized_keys"}}, "not under a session directory"},
		"sibling":      {[]tarEntry{{name: ".claude/projects-evil/x"}}, "not under a session directory"},
		"device":       {[]tarEntry{{name: ".pi/agent/sessions/x/dev", typ: tar.TypeChar}}, "refusing"},
		"fifo":         {[]tarEntry{{name: ".pi/agent/sessions/x/f", typ: tar.TypeFifo}}, "refusing"},
		"duplicate":    {[]tarEntry{{name: piStream, body: "a"}, {name: piStream, body: "b"}}, "exists"},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "in")
			os.Mkdir(dir, 0o700)
			_, err := extractSessions(strings.NewReader(tgz(t, tc.es...)), dir, 1<<20, 100)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
				t.Error("a file escaped")
			}
		})
	}
}

func TestExtractSessionsNeverMakesOrFollowsALink(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "in")
	os.Mkdir(dir, 0o700)
	target := filepath.Join(parent, "target")
	os.Mkdir(target, 0o700)
	n, err := extractSessions(strings.NewReader(tgz(t,
		tarEntry{name: ".pi/agent/sessions/link", typ: tar.TypeSymlink, link: target},
		tarEntry{name: ".pi/agent/sessions/link/written", body: "x"},
		tarEntry{name: ".pi/agent/sessions/hard", typ: tar.TypeLink, link: "/etc/passwd"},
	)), dir, 1<<20, 100)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "pi", "link")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("link: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(target, "written")); err == nil {
		t.Error("wrote through a link")
	}
	if _, err := os.Lstat(filepath.Join(dir, "pi", "hard")); err == nil {
		t.Error("made a hard link")
	}
}

func TestExtractSessionsCaps(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 1000)
	_, err := extractSessions(strings.NewReader(tgz(t, tarEntry{name: ".pi/agent/sessions/a/1", body: big}, tarEntry{name: ".pi/agent/sessions/a/2", body: big})), dir, 1500, 100)
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("byte cap: %v", err)
	}
	dir = t.TempDir()
	_, err = extractSessions(strings.NewReader(tgz(t, tarEntry{name: ".pi/agent/sessions/a/1"}, tarEntry{name: ".pi/agent/sessions/a/2"})), dir, 1500, 1)
	if err == nil || !strings.Contains(err.Error(), "files") {
		t.Errorf("file cap: %v", err)
	}
	if _, err := extractSessions(strings.NewReader("not a tar"), t.TempDir(), 10, 10); err == nil {
		t.Error("garbage accepted")
	}
	if n, err := extractSessions(strings.NewReader(tgz(t)), t.TempDir(), 10, 10); err != nil || n != 0 {
		t.Errorf("an empty worker: %d %v", n, err)
	}
}

func TestExtractSessionsKeepsMtimeAndIsPrivate(t *testing.T) {
	dir := t.TempDir()
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := extractSessions(strings.NewReader(tgz(t, tarEntry{name: ".claude/projects/-x/s.jsonl", body: "a\n", mod: mod})), dir, 100, 10); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "claude", "-x", "s.jsonl")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mod) || fi.Mode().Perm() != 0o600 {
		t.Errorf("%v %o", fi.ModTime(), fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(p))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir %o", di.Mode().Perm())
	}
}

func TestLoadArchiveEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	os.WriteFile(p, []byte("# comment\nCOMEBYE_ARCHIVE_S3=\"https://x/b\"\n COMEBYE_ARCHIVE_ENCRYPTION = none\nCOMEBYE_HOST_NAME=laharl\nOTHER=1\nnot a line\n"), 0o600)
	c, err := loadArchiveEnv(p)
	if err != nil || c.S3 != "https://x/b" || c.Encryption != "none" {
		t.Fatalf("%v %+v", err, c)
	}
	if c.Host != "" {
		t.Errorf("the timer's host name must not reach a worker's upload: %q", c.Host)
	}
}

// ---- streaming ----

type streamFunc struct {
	Runner
	f func(w io.Writer) error
}

func (s streamFunc) RunStream(_ context.Context, _ string, _ []string, w io.Writer) error {
	return s.f(w)
}

func TestPullStopsWhenExtractionFails(t *testing.T) {
	c := testCfg(t)
	e := &env{Cfg: c}
	// Far more output than a pipe holds: a pull that ignored the failure would hang here.
	e.Run = streamFunc{f: func(w io.Writer) error {
		for i := 0; i < 1000; i++ {
			if _, err := w.Write(make([]byte, 1<<16)); err != nil {
				return err
			}
		}
		return nil
	}}
	if _, err := e.pull(context.Background(), "w1", t.TempDir()); err == nil || !strings.Contains(err.Error(), "reading the worker") {
		t.Fatalf("err = %v", err)
	}
}

func TestPullReportsAFailedSSH(t *testing.T) {
	e := &env{Cfg: testCfg(t)}
	boom := errors.New("ssh: connection reset")
	e.Run = streamFunc{f: func(w io.Writer) error {
		w.Write([]byte(tgz(t, tarEntry{name: piStream, body: "x"})))
		return boom
	}}
	if _, err := e.pull(context.Background(), "w1", t.TempDir()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestOSRunnerStreams(t *testing.T) {
	var b bytes.Buffer
	if err := runStream(context.Background(), osRunner{}, "printf", []string{"hello"}, &b); err != nil || b.String() != "hello" {
		t.Fatalf("%v %q", err, b.String())
	}
	if err := runStream(context.Background(), osRunner{}, "false", nil, &b); err == nil {
		t.Fatal("a failure was lost")
	}
}

// ---- destroy and archive ----

// archWorld is an AWS worker with an ssh that serves a tar of its sessions and
// records the order of what happens on the VM.
type archWorld struct {
	*awsFake
	s3      *miniS3
	r       *fakeRunner
	out     *strings.Builder
	log     []string
	sessTar func() string
	sshErr  func(remote string) error
	world   *fakeWorld
}

func newArchWorld(t *testing.T, input string) (*archWorld, *env) {
	f := fullAWS()
	e, r, out := awsDestroyEnv(t, f, input)
	a := &archWorld{awsFake: f, r: r, out: out}
	a.world = e.HTTP.(*fakeWorld)
	a.sessTar = func() string { return tgz(t, tarEntry{name: piStream, body: sessionBody()}) }
	inner := r.handle
	r.handle = func(c call, fr *fakeRunner) (string, string, error) {
		if c.name == "ssh" {
			remote := c.args[len(c.args)-1]
			if a.sshErr != nil {
				if err := a.sshErr(remote); err != nil {
					return "", "", err
				}
			}
			switch {
			case remote == pullScript:
				a.log = append(a.log, "pull")
				return a.sessTar(), "", nil
			case strings.Contains(remote, "--user stop"):
				a.log = append(a.log, "stop agents")
			case strings.Contains(remote, "--user start"):
				a.log = append(a.log, "start agents")
			case strings.Contains(remote, "tailscale logout"):
				a.log = append(a.log, "logout")
			}
		}
		return inner(c, fr)
	}
	s3, srv := startS3(t)
	a.s3 = s3
	writeArchiveEnv(t, e.Cfg, srv.URL)
	return a, e
}

// nothingChanged: the VM, its node, its peer entry and its keys are all there.
func (a *archWorld) nothingChanged(t *testing.T) {
	t.Helper()
	if a.did("terminate-instances") || a.did("cancel-spot-instance-requests") {
		t.Errorf("the VM was touched: %v", a.ops)
	}
	if len(a.world.peerRemoved) > 0 || len(a.world.deleted) > 0 {
		t.Errorf("hub or tailnet touched: %v %v", a.world.peerRemoved, a.world.deleted)
	}
	if a.r.saw("tailscale logout") {
		t.Error("left the tailnet")
	}
}

func TestDestroySavesSessionsFirst(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	if err := e.destroy(context.Background(), "w1", destroyOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, a.out)
	}
	if got := strings.Join(a.log, ","); !strings.HasPrefix(got, "pull,stop agents,pull,") || !strings.Contains(got, "logout") {
		t.Errorf("order: %s", got)
	}
	if !a.did("terminate-instances") {
		t.Error("the VM was not removed")
	}
	host := "w1.i-1"
	segs := a.s3.keys("v1/" + host + "/pi/")
	if len(segs) != 1 || !strings.Contains(segs[0], "--data-worktrees-x--") {
		t.Fatalf("segments: %v (all: %v)", segs, a.s3.keys(""))
	}
	var cl struct{ Kind, Worker, Provider string }
	var inst struct {
		ID string `json:"instance_id"`
	}
	raw := a.s3.objs["hosts/"+host+".json"]
	if err := json.Unmarshal(raw, &cl); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &inst)
	if cl.Kind != "worker" || cl.Worker != "w1" || cl.Provider != "aws" || inst.ID != "i-1" {
		t.Errorf("claim: %s", raw)
	}
	if len(a.s3.keys("v1/laharl/")) != 0 || len(a.s3.keys("hosts/laharl")) != 0 {
		t.Error("wrote under the timer's own host name")
	}
	if ents, _ := os.ReadDir(filepath.Join(e.Cfg.StateDir, "pull")); len(ents) != 0 {
		t.Errorf("the pulled plaintext was left on disk: %v", ents)
	}
}

func TestDestroyFinalPullTakesTheLastLine(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	calls := 0
	a.sessTar = func() string {
		calls++
		body := sessionBody()
		if calls > 1 {
			body += `{"unterminated`
		}
		return tgz(t, tarEntry{name: piStream, body: body, mod: time.Now()})
	}
	if err := e.destroy(context.Background(), "w1", destroyOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, a.out)
	}
	if segs := a.s3.keys("v1/w1.i-1/pi/"); len(segs) != 2 {
		t.Fatalf("want the first segment and the tail: %v", segs)
	}
}

func TestDestroyRefusesAnUnreachableBucket(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	writeArchiveEnv(t, e.Cfg, url)
	err := e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "--no-archive") {
		t.Fatalf("err = %v", err)
	}
	a.nothingChanged(t)
	if len(a.log) != 0 {
		t.Errorf("touched the VM: %v", a.log)
	}
}

func TestDestroyRefusesWithoutArchiveSettings(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	os.Remove(e.Cfg.ArchiveEnv)
	err := e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "archive_env") {
		t.Fatalf("err = %v", err)
	}
	a.nothingChanged(t)
}

func TestDestroyRefusesAStoppedVMBeforeAsking(t *testing.T) {
	a, e := newArchWorld(t, "")
	a.inst = "stopped"
	err := e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "worker start w1") || !strings.Contains(err.Error(), "--no-archive") {
		t.Fatalf("err = %v", err)
	}
	a.nothingChanged(t)
	if strings.Contains(a.out.String(), "Type the worker's name") {
		t.Error("it asked for confirmation first")
	}
}

func TestDestroyNoArchiveOverrides(t *testing.T) {
	for _, state := range []string{"running", "stopped"} {
		t.Run(state, func(t *testing.T) {
			a, e := newArchWorld(t, "w1\n")
			a.inst = state
			os.Remove(e.Cfg.ArchiveEnv)
			if err := e.destroy(context.Background(), "w1", destroyOpts{NoArchive: true}); err != nil {
				t.Fatalf("%v\n%s", err, a.out)
			}
			if !a.did("terminate-instances") {
				t.Error("the VM was not removed")
			}
			if strings.Contains(strings.Join(a.log, ","), "pull") || len(a.s3.keys("")) != 0 {
				t.Errorf("--no-archive still pulled: %v", a.log)
			}
			if !strings.Contains(a.out.String(), "NOT being saved") {
				t.Errorf("no warning:\n%s", a.out)
			}
		})
	}
}

func TestDestroyContinuesWhenTheVMIsGone(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	a.inst = ""
	os.Remove(e.Cfg.ArchiveEnv) // nothing to save, so the archive is not needed
	if err := e.destroy(context.Background(), "w1", destroyOpts{}); err != nil {
		t.Fatalf("%v\n%s", err, a.out)
	}
	if !strings.Contains(a.out.String(), "no sessions to save") {
		t.Errorf("%s", a.out)
	}
	if len(a.world.peerRemoved) != 1 {
		t.Errorf("the peer entry stayed: %v", a.world.peerRemoved)
	}
}

func TestDestroyFailedFirstPullChangesNothing(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	a.sessTar = func() string { return "garbage" }
	err := e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	a.nothingChanged(t)
	if strings.Contains(strings.Join(a.log, ","), "stop agents") {
		t.Errorf("stopped the agents: %v", a.log)
	}
}

func TestDestroyFailedFinalPullRestartsTheAgents(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	calls := 0
	good := a.sessTar
	a.sessTar = func() string {
		if calls++; calls > 1 {
			return "garbage"
		}
		return good()
	}
	err := e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "left running") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "STOPPED") {
		t.Errorf("said the agents are stopped: %v", err)
	}
	a.nothingChanged(t)
	if got := strings.Join(a.log, ","); got != "pull,stop agents,pull,start agents" {
		t.Errorf("order: %s", got)
	}
}

func TestDestroySaysPlainlyWhenTheRestartFails(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	calls := 0
	good := a.sessTar
	a.sessTar = func() string {
		if calls++; calls > 1 {
			return "garbage"
		}
		return good()
	}
	a.sshErr = func(remote string) error {
		if strings.Contains(remote, "--user start") {
			return errors.New("connection lost")
		}
		return nil
	}
	err := e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "agents are STOPPED") || !strings.Contains(err.Error(), "left running") {
		t.Fatalf("err = %v", err)
	}
	a.nothingChanged(t)
}

func TestDestroyRefusesAFileThatDiffersFromTheArchive(t *testing.T) {
	a, e := newArchWorld(t, "w1\n")
	// What is stored and what the worker now has disagree, as after a rewrite.
	calls := 0
	a.sessTar = func() string {
		calls++
		body := sessionBody()
		if calls > 1 {
			body = strings.ToUpper(body)
		}
		return tgz(t, tarEntry{name: piStream, body: body})
	}
	err := e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("err = %v", err)
	}
	a.nothingChanged(t)
}

func TestWorkerArchive(t *testing.T) {
	a, e := newArchWorld(t, "")
	if err := e.archiveOne(context.Background(), "w1"); err != nil {
		t.Fatalf("%v\n%s", err, a.out)
	}
	if len(a.s3.keys("v1/w1.i-1/pi/")) != 1 {
		t.Errorf("%v", a.s3.keys(""))
	}
	if strings.Contains(strings.Join(a.log, ","), "stop agents") {
		t.Errorf("a plain archive stopped the agents: %v", a.log)
	}
	// Safe to repeat.
	if err := e.archiveOne(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if len(a.s3.keys("v1/w1.i-1/pi/")) != 1 {
		t.Errorf("%v", a.s3.keys(""))
	}
	if !strings.Contains(a.out.String(), "w1.i-1") {
		t.Errorf("does not name the archive host:\n%s", a.out)
	}

	a.inst = "stopped"
	if err := e.archiveOne(context.Background(), "w1"); err == nil || !strings.Contains(err.Error(), "worker start") {
		t.Errorf("stopped: %v", err)
	}
	a.foreign, a.inst = true, "running"
	if err := e.archiveOne(context.Background(), "w1"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("foreign: %v", err)
	}
}

// listProv lists several workers, and answers Describe through the real provider.
type listProv struct {
	Provider
	list []Inst
}

func (l listProv) Instances(context.Context) ([]Inst, error) { return l.list, nil }

func TestWorkerArchiveAllSkipsStoppedAndNeverStarts(t *testing.T) {
	a, e := newArchWorld(t, "")
	e.P = listProv{Provider: newProvider(e.Run, e.Cfg), list: []Inst{{Name: "w1", State: stateRunning}, {Name: "off", State: stateStopped}}}
	if err := e.archiveAll(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, a.out)
	}
	if !strings.Contains(a.out.String(), "off: STOPPED, skipped") {
		t.Errorf("%s", a.out)
	}
	if a.did("start-instances") {
		t.Error("started a VM")
	}
	if len(a.s3.keys("v1/w1.i-1/pi/")) != 1 {
		t.Errorf("%v", a.s3.keys(""))
	}
	// A failure does not stop the rest, and is reported.
	a.sessTar = func() string { return "garbage" }
	e.P = listProv{Provider: newProvider(e.Run, e.Cfg), list: []Inst{{Name: "w1", State: stateRunning}}}
	if err := e.archiveAll(context.Background()); err == nil || !strings.Contains(err.Error(), "w1") {
		t.Errorf("err = %v", err)
	}
}

func TestWorkerArchiveFlags(t *testing.T) {
	for _, args := range [][]string{{"archive"}, {"archive", "w1", "--all"}, {"status", "--all"}, {"stop", "w1", "--no-archive"}} {
		var out, errb bytes.Buffer
		if st := run(args, &env{Out: &out, Err: &errb, In: strings.NewReader("")}); st != 2 {
			t.Errorf("%v: exit %d: %s", args, st, errb.String())
		}
	}
}

func TestInstanceCreatedSurvivesARestart(t *testing.T) {
	// LaunchTime moves on every start, so it must not be what is read.
	i := awsInstance{RootDevice: "/dev/xvda"}
	raw := `{"RootDeviceName":"/dev/xvda","LaunchTime":"2026-11-06T12:00:00Z","BlockDeviceMappings":[
		{"DeviceName":"/dev/sdf","Ebs":{"AttachTime":"2026-11-06T11:00:00Z"}},
		{"DeviceName":"/dev/xvda","Ebs":{"AttachTime":"2026-11-01T09:00:00Z"}}]}`
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatal(err)
	}
	if got := i.created(); !got.Equal(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("created = %v", got)
	}
	if (&awsInstance{}).created() != (time.Time{}) {
		t.Error("no devices should be unknown")
	}
}

func TestGCEInstanceIDIsTheCloudsNotTheName(t *testing.T) {
	c := testCfg(t)
	r := &fakeRunner{state: map[string]bool{}}
	r.handle = func(cl call, _ *fakeRunner) (string, string, error) {
		return `{"name":"w1","id":"1234567890123456789","creationTimestamp":"2026-11-01T09:00:00.000-07:00","status":"TERMINATED","labels":{"managed-by":"comebye-worker"}}`, "", nil
	}
	i, err := newProvider(r, c).Describe(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if i.ID != "1234567890123456789" || i.State != stateStopped {
		t.Errorf("%+v", i)
	}
	if !i.Created.Equal(time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("created = %v", i.Created)
	}
}
