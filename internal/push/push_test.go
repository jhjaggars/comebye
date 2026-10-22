package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeB64(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// RFC 8291 Appendix A / Section 5.
func TestEncryptRFC8291Vector(t *testing.T) {
	plaintext := mustB64(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24")
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	asPub := mustB64(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	if !bytes.Equal(asPriv.PublicKey().Bytes(), asPub) {
		t.Fatal("as_public does not match as_private")
	}
	uaPub := mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	auth := mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg")

	got, err := encrypt(plaintext, uaPub, auth, asPriv, salt)
	if err != nil {
		t.Fatal(err)
	}
	want := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml"+
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT"+
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")
	if !bytes.Equal(got, want) {
		t.Fatalf("ciphertext mismatch\n got %s\nwant %s", b64.EncodeToString(got), b64.EncodeToString(want))
	}

	// And the receiver side decrypts it with ua_private.
	uaPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := decryptForTest(got, uaPriv, auth)
	if err != nil || !bytes.Equal(pt, plaintext) {
		t.Fatalf("decrypt: %q %v", pt, err)
	}
}

// decryptForTest is the browser side of RFC 8291 for a single record.
func decryptForTest(body []byte, uaPriv *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	if len(body) < headerLen {
		return nil, errors.New("short body")
	}
	salt := body[:16]
	if rs := binary.BigEndian.Uint32(body[16:20]); rs != recordSize {
		return nil, errors.New("bad rs")
	}
	idlen := int(body[20])
	asPubBytes := body[21 : 21+idlen]
	asPub, err := ecdh.P256().NewPublicKey(asPubBytes)
	if err != nil {
		return nil, err
	}
	secret, err := uaPriv.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), uaPriv.PublicKey().Bytes()...), asPubBytes...)
	ikm, _ := hkdf.Key(sha256.New, secret, auth, string(info), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	rec, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(rec, 0x02)
	if i < 0 || len(bytes.Trim(rec[i+1:], "\x00")) != 0 {
		return nil, errors.New("bad padding")
	}
	return rec[:i], nil
}

type testBrowser struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newTestBrowser(t *testing.T) (*testBrowser, Keys) {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	return &testBrowser{priv, auth}, Keys{P256DH: b64.EncodeToString(priv.PublicKey().Bytes()), Auth: b64.EncodeToString(auth)}
}

func TestEncryptRoundTripAndLimit(t *testing.T) {
	br, keys := newTestBrowser(t)
	msg := []byte(`{"title":"héllo"}`)
	body, err := Encrypt(msg, keys)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := decryptForTest(body, br.priv, br.auth); err != nil || !bytes.Equal(pt, msg) {
		t.Fatalf("round trip: %q %v", pt, err)
	}
	if _, err := Encrypt(make([]byte, MaxPlaintext+1), keys); err == nil {
		t.Fatal("oversized payload should fail")
	}
	if _, err := Encrypt(make([]byte, MaxPlaintext), keys); err != nil {
		t.Fatalf("max payload: %v", err)
	}
}

func TestVAPIDJWTShape(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Unix(1_700_000_000, 0)
	tok, err := vapidJWT(key, "https://web.push.apple.com", "mailto:me@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	for _, p := range parts {
		if strings.ContainsAny(p, "=+/") {
			t.Fatalf("jwt part %q is not unpadded base64url", p)
		}
	}
	var hdr map[string]string
	var claims map[string]any
	if err := json.Unmarshal(mustB64(t, parts[0]), &hdr); err != nil || hdr["alg"] != "ES256" || hdr["typ"] != "JWT" {
		t.Fatalf("header %v %v", hdr, err)
	}
	if err := json.Unmarshal(mustB64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims["aud"] != "https://web.push.apple.com" || claims["sub"] != "mailto:me@example.com" {
		t.Fatalf("claims %v", claims)
	}
	if exp := int64(claims["exp"].(float64)); exp <= now.Unix() || exp > now.Add(24*time.Hour).Unix() {
		t.Fatalf("exp %d must be within 24h", exp)
	}
	sig := mustB64(t, parts[2])
	if len(sig) != 64 || !verifyES256(&key.PublicKey, parts[0]+"."+parts[1], sig) {
		t.Fatal("signature must be a valid raw 64-byte R||S")
	}
}

func TestVAPIDKeyPersisted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	k1, err := LoadOrCreateVAPID(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, vapidFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", fi.Mode(), err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode %v", di.Mode())
	}
	k2, err := LoadOrCreateVAPID(dir)
	if err != nil || !k1.Equal(k2) {
		t.Fatalf("reload gave a different key: %v", err)
	}
	pub, _ := publicKeyBytes(k1)
	if len(pub) != 65 || pub[0] != 4 {
		t.Fatalf("public key not uncompressed: %d", len(pub))
	}
}

func TestSubjects(t *testing.T) {
	for sub, ok := range map[string]bool{
		"mailto:me@example.com":             true,
		"https://example.com/contact":       true,
		"mailto:admin@localhost":            false,
		"mailto:me@github":                  false,
		"mailto: me@example.com":            false,
		"mailto:<me@example.com>":           false,
		"https://localhost":                 false,
		"https://box.tailnet.ts.net":        false,
		"https://10.0.0.1":                  false,
		"http://example.com":                false,
		"me@example.com":                    false,
		"mailto:me@printer.local":           false,
		DefaultSubject:                      true,
		"mailto:first.last@sub.example.org": true,
	} {
		if err := ValidateSubject(sub); (err == nil) != ok {
			t.Errorf("ValidateSubject(%q) = %v, want ok=%v", sub, err, ok)
		}
	}
	if got := DefaultSubjectFor([]string{"me@github", "me@example.com"}); got != "mailto:me@example.com" {
		t.Errorf("DefaultSubjectFor = %q", got)
	}
	if got := DefaultSubjectFor([]string{"me@github"}); got != DefaultSubject {
		t.Errorf("DefaultSubjectFor github login = %q", got)
	}
}

func TestValidateEndpoint(t *testing.T) {
	for ep, ok := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/abc":                 true,
		"https://web.push.apple.com/QGx1":                         true,
		"https://updates.push.services.mozilla.com/wpush/v2/gAAA": true,
		"https://wns2-par02p.notify.windows.com/w/?token=x":       true,
		"https://FCM.googleapis.com./fcm/send/abc":                true,
		"http://fcm.googleapis.com/fcm/send/abc":                  false,
		"https://fcm.googleapis.com:8443/x":                       false,
		"https://user:pw@fcm.googleapis.com/x":                    false,
		"https://evilfcm.googleapis.com.attacker.net/x":           false,
		"https://attacker.net/fcm.googleapis.com":                 false,
		"https://127.0.0.1/x":                                     false,
		"https://[::1]/x":                                         false,
		"https://localhost/x":                                     false,
		"https://169.254.169.254/latest/meta-data":                false,
		"https://box.tailnet.ts.net/api/agents/x/keys":            false,
		"file:///etc/passwd":                                      false,
		"":                                                        false,
		"https://fcm.googleapis.com/" + strings.Repeat("a", 3000): false,
	} {
		if err := ValidateEndpoint(ep); (err == nil) != ok {
			t.Errorf("ValidateEndpoint(%.60q) = %v, want ok=%v", ep, err, ok)
		}
	}
}

func TestPublicAddr(t *testing.T) {
	for a, ok := range map[string]bool{
		"142.250.80.10": true, "2a00:1450:4001::1": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.100.100.100": false, "100.64.0.1": false, "0.0.0.0": false,
		"::1": false, "fe80::1": false, "fd7a:115c:a1e0::1": false, "::ffff:127.0.0.1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(a)); got != ok {
			t.Errorf("publicAddr(%s) = %v, want %v", a, got, ok)
		}
	}
	if err := safeDialControl("tcp", "127.0.0.1:443", nil); err == nil {
		t.Error("dialer should refuse loopback")
	}
	if err := safeDialControl("tcp", "142.250.80.10:443", nil); err != nil {
		t.Errorf("dialer refused a public address: %v", err)
	}
}

func TestStorePersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, keys := newTestBrowser(t)
	sub := Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/a", Keys: keys, Login: "me@example.com", Created: time.Unix(1, 0).UTC()}
	if err := sub.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(sub); err != nil {
		t.Fatal(err)
	}
	sub2 := sub
	sub2.Endpoint = "https://fcm.googleapis.com/fcm/send/b"
	sub2.Created = time.Unix(2, 0).UTC()
	if err := s.Put(sub2); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, subsFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("subs file mode: %v %v", fi, err)
	}
	re, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l := re.List(); len(l) != 2 || l[0] != sub || l[1] != sub2 {
		t.Fatalf("reloaded %+v", l)
	}
	if ok, err := re.Delete(sub.Endpoint); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, _ := re.Delete(sub.Endpoint); ok {
		t.Fatal("second delete should report absent")
	}
	re2, _ := OpenStore(dir)
	if re2.Len() != 1 {
		t.Fatalf("after delete: %d", re2.Len())
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
	bad := sub
	bad.Keys.Auth = "AAAA"
	if bad.Validate() == nil {
		t.Fatal("short auth secret accepted")
	}
}

func TestStoreLimit(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	_, keys := newTestBrowser(t)
	for i := range MaxSubscriptions {
		if err := s.Put(Subscription{Endpoint: "https://fcm.googleapis.com/" + string(rune('A'+i%26)) + strings.Repeat("x", i), Keys: keys}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(Subscription{Endpoint: "https://fcm.googleapis.com/overflow", Keys: keys}); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("want ErrStoreFull, got %v", err)
	}
}

func TestPresence(t *testing.T) {
	p := NewPresence()
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	p.Update("c1", "wS:p1", true)
	if !p.Viewing("wS:p1") || p.Viewing("wS:p2") {
		t.Fatal("c1 views p1 only")
	}
	now = now.Add(p.TTL - time.Second)
	if !p.Viewing("wS:p1") {
		t.Fatal("still within TTL")
	}
	p.Update("c1", "wS:p1", true) // refresh
	now = now.Add(p.TTL - time.Second)
	if !p.Viewing("wS:p1") {
		t.Fatal("refresh should extend")
	}
	now = now.Add(2 * time.Second)
	if p.Viewing("wS:p1") {
		t.Fatal("expired report must not suppress")
	}
	p.Update("c1", "wS:p1", true)
	p.Update("c2", "wS:p1", true)
	p.Update("c1", "wS:p1", false)
	if !p.Viewing("wS:p1") {
		t.Fatal("c2 still views p1")
	}
	p.Update("c2", "", true)
	if p.Viewing("wS:p1") {
		t.Fatal("closing detail clears presence")
	}
}

// pushServer is a fake push service that decrypts what it receives.
type pushServer struct {
	t      *testing.T
	br     *testBrowser
	status int
	mu     sync.Mutex
	got    []*http.Request
	bodies [][]byte
}

func (p *pushServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	pt, err := decryptForTest(body, p.br.priv, p.br.auth)
	if err != nil {
		p.t.Errorf("push service could not decrypt: %v", err)
	}
	p.mu.Lock()
	p.got = append(p.got, r)
	p.bodies = append(p.bodies, pt)
	p.mu.Unlock()
	w.WriteHeader(p.status)
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	key, _ := LoadOrCreateVAPID(dir)
	store, _ := OpenStore(dir)
	s, err := newService(Config{Dir: dir, Subject: "mailto:me@example.com", MinInterval: time.Minute, ExcerptChars: 200}, key, store)
	if err != nil {
		t.Fatal(err)
	}
	s.validate = func(string) error { return nil }
	s.client = http.DefaultClient
	return s
}

func TestSendHeadersAndPayload(t *testing.T) {
	s := newTestService(t)
	br, keys := newTestBrowser(t)
	ps := &pushServer{t: t, br: br, status: http.StatusCreated}
	srv := httptest.NewServer(ps)
	defer srv.Close()
	sub := Subscription{Endpoint: srv.URL + "/push/abc", Keys: keys}
	if err := s.store.Put(sub); err != nil {
		t.Fatal(err)
	}
	payload, _ := encodePayload(Message{Title: "fixer needs input", Body: "repo", URL: "/#/agent/wS%3Ap1", Tag: "wS:p1", Renotify: true})
	code, err := s.Send(context.Background(), sub, payload, Options{TTL: 2 * time.Hour, Urgency: "high", Topic: TopicFor("wS:p1")})
	if err != nil || code != 201 {
		t.Fatalf("send: %d %v", code, err)
	}
	r := ps.got[0]
	if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "7200" || r.Header.Get("Urgency") != "high" {
		t.Fatalf("headers %v", r.Header)
	}
	if tp := r.Header.Get("Topic"); len(tp) != 32 || strings.ContainsAny(tp, "=+/:") {
		t.Fatalf("topic %q", tp)
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "vapid t=") || !strings.HasSuffix(auth, ", k="+s.PublicKey()) {
		t.Fatalf("authorization %q", auth)
	}
	jwt := strings.TrimSuffix(strings.TrimPrefix(auth, "vapid t="), ", k="+s.PublicKey())
	var claims map[string]any
	json.Unmarshal(mustB64(t, strings.Split(jwt, ".")[1]), &claims)
	if claims["aud"] != srv.URL {
		t.Fatalf("aud %v, want %s", claims["aud"], srv.URL)
	}
	var m Message
	if err := json.Unmarshal(ps.bodies[0], &m); err != nil || m.Title != "fixer needs input" || m.URL != "/#/agent/wS%3Ap1" || !m.Renotify {
		t.Fatalf("payload %+v %v", m, err)
	}
}

func TestSendRemovesGoneSubscription(t *testing.T) {
	s := newTestService(t)
	br, keys := newTestBrowser(t)
	srv := httptest.NewServer(&pushServer{t: t, br: br, status: http.StatusGone})
	defer srv.Close()
	sub := Subscription{Endpoint: srv.URL + "/gone", Keys: keys}
	s.store.Put(sub)
	if _, err := s.Send(context.Background(), sub, []byte(`{}`), Options{}); !errors.Is(err, ErrGone) {
		t.Fatalf("want ErrGone, got %v", err)
	}
	if s.store.Len() != 0 {
		t.Fatal("410 should remove the subscription")
	}
}

func TestSendRefusesInvalidEndpoint(t *testing.T) {
	s := newTestService(t)
	s.validate = ValidateEndpoint
	_, keys := newTestBrowser(t)
	if _, err := s.Send(context.Background(), Subscription{Endpoint: "https://127.0.0.1/x", Keys: keys}, []byte(`{}`), Options{}); err == nil {
		t.Fatal("send to loopback should be refused")
	}
}

func TestNotifyAsyncAndThrottled(t *testing.T) {
	s := newTestService(t)
	br, keys := newTestBrowser(t)
	ps := &pushServer{t: t, br: br, status: http.StatusCreated}
	srv := httptest.NewServer(ps)
	defer srv.Close()
	s.store.Put(Subscription{Endpoint: srv.URL + "/a", Keys: keys})
	s.Notify("fixer/done", Message{Title: "fixer is done"}, Options{})
	s.Notify("fixer/done", Message{Title: "fixer is done"}, Options{}) // throttled
	s.Notify("fixer/blocked", Message{Title: "fixer needs input"}, Options{})
	deadline := time.Now().Add(5 * time.Second)
	for {
		ps.mu.Lock()
		n := len(ps.got)
		ps.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d pushes, want 2", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if len(ps.got) != 2 {
		t.Fatalf("got %d pushes after settling, want 2", len(ps.got))
	}
}

func TestEncodePayloadTruncates(t *testing.T) {
	b, err := encodePayload(Message{Title: "t", Body: strings.Repeat("é", 5000)})
	if err != nil || len(b) > MaxPlaintext {
		t.Fatalf("len %d err %v", len(b), err)
	}
}

func TestPresenceLastViewed(t *testing.T) {
	p := NewPresence()
	now := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return now }
	if !p.LastViewed("wS:p1").IsZero() {
		t.Fatal("never viewed should be zero")
	}
	p.Update("c1", "wS:p1", true)
	now = now.Add(time.Hour)
	p.Update("c1", "wS:p1", false) // hiding does not count as a view
	if got := p.LastViewed("wS:p1"); !got.Equal(now.Add(-time.Hour)) {
		t.Fatalf("LastViewed = %v", got)
	}
}

// A full last-viewed table evicts its stalest entry, so junk keys cannot
// lock out real agents.
func TestPresenceViewedEvictsOldest(t *testing.T) {
	p := NewPresence()
	now := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return now }
	p.Update("c", "first", true)
	for i := 0; i < maxViewed; i++ {
		now = now.Add(time.Second)
		p.Update("c", fmt.Sprintf("junk%d", i), true)
	}
	now = now.Add(time.Second)
	p.Update("c", "real", true)
	if !p.LastViewed("first").IsZero() || !p.LastViewed("junk0").IsZero() {
		t.Fatal("the oldest entries should have been evicted")
	}
	if !p.LastViewed("real").Equal(now) || len(p.viewed) != maxViewed {
		t.Fatalf("real agent not recorded (table %d)", len(p.viewed))
	}
}
