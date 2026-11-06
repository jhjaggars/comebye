package archive

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a small S3 server that behaves as Garage v2.5.0 does on PUT: it
// ignores If-None-Match. It checks the payload hash and Content-MD5 it is sent.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests []string
	honour   bool // honour If-None-Match, unlike Garage
	failNext int  // answer this many requests with a 500
	status   int  // answer everything with this
	badETag  bool
	partETag bool
	noToken  bool // truncated listing with no continuation token
	corrupt  bool // GET returns different bytes from the ETag
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=GKtest/") {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
		return
	}
	if f.status != 0 {
		http.Error(w, "<Error><Code>Denied</Code><Message>no</Message></Error>", f.status)
		return
	}
	if f.failNext > 0 {
		f.failNext--
		http.Error(w, "<Error><Code>InternalError</Code></Error>", http.StatusInternalServerError)
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/b"), "/")
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && key == "":
		f.list(w, r)
	case r.Method == http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.etag(w, data)
		if f.corrupt {
			data = append([]byte("x"), data...)
		}
		w.Write(data)
	case r.Method == http.MethodPut:
		sum := sha256.Sum256(body)
		if r.Header.Get("x-amz-content-sha256") != hex.EncodeToString(sum[:]) {
			http.Error(w, "<Error><Code>XAmzContentSHA256Mismatch</Code></Error>", http.StatusBadRequest)
			return
		}
		m := md5.Sum(body)
		if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(m[:]) {
			http.Error(w, "<Error><Code>BadDigest</Code></Error>", http.StatusBadRequest)
			return
		}
		if _, ok := f.objects[key]; ok && f.honour && r.Header.Get("If-None-Match") == "*" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		f.objects[key] = body
		f.etag(w, body)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeS3) etag(w http.ResponseWriter, data []byte) {
	sum := md5.Sum(data)
	e := hex.EncodeToString(sum[:])
	switch {
	case f.badETag:
		e = strings.Repeat("0", 32)
	case f.partETag:
		e += "-2"
	}
	w.Header().Set("ETag", `"`+e+`"`)
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, q.Get("prefix")) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start, _ := strconv.Atoi(q.Get("continuation-token"))
	page := 1000
	if n, err := strconv.Atoi(q.Get("max-keys")); err == nil {
		page = n
	}
	type item struct {
		Key  string
		Size int
	}
	var out struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Contents              []item
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
	}
	end := min(start+page, len(keys))
	for _, k := range keys[start:end] {
		out.Contents = append(out.Contents, item{k, len(f.objects[k])})
	}
	if end < len(keys) {
		out.IsTruncated = true
		if !f.noToken {
			out.NextContinuationToken = strconv.Itoa(end)
		}
	}
	xml.NewEncoder(w).Encode(out)
}

func fakeBackend(t *testing.T, f *fakeS3) *S3Backend {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b, err := NewS3Backend(srv.URL+"/b", "GKtest", "the-secret-value", "")
	if err != nil {
		t.Fatal(err)
	}
	b.sleep = func(time.Duration) {}
	return b
}

// realGarage returns a backend for a real store when the environment names
// one: COMEBYE_TEST_S3_URL and COMEBYE_TEST_S3_CREDENTIALS. See docs/archive.md.
func realGarage(t *testing.T) *S3Backend {
	t.Helper()
	u, c := os.Getenv("COMEBYE_TEST_S3_URL"), os.Getenv("COMEBYE_TEST_S3_CREDENTIALS")
	if u == "" || c == "" {
		t.Skip("no real store: set COMEBYE_TEST_S3_URL and COMEBYE_TEST_S3_CREDENTIALS")
	}
	id, secret, err := LoadS3Credentials(c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewS3Backend(u, id, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testPrefix(t *testing.T) string {
	t.Helper()
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		t.Fatal(err)
	}
	return "contract-test/" + hex.EncodeToString(r[:]) + "/"
}

// backendContract is what the rest of the package relies on from any backend.
func backendContract(t *testing.T, b Backend) {
	pre := testPrefix(t)
	t.Cleanup(func() {
		keys, _ := b.List(pre)
		for _, k := range keys {
			b.Delete(k)
		}
	})

	if _, err := b.Get(pre + "missing"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Get of a missing key: %v", err)
	}
	if keys, err := b.List(pre); err != nil || len(keys) != 0 {
		t.Errorf("List of an empty prefix: %v, %v", keys, err)
	}

	key := pre + "a/b.seg"
	if err := b.Put(key, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(key, []byte("second")); !errors.Is(err, ErrExists) {
		t.Errorf("second Put: %v", err)
	}
	if got, err := b.Get(key); err != nil || string(got) != "first" {
		t.Errorf("after a refused Put: %q, %v", got, err)
	}

	// Real keys hold slugs and tool-result names; they must survive signing.
	odd := []string{
		pre + "claude/-home-j-code-x/a b+c=d@e,f;g/ü~1.seg",
		pre + "claude/slug/0000000000000000-0000000000000010.seg",
		pre + "z&y'z(1)!$*.jsonl",
	}
	for _, k := range odd {
		if err := b.Put(k, []byte(k)); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
		if got, err := b.Get(k); err != nil || string(got) != k {
			t.Errorf("Get %q: %q, %v", k, got, err)
		}
	}
	keys, err := b.List(pre)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{key}, odd...)
	sort.Strings(want)
	if strings.Join(keys, "\n") != strings.Join(want, "\n") {
		t.Errorf("List:\n%q\nwant\n%q", keys, want)
	}
	if !sort.StringsAreSorted(keys) {
		t.Errorf("List is not sorted: %q", keys)
	}
	if sub, err := b.List(pre + "a/"); err != nil || len(sub) != 1 || sub[0] != key {
		t.Errorf("List of a sub-prefix: %q, %v", sub, err)
	}

	if err := b.Delete(key); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(key); err != nil {
		t.Errorf("Delete of a missing key: %v", err)
	}
	if _, err := b.Get(key); !errors.Is(err, ErrNotExist) {
		t.Errorf("Get after Delete: %v", err)
	}
	if err := b.Put(pre+"../up", []byte("x")); err == nil {
		t.Error("a key with .. was accepted")
	}
}

func TestBackendContractFS(t *testing.T) {
	b, err := NewFSBackend(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	backendContract(t, b)
}

func TestBackendContractFakeS3(t *testing.T) {
	backendContract(t, fakeBackend(t, newFakeS3()))
}

func TestBackendContractRealStore(t *testing.T) {
	backendContract(t, realGarage(t))
}

// Put's create-only rests on HEAD because Garage v2.5.0 ignores If-None-Match
// on PUT. If this starts to fail after a Garage upgrade, Put is still correct,
// and the comment on S3Backend and the design's reasoning should be updated.
func TestRealStoreIgnoresConditionalPut(t *testing.T) {
	b := realGarage(t)
	key := testPrefix(t) + "conditional"
	t.Cleanup(func() { b.Delete(key) })
	put := func(body string) int {
		sum := md5.Sum([]byte(body))
		r, err := b.do(http.MethodPut, key, nil, []byte(body), map[string]string{
			"content-md5":   base64.StdEncoding.EncodeToString(sum[:]),
			"if-none-match": "*",
		})
		if err != nil {
			t.Fatal(err)
		}
		return r.status
	}
	if s := put("one"); s != http.StatusOK {
		t.Fatalf("first PUT: %d", s)
	}
	if s := put("two"); s != http.StatusOK {
		t.Fatalf("Garage now refuses a conditional PUT (HTTP %d). Put is still correct; update S3Backend's comment", s)
	}
	if got, _ := b.Get(key); string(got) != "two" {
		t.Fatalf("after the second PUT: %q", got)
	}
}

func TestRealStoreListPages(t *testing.T) {
	testListPages(t, realGarage(t))
}

func TestFakeS3ListPages(t *testing.T) {
	testListPages(t, fakeBackend(t, newFakeS3()))
}

func testListPages(t *testing.T, b *S3Backend) {
	pre := testPrefix(t)
	var want []string
	for i := range 7 {
		k := fmt.Sprintf("%s%02d", pre, i)
		want = append(want, k)
		if err := b.Put(k, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, k := range want {
			b.Delete(k)
		}
	})
	b.pageSize = 2
	got, err := b.List(pre)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paged listing: %q", got)
	}
	if n, err := b.Size(want[3]); err != nil || n != 1 {
		t.Fatalf("Size: %d, %v", n, err)
	}
}

func TestS3PutHonoursAPreconditionFailed(t *testing.T) {
	f := newFakeS3()
	f.honour = true
	b := fakeBackend(t, f)
	f.objects["k"] = []byte("there") // appears between our HEAD and PUT
	head := true
	real := b.client.Transport
	b.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodHead && head {
			head = false
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
		}
		return real.RoundTrip(r)
	})
	if err := b.Put("k", []byte("mine")); !errors.Is(err, ErrExists) {
		t.Fatalf("Put: %v", err)
	}
	if string(f.objects["k"]) != "there" {
		t.Fatal("the object was replaced")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestS3Retries(t *testing.T) {
	f := newFakeS3()
	b := fakeBackend(t, f)
	f.failNext = s3Attempts - 1
	if err := b.Put("k", []byte("v")); err != nil {
		t.Fatalf("a blip was not retried: %v", err)
	}
	f.failNext = s3Attempts
	if _, err := b.Get("k"); err == nil || errors.Is(err, ErrNotExist) {
		t.Fatalf("a lasting failure must be an error, not a missing object: %v", err)
	}
	f.failNext = 0
	f.status = http.StatusForbidden
	before := len(f.requests)
	_, err := b.Get("k")
	var se *s3Error
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.Code != "Denied" {
		t.Fatalf("Get: %v", err)
	}
	if got := len(f.requests) - before; got != 1 {
		t.Fatalf("a 403 was retried: %d requests", got)
	}
	if strings.Contains(err.Error(), b.Secret) {
		t.Fatal("the error holds the secret")
	}
	if err := b.Put("new", []byte("v")); err == nil {
		t.Fatal("Put through a refusal succeeded")
	}
	if _, err := b.List(""); err == nil {
		t.Fatal("List through a refusal succeeded")
	}
	if err := b.Delete("k"); err == nil {
		t.Fatal("Delete through a refusal succeeded")
	}
}

func TestS3ChecksIntegrity(t *testing.T) {
	f := newFakeS3()
	b := fakeBackend(t, f)
	f.badETag = true
	if err := b.Put("a", []byte("v")); err == nil {
		t.Fatal("a wrong ETag on PUT went unnoticed")
	}
	f.badETag = false
	f.objects["c"] = []byte("v")
	f.corrupt = true
	if _, err := b.Get("c"); err == nil {
		t.Fatal("a body that does not match its MD5 went unnoticed")
	}
	f.corrupt = false
	f.partETag = true // uploaded in parts by aws s3 sync: not an MD5, so not checked
	if got, err := b.Get("c"); err != nil || string(got) != "v" {
		t.Fatalf("a multipart object: %q, %v", got, err)
	}
}

func TestS3TruncatedListWithNoTokenIsAnError(t *testing.T) {
	f := newFakeS3()
	b := fakeBackend(t, f)
	b.pageSize = 1
	f.objects["a"], f.objects["b"] = []byte("1"), []byte("2")
	f.noToken = true
	if keys, err := b.List(""); err == nil {
		t.Fatalf("a short listing was accepted: %q", keys)
	}
}

func TestNewS3BackendURLs(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://comebye-archive.tail4a1fd.ts.net/comebye-archive", true},
		{"https://h/b/", true},
		{"http://127.0.0.1:39000/b", true},
		{"http://localhost:39000/b", true},
		{"http://[::1]:39000/b", true},
		{"http://garage.jaggars.cc/b", false},
		{"https://h", false},
		{"https://h/", false},
		{"https://h/b/c", false},
		{"https://k:s@h/b", false},
		{"https://h/b?x=1", false},
		{"ftp://h/b", false},
		{"h/b", false},
		{"", false},
	} {
		_, err := NewS3Backend(tc.url, "id", "s", "")
		if (err == nil) != tc.ok {
			t.Errorf("%q: %v", tc.url, err)
		}
	}
	if _, err := NewS3Backend("https://h/b", "", "s", ""); err == nil {
		t.Error("no key id accepted")
	}
}

func writeCreds(t *testing.T, mode os.FileMode, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(p, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadS3Credentials(t *testing.T) {
	id, sec, err := LoadS3Credentials(writeCreds(t, 0o600, "# comment\nkey_id=GKabc\n\nsecret_key = s3cret==\n"))
	if err != nil || id != "GKabc" || sec != "s3cret==" {
		t.Fatalf("%q %q %v", id, sec, err)
	}
	if _, _, err := LoadS3Credentials(writeCreds(t, 0o640, "key_id=a\nsecret_key=b\n")); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a group-readable file: %v", err)
	}
	if _, _, err := LoadS3Credentials(writeCreds(t, 0o600, "key_id=a\n")); err == nil {
		t.Fatal("no secret accepted")
	}
	_, _, err = LoadS3Credentials(writeCreds(t, 0o600, "not-a-pair-hunter2\n"))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("a bad line must fail without quoting the file: %v", err)
	}
	if _, _, err := LoadS3Credentials(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("a missing file accepted")
	}
}

func s3Config(t *testing.T, url string) Config {
	t.Helper()
	return Config{
		Encryption: ModeNone, Host: "h1", S3: url, S3Plaintext: "homelab",
		S3Credentials: writeCreds(t, 0o600, "key_id=GKtest\nsecret_key=s\n"),
	}
}

func TestConfigS3Rules(t *testing.T) {
	f := newFakeS3()
	srv := httptest.NewServer(f)
	defer srv.Close()
	url := srv.URL + "/b"

	if _, err := (Config{}).Open(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("nothing configured: %v", err)
	}
	c := s3Config(t, url)
	c.Dir = t.TempDir()
	if _, err := c.Open(); err == nil {
		t.Error("both a directory and a bucket accepted")
	}
	c = s3Config(t, url)
	c.S3Plaintext = ""
	if _, err := c.Open(); err == nil || !strings.Contains(err.Error(), "COMEBYE_ARCHIVE_S3_PLAINTEXT") {
		t.Errorf("plaintext to a bucket without the acknowledgement: %v", err)
	}
	if len(f.objects) != 0 {
		t.Error("the plaintext gate came after touching the bucket")
	}
	c = s3Config(t, url)
	c.S3Plaintext = "yes"
	if _, err := c.Open(); err == nil {
		t.Error("any value but homelab must not pass")
	}
	c = s3Config(t, url)
	c.S3Credentials = ""
	if _, err := c.Open(); err == nil {
		t.Error("no credentials accepted")
	}
	c = s3Config(t, url)
	a, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	if string(f.objects[modeKey]) != "none\n" {
		t.Errorf("mode marker: %q", f.objects[modeKey])
	}
	if a.Dir() != "" {
		t.Errorf("Dir: %q", a.Dir())
	}
	// An age archiver finds this bucket already plaintext.
	age := s3Config(t, url)
	age.Encryption, age.S3Plaintext = ModeAge, ""
	_, rcpt, _ := GenerateIdentity()
	age.Recipient = rcpt
	if _, err := age.Open(); err == nil {
		t.Error("an age writer accepted a plaintext bucket")
	}
}

func s3Rig(t *testing.T) (*rig, *fakeS3) {
	t.Helper()
	r := newPlainRig(t)
	f := newFakeS3()
	r.a.B = fakeBackend(t, f)
	return r, f
}

// The whole archive path over S3: keys match the filesystem layout, and
// deletes stick.
func TestArchiveOverS3(t *testing.T) {
	r, f := s3Rig(t)
	mainA, _, _ := r.fixture()
	res, err := r.a.Run(r.sources())
	if err != nil || res.Segments != 4 || len(res.Errors) > 0 {
		t.Fatalf("run: %+v, %v", res, err)
	}
	if res, err := r.a.Run(r.sources()); err != nil || res.Segments != 0 {
		t.Fatalf("rerun wrote again: %+v, %v", res, err)
	}
	want, _ := os.ReadFile(mainA)
	if got := restoreString(t, r, "claude", "-proj/"+sidA+".jsonl"); got != string(want) {
		t.Fatalf("restored %q", got)
	}
	vr, err := r.a.Verify(r.op, r.sources())
	if err != nil || vr.Mismatched != 0 || vr.Matched != 4 {
		t.Fatalf("verify: %+v, %v", vr, err)
	}
	if _, err := r.a.DeleteSession("claude", "-proj", sidA); err != nil {
		t.Fatal(err)
	}
	if res, err := r.a.Run(r.sources()); err != nil || res.Segments != 0 || res.Tombstoned != 3 {
		t.Fatalf("after delete: %+v, %v", res, err)
	}
	for k := range f.objects {
		if strings.Contains(k, sidA) && !strings.HasPrefix(k, "tomb/") {
			t.Fatalf("left %s", k)
		}
	}
}

func TestIndexOverS3(t *testing.T) {
	hasSqlite(t)
	r, _ := s3Rig(t)
	r.fixture()
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	ix := &Index{Path: filepath.Join(r.dir, "index.db")}
	st, err := ix.Sync(r.a, r.op, nil, SyncOptions{})
	if err != nil || st.Indexed != 2 || len(st.Errors) > 0 {
		t.Fatalf("sync: %+v, %v", st, err)
	}
}

func TestHostClaim(t *testing.T) {
	r, f := s3Rig(t)
	if err := r.a.claimHost(false); err != nil {
		t.Fatal(err)
	}
	raw := f.objects[hostClaimKey("laharl")]
	if len(raw) == 0 {
		t.Fatal("no claim written")
	}
	if strings.Contains(string(raw), "machine-id") {
		t.Fatalf("claim: %s", raw)
	}
	if err := r.a.claimHost(false); err != nil {
		t.Fatalf("the same machine again: %v", err)
	}
	f.objects[hostClaimKey("laharl")] = []byte(`{"machine":"another","hostname":"elsewhere","first_seen":"2026-01-01T00:00:00Z"}`)
	err := r.a.claimHost(false)
	if err == nil || !strings.Contains(err.Error(), "elsewhere") || !strings.Contains(err.Error(), "--adopt-host") {
		t.Fatalf("a second machine: %v", err)
	}
	if err := r.a.claimHost(true); err != nil {
		t.Fatalf("adopt: %v", err)
	}
}

// A second machine using the host name writes nothing.
func TestRunRefusedForAnotherMachine(t *testing.T) {
	f := newFakeS3()
	srv := httptest.NewServer(f)
	defer srv.Close()
	f.objects[hostClaimKey("h1")] = []byte(`{"machine":"another","hostname":"elsewhere"}`)
	c := s3Config(t, srv.URL+"/b")
	a, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	a.LockDir = t.TempDir()
	d := t.TempDir()
	r := &rig{t: t}
	r.write(filepath.Join(d, "-p", sidA+".jsonl"), "x\n", false)
	if _, err := a.Run([]Source{{Kind: "claude", Root: d}}); err == nil {
		t.Fatal("run went ahead")
	}
	for k := range f.objects {
		if strings.HasPrefix(k, "v1/") {
			t.Fatalf("wrote %s", k)
		}
	}
}

// A Garage outage must not stop the server from starting. The first flush
// reports it, and a later one works.
func TestOpenDeferredNeedsNoNetwork(t *testing.T) {
	f := newFakeS3()
	var mu sync.Mutex
	up := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !up {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := s3Config(t, srv.URL+"/b")
	if _, err := c.Open(); err == nil {
		t.Fatal("Open should have reached the store and failed")
	}
	a, err := c.OpenDeferred()
	if err != nil {
		t.Fatalf("OpenDeferred touched the network: %v", err)
	}
	a.LockDir = t.TempDir()
	a.B.(*S3Backend).sleep = func(time.Duration) {}
	d := t.TempDir()
	r := &rig{t: t}
	path := filepath.Join(d, "-p", sidA+".jsonl")
	r.write(path, "line\n", false)
	src := Source{Kind: "claude", Root: d}
	if _, err := a.Flush(src, path); err == nil {
		t.Fatal("a flush during the outage reported success")
	}
	mu.Lock()
	up = true
	mu.Unlock()
	res, err := a.Flush(src, path)
	if err != nil || res.Segments != 1 {
		t.Fatalf("flush after the outage: %+v, %v", res, err)
	}
	if string(f.objects[modeKey]) != "none\n" || len(f.objects[hostClaimKey("h1")]) == 0 {
		t.Fatalf("the deferred checks did not run: %v", f.objects)
	}
	// A configuration mistake still fails early.
	bad := s3Config(t, srv.URL+"/b")
	bad.S3Plaintext = ""
	if _, err := bad.OpenDeferred(); err == nil {
		t.Fatal("the plaintext gate must not be deferred")
	}
}
