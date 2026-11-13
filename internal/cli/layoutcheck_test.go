package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func fakeRender(t *testing.T, dom string, err error) (calls *int) {
	t.Helper()
	old := layoutRender
	t.Cleanup(func() { layoutRender = old })
	n := 0
	layoutRender = func(ctx context.Context, chrome, host string) (string, error) {
		n++
		if !strings.Contains(host, `id="f390"`) || !strings.Contains(host, `id="f1280"`) {
			t.Errorf("host lacks both widths")
		}
		return dom, err
	}
	return &n
}

const badDOM = `<html><body><pre id="cbout">[{&#34;width&#34;:390,&#34;kind&#34;:&#34;scroll&#34;,&#34;what&#34;:&#34;the page&#34;,&#34;over&#34;:300},{&#34;width&#34;:1280,&#34;kind&#34;:&#34;clipped&#34;,&#34;what&#34;:&#34;div#a: hello&#34;,&#34;over&#34;:8}]</pre></body></html>`

func TestPublishWarnsOnLayout(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/pages"] = answer{code: 201, body: `{"page":` + pageJSON + `,"path":"/p/pg_0123456789","url":"https://hub.t.ts.net/p/pg_0123456789","created":true}`}
	env := e.Getenv
	e.Getenv = func(k string) string {
		if k == "COMEBYE_CHROME" {
			return "/fake/chrome"
		}
		return env(k)
	}
	n := fakeRender(t, badDOM, nil)
	e.Stdin = strings.NewReader("<!doctype html><title>x</title><p>hi")
	if code := run(e, "publish", "-"); code != 0 {
		t.Fatalf("publish: %d %s", code, errb)
	}
	if *n != 1 {
		t.Fatalf("renders = %d", *n)
	}
	if out.String() != "https://hub.t.ts.net/p/pg_0123456789\n" {
		t.Fatalf("stdout = %q", out)
	}
	for _, w := range []string{"at 390 px the page scrolls sideways (300 px too wide)", "at 1280 px text is clipped in div#a: hello"} {
		if !strings.Contains(errb.String(), w) {
			t.Errorf("stderr lacks %q: %s", w, errb)
		}
	}
}

func TestLayoutCheckQuiet(t *testing.T) {
	_, e, _, _ := newFake(t)
	// No Chrome found (PATH is empty in the test env): no render at all.
	n := fakeRender(t, badDOM, nil)
	if l := layoutCheck(e, "<p>x"); l != nil || *n != 0 {
		t.Fatalf("no chrome: %v, %d renders", l, *n)
	}
	env := e.Getenv
	with := func(extra map[string]string) {
		e.Getenv = func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return env(k)
		}
	}
	// Render failure, garbage output and a clean page are all silent.
	with(map[string]string{"COMEBYE_CHROME": "/fake/chrome"})
	for _, c := range []struct {
		dom string
		err error
	}{{"", errors.New("boom")}, {"<html>nothing</html>", nil}, {`<pre id="cbout">[]</pre>`, nil}} {
		fakeRender(t, c.dom, c.err)
		if l := layoutCheck(e, "<p>x"); l != nil {
			t.Errorf("%+v: warned %v", c, l)
		}
	}
	// The opt-out wins over a found Chrome.
	n = fakeRender(t, badDOM, nil)
	with(map[string]string{"COMEBYE_CHROME": "/fake/chrome", "COMEBYE_NO_LAYOUT_CHECK": "1"})
	if l := layoutCheck(e, "<p>x"); l != nil || *n != 0 {
		t.Fatalf("opt-out ignored: %v", l)
	}
}

func TestLayoutHostEscapesPage(t *testing.T) {
	h := layoutHost(`<p title="a&b">"x"</p><script>1</script>`)
	if strings.Contains(h, `title="a&b"`) || strings.Count(h, "<script>") != 1 {
		t.Fatalf("page not escaped into srcdoc: %s", h)
	}
}
