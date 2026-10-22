package server

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jhjaggars/herdr-phone/internal/push"
)

const me = "me@example.com"

func newPushTest(t *testing.T, pushURL string) (*Server, *push.Service, *push.Presence) {
	t.Helper()
	svc, err := push.New(push.Config{
		Dir: t.TempDir(), Subject: "mailto:me@example.com",
		HTTPClient: http.DefaultClient,
		CheckEndpoint: func(ep string) error {
			if pushURL != "" && strings.HasPrefix(ep, pushURL) {
				return nil
			}
			return push.ValidateEndpoint(ep)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pr := push.NewPresence()
	return New(Config{AllowLogins: []string{me, "other@example.com"}, AllowHosts: testHosts, ReadLines: 100, Push: svc, Presence: pr}, nil, nil), svc, pr
}

// call goes through the full Handler (CSRF + JSON checks) as a same-origin
// browser request from login.
func call(s *Server, method, path, login, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if login != "" {
		r.Header.Set("Tailscale-User-Login", login)
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func browserKeys(t *testing.T) push.Keys {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	enc := base64.RawURLEncoding
	return push.Keys{P256DH: enc.EncodeToString(priv.PublicKey().Bytes()), Auth: enc.EncodeToString(auth)}
}

func subJSON(endpoint string, k push.Keys) string {
	b, _ := json.Marshal(map[string]any{"endpoint": endpoint, "expirationTime": nil, "keys": k})
	return string(b)
}

func TestPushEndpointsRequireAuthAndCSRF(t *testing.T) {
	s, _, _ := newPushTest(t, "")
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/push/key"}, {"POST", "/api/push/subscribe"}, {"POST", "/api/push/unsubscribe"},
		{"POST", "/api/push/test"}, {"POST", "/api/push/presence"},
	} {
		if w := call(s, ep.method, ep.path, "", "{}"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without login: %d", ep.method, ep.path, w.Code)
		}
		if w := call(s, ep.method, ep.path, "stranger@example.com", "{}"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with other login: %d", ep.method, ep.path, w.Code)
		}
		if ep.method != "POST" {
			continue
		}
		r := httptest.NewRequest("POST", ep.path, strings.NewReader("{}"))
		r.Header.Set("Tailscale-User-Login", me)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("cross-site %s: %d", ep.path, w.Code)
		}
		r = httptest.NewRequest("POST", ep.path, strings.NewReader("{}"))
		r.Header.Set("Tailscale-User-Login", me)
		r.Header.Set("Content-Type", "text/plain")
		w = httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("text/plain %s: %d", ep.path, w.Code)
		}
	}
}

func TestPushKeyAndDisabled(t *testing.T) {
	s, svc, _ := newPushTest(t, "")
	w := call(s, "GET", "/api/push/key", me, "")
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got["enabled"] != true || got["key"] != svc.PublicKey() {
		t.Fatalf("key: %d %v", w.Code, got)
	}
	if k, _ := base64.RawURLEncoding.DecodeString(svc.PublicKey()); len(k) != 65 {
		t.Fatalf("public key is %d bytes", len(k))
	}
	off := New(Config{AllowLogins: []string{me}, AllowHosts: testHosts}, nil, nil)
	if w := call(off, "GET", "/api/push/key", me, ""); !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled key: %s", w.Body.String())
	}
	if w := call(off, "POST", "/api/push/subscribe", me, "{}"); w.Code != 404 || !strings.Contains(w.Body.String(), "push_disabled") {
		t.Fatalf("disabled subscribe: %d %s", w.Code, w.Body.String())
	}
}

func TestPushSubscribeValidation(t *testing.T) {
	s, svc, _ := newPushTest(t, "")
	k := browserKeys(t)
	for _, ep := range []string{"http://fcm.googleapis.com/x", "https://127.0.0.1/x", "https://box.tailnet.ts.net/api/agents/x/keys", "https://attacker.example.com/x"} {
		if w := call(s, "POST", "/api/push/subscribe", me, subJSON(ep, k)); w.Code != 400 || !strings.Contains(w.Body.String(), "bad_endpoint") {
			t.Errorf("%s: %d %s", ep, w.Code, w.Body.String())
		}
	}
	bad := k
	bad.P256DH = "AAAA"
	if w := call(s, "POST", "/api/push/subscribe", me, subJSON("https://fcm.googleapis.com/fcm/send/x", bad)); w.Code != 400 {
		t.Errorf("bad keys: %d", w.Code)
	}
	if svc.Store().Len() != 0 {
		t.Fatal("nothing should be stored")
	}
	ep := "https://fcm.googleapis.com/fcm/send/abc"
	if w := call(s, "POST", "/api/push/subscribe", "Me@Example.com", subJSON(ep, k)); w.Code != 200 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
	}
	sub, ok := svc.Store().Get(ep)
	if !ok || sub.Login != me || sub.Keys != k {
		t.Fatalf("stored %+v", sub)
	}
	// Another login may not take it over with its own keys...
	if w := call(s, "POST", "/api/push/subscribe", "other@example.com", subJSON(ep, browserKeys(t))); w.Code != 403 {
		t.Fatalf("foreign re-subscribe: %d", w.Code)
	}
	if sub, _ := svc.Store().Get(ep); sub.Login != me || sub.Keys != k {
		t.Fatalf("foreign re-subscribe changed the subscription: %+v", sub)
	}
	// ...or remove it.
	if w := call(s, "POST", "/api/push/unsubscribe", "other@example.com", `{"endpoint":"`+ep+`"}`); w.Code != 403 {
		t.Fatalf("foreign unsubscribe: %d", w.Code)
	}
	if w := call(s, "POST", "/api/push/unsubscribe", me, `{"endpoint":"`+ep+`"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"removed":true`) {
		t.Fatalf("unsubscribe: %d %s", w.Code, w.Body.String())
	}
	if svc.Store().Len() != 0 {
		t.Fatal("unsubscribe should remove")
	}
}

func TestPushSubscribeReplaces(t *testing.T) {
	s, svc, _ := newPushTest(t, "")
	k := browserKeys(t)
	old, nu := "https://fcm.googleapis.com/fcm/send/old", "https://fcm.googleapis.com/fcm/send/new"
	call(s, "POST", "/api/push/subscribe", me, subJSON(old, k))
	body := `{"endpoint":"` + nu + `","keys":{"p256dh":"` + k.P256DH + `","auth":"` + k.Auth + `"},"replaces":"` + old + `"}`
	if w := call(s, "POST", "/api/push/subscribe", me, body); w.Code != 200 {
		t.Fatalf("replace: %d", w.Code)
	}
	if _, ok := svc.Store().Get(old); ok {
		t.Fatal("old endpoint should be gone")
	}
	if _, ok := svc.Store().Get(nu); !ok {
		t.Fatal("new endpoint missing")
	}
}

func TestPushTestSendsToCallerOnly(t *testing.T) {
	var hits atomic.Int32
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") {
			t.Errorf("bad push request headers: %v", r.Header)
		}
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer ps.Close()
	s, svc, _ := newPushTest(t, ps.URL)
	k := browserKeys(t)
	svc.Store().Put(push.Subscription{Endpoint: ps.URL + "/mine", Keys: k, Login: me})
	svc.Store().Put(push.Subscription{Endpoint: ps.URL + "/gone", Keys: k, Login: me})
	svc.Store().Put(push.Subscription{Endpoint: ps.URL + "/theirs", Keys: k, Login: "other@example.com"})

	w := call(s, "POST", "/api/push/test", me, `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sent":1`) {
		t.Fatalf("test: %d %s", w.Code, w.Body.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("push service got %d requests, want 2 (caller's subscriptions only)", hits.Load())
	}
	if _, ok := svc.Store().Get(ps.URL + "/gone"); ok {
		t.Fatal("410 subscription should be removed")
	}
	if w := call(s, "POST", "/api/push/test", "other@example.com", `{"endpoint":"`+ps.URL+`/mine"}`); w.Code != 404 {
		t.Fatalf("test of someone else's endpoint: %d", w.Code)
	}
}

func TestPresenceEndpoint(t *testing.T) {
	s, _, pr := newPushTest(t, "")
	if w := call(s, "POST", "/api/push/presence", me, `{"client":"c1","pane":"wS:p1","visible":true}`); w.Code != 204 {
		t.Fatalf("presence: %d", w.Code)
	}
	if !pr.Viewing("wS:p1") {
		t.Fatal("presence not recorded")
	}
	// Another login's identical client id is a different entry.
	call(s, "POST", "/api/push/presence", "other@example.com", `{"client":"c1","pane":"","visible":false}`)
	if !pr.Viewing("wS:p1") {
		t.Fatal("another login cleared my presence")
	}
	call(s, "POST", "/api/push/presence", me, `{"client":"c1","pane":"wS:p1","visible":false}`)
	if pr.Viewing("wS:p1") {
		t.Fatal("hidden should clear")
	}
	if w := call(s, "POST", "/api/push/presence", me, `{"pane":"wS:p1","visible":true}`); w.Code != 400 {
		t.Fatalf("missing client: %d", w.Code)
	}
}
