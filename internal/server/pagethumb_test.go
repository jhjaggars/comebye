package server

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func thumbPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		im.Set(x, 0, color.RGBA{200, 10, 10, 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestPageThumbnails(t *testing.T) {
	s, _, _ := todoServer(t)
	p := pageOf(t, do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"key": "k", "body": "<p>x"})).Body.Bytes())
	good := thumbPNG(t, 400, 250)
	put := func(h map[string]string, m map[string]any) int {
		return do(s, "PUT", "/api/pages/"+p.ID+"/thumbs", h, pubJSON(m)).Code
	}
	// The publisher sends both; the index says which versions have them.
	if c := put(agentHdr("w2:p1"), map[string]any{"version": 1, "light_b64": b64(good), "dark_b64": b64(good)}); c != 200 {
		t.Fatalf("publisher put: %d", c)
	}
	got := pageOf(t, do(s, "GET", "/api/pages/"+p.ID, userHdr, "").Body.Bytes())
	if th := got.History[0].Thumbs; len(th) != 2 || th[0] != "light" || th[1] != "dark" || got.History[0].ThumbSize != int64(2*len(good)) {
		t.Fatalf("thumbs = %+v", got.History[0])
	}
	// The user reads it as an image that is never sniffed or run.
	w := do(s, "GET", "/api/pages/"+p.ID+"/thumb?v=1&scheme=dark", userHdr, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || !bytes.Equal(w.Body.Bytes(), good) {
		t.Fatalf("get: %d %v", w.Code, w.Header())
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("a pinned version is cacheable: %q", w.Header().Get("Cache-Control"))
	}
	// Same host reads; another host does not; no token, nothing.
	if c := do(s, "GET", "/api/pages/"+p.ID+"/thumb?scheme=light", agentHdr("w3:p1"), "").Code; c != 200 {
		t.Errorf("same-host agent read: %d", c)
	}
	fwd := map[string]string{"Tailscale-User-Login": "me@example.com", HeaderForwarded: "1", HeaderAgent: "w3:p1", HeaderAgentHost: "elsewhere"}
	if c := do(s, "GET", "/api/pages/"+p.ID+"/thumb?scheme=light", fwd, "").Code; c != 403 {
		t.Errorf("other-host read: %d", c)
	}
	if c := do(s, "GET", "/api/pages/"+p.ID+"/thumb?scheme=light", nil, "").Code; c != 401 {
		t.Errorf("anonymous read: %d", c)
	}
	if c := do(s, "GET", "/api/pages/"+p.ID+"/thumb?v=2&scheme=light", userHdr, "").Code; c != 404 {
		t.Errorf("a version without one: %d", c)
	}
	if c := do(s, "GET", "/api/pages/"+p.ID+"/thumb?v=1&scheme=../x", userHdr, "").Code; c != 404 {
		t.Errorf("odd scheme: %d", c)
	}
	// Another agent may not send one for it; a missing version or page is 404.
	if c := put(agentHdr("w3:p1"), map[string]any{"version": 1, "light_b64": b64(good)}); c != 403 {
		t.Errorf("other agent put: %d", c)
	}
	if c := put(agentHdr("w2:p1"), map[string]any{"version": 7, "light_b64": b64(good)}); c != 404 {
		t.Errorf("unknown version: %d", c)
	}
	if c := do(s, "PUT", "/api/pages/pg_0000000000/thumbs", userHdr, pubJSON(map[string]any{"version": 1, "light_b64": b64(good)})).Code; c != 404 {
		t.Errorf("unknown page: %d", c)
	}
	if c := do(s, "PUT", "/api/pages/"+p.ID+"/thumbs", nil, pubJSON(map[string]any{"version": 1, "light_b64": b64(good)})).Code; c != 401 {
		t.Errorf("anonymous put: %d", c)
	}
	// What is not a small PNG is refused.
	big := append(thumbPNG(t, 400, 250), bytes.Repeat([]byte{0}, thumbMaxBytes)...)
	for name, m := range map[string]map[string]any{
		"not png":    {"version": 1, "light_b64": b64([]byte("<svg onload=alert(1)>"))},
		"html":       {"version": 1, "light_b64": b64([]byte("<html><script>x</script>"))},
		"too big":    {"version": 1, "light_b64": b64(big)},
		"too wide":   {"version": 1, "light_b64": b64(thumbPNG(t, 900, 100))},
		"not base64": {"version": 1, "light_b64": "!!!"},
		"none":       {"version": 1},
		"no version": {"light_b64": b64(good)},
	} {
		if c := put(agentHdr("w2:p1"), m); c != 400 {
			t.Errorf("%s: %d", name, c)
		}
	}
	// A new version has none until its own are sent; old versions' stay.
	do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"id": p.ID, "body": "<p>y"}))
	if c := do(s, "GET", "/api/pages/"+p.ID+"/thumb?scheme=light", userHdr, "").Code; c != 404 {
		t.Errorf("newest version has none yet: %d", c)
	}
	if c := do(s, "GET", "/api/pages/"+p.ID+"/thumb?v=1&scheme=light", userHdr, "").Code; c != 200 {
		t.Errorf("v1 keeps its own: %d", c)
	}
	// Deleting the page deletes them.
	do(s, "DELETE", "/api/pages/"+p.ID, userHdr, "")
	if c := do(s, "GET", "/api/pages/"+p.ID+"/thumb?v=1&scheme=light", userHdr, "").Code; c != 404 {
		t.Errorf("after delete: %d", c)
	}
}
