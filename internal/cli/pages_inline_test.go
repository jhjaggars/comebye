package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 1x1 PNG.
var tinyPNG = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89}

func writeTree(t *testing.T, files map[string][]byte) string {
	t.Helper()
	d := t.TempDir()
	for n, b := range files {
		p := filepath.Join(d, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestInlineImages(t *testing.T) {
	d := writeTree(t, map[string][]byte{
		"a.png":      tinyPNG,
		"sub/b.png":  tinyPNG,
		"c.svg":      []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
		"fake.png":   []byte("not an image"),
		"font.woff2": []byte("x"),
	})
	html := `<img src="a.png"><img alt=x src='sub/b.png'><img srcset="a.png 1x, sub/b.png 2x">
<div style="background:url(c.svg)"></div><style>.x{background:url("a.png")}.y{src:url(font.woff2)}</style>
<img src="https://example.com/x.png"><img src="data:image/png;base64,AAAA"><img src="missing.png"><img src="fake.png"><img src="/abs.png"><a href="a.png">link</a>`
	out, done, notes, err := inlineImages(html, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 3 { // a.png, sub/b.png, c.svg, once each
		t.Errorf("inlined %v", done)
	}
	for _, want := range []string{`src="data:image/png;base64,`, `src='data:image/png;base64,`, `url(data:image/svg+xml;base64,`, `url("data:image/png;base64,`,
		`src="https://example.com/x.png"`, `src="missing.png"`, `src="fake.png"`, `url(font.woff2)`, `<a href="a.png">`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `srcset="a.png`) || strings.Contains(out, "sub/b.png 2x") {
		t.Errorf("srcset not inlined:\n%s", out)
	}
	if len(notes) != 4 { // missing, fake, font, absolute
		t.Errorf("notes %v", notes)
	}
}

func TestInlineImagesStaysUnderTheDirectory(t *testing.T) {
	outer := writeTree(t, map[string][]byte{"secret.png": tinyPNG, "page/ok.png": tinyPNG})
	page := filepath.Join(outer, "page")
	if _, _, _, err := inlineImages(`<img src="../secret.png">`, page); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("dotdot: %v", err)
	}
	if _, _, _, err := inlineImages(`<img src="sub/../../secret.png">`, page); err == nil {
		t.Error("nested dotdot not refused")
	}
	if err := os.Symlink(filepath.Join(outer, "secret.png"), filepath.Join(page, "link.png")); err != nil {
		t.Skip(err)
	}
	if _, _, _, err := inlineImages(`<img src="link.png">`, page); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("symlink out: %v", err)
	}
	if err := os.Symlink("ok.png", filepath.Join(page, "inside.png")); err != nil {
		t.Fatal(err)
	}
	if out, _, _, err := inlineImages(`<img src="inside.png">`, page); err != nil || !strings.Contains(out, "data:image/png") {
		t.Errorf("symlink inside: %v %s", err, out)
	}
}

func TestPublishInlinesImages(t *testing.T) {
	f, e, _, errb := newFake(t)
	f.answers["POST /api/pages"] = answer{code: 201, body: `{"page":` + pageJSON + `,"path":"/p/pg_0123456789","url":"https://hub.t.ts.net/p/pg_0123456789","created":true}`}
	d := writeTree(t, map[string][]byte{"a.png": tinyPNG, "p.html": []byte(`<h1>x</h1><img src="a.png">`), "p.md": []byte("![x](a.png)")})
	if code := run(e, "publish", filepath.Join(d, "p.html")); code != 0 {
		t.Fatalf("publish: %d %s", code, errb)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/pages").body), &body)
	if !strings.Contains(body["body"].(string), `src="data:image/png;base64,`) || !strings.Contains(errb.String(), "Inlined 1 image(s): a.png") {
		t.Fatalf("body %v, stderr %q", body["body"], errb)
	}
	// Markdown is sent as written.
	if code := run(e, "publish", filepath.Join(d, "p.md")); code != 0 {
		t.Fatal(errb)
	}
	json.Unmarshal([]byte(f.find("POST", "/api/pages").body), &body)
	if body["body"] != "![x](a.png)" {
		t.Fatalf("markdown changed: %v", body["body"])
	}
}

func TestPublishRefusesOversizedInlinedPage(t *testing.T) {
	_, e, _, errb := newFake(t)
	big := append(append([]byte{}, tinyPNG...), make([]byte, 4<<20)...) // 4 MiB -> 5.3 MiB as base64
	d := writeTree(t, map[string][]byte{"big.png": big, "p.html": []byte(`<img src="big.png">`)})
	if code := run(e, "publish", filepath.Join(d, "p.html")); code != 1 || !strings.Contains(errb.String(), "inlined") {
		t.Fatalf("code, stderr %q", errb)
	}
	d = writeTree(t, map[string][]byte{"x.png": tinyPNG, "page/p.html": []byte(`<img src="../x.png">`)})
	if code := run(e, "publish", filepath.Join(d, "page", "p.html")); code != 1 || !strings.Contains(errb.String(), "outside") {
		t.Fatalf("escape: stderr %q", errb)
	}
}
