package cli

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func shotPNG(w, h int, c color.RGBA) []byte {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, im)
	return b.Bytes()
}

func TestShrinkMakesA16By10Thumbnail(t *testing.T) {
	for _, dim := range [][2]int{{1280, 800}, {1280, 2000}, {2000, 800}} {
		out, err := shrink(shotPNG(dim[0], dim[1], color.RGBA{10, 120, 200, 255}))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(out))
		if err != nil || cfg.Width != thumbW || cfg.Height != thumbH {
			t.Fatalf("%v: %+v %v", dim, cfg, err)
		}
		im, _ := png.Decode(bytes.NewReader(out))
		if r, g, b, _ := im.At(5, 5).RGBA(); r>>8 != 10 || g>>8 != 120 || b>>8 != 200 {
			t.Errorf("colour changed: %d %d %d", r>>8, g>>8, b>>8)
		}
	}
	if _, err := shrink([]byte("nope")); err == nil {
		t.Error("garbage shrinks")
	}
}

func fakeShots(t *testing.T, fail string) {
	t.Helper()
	old := shotRender
	t.Cleanup(func() { shotRender = old })
	shotRender = func(ctx context.Context, chrome, page, scheme string) ([]byte, error) {
		if scheme == fail {
			return nil, errors.New("boom")
		}
		return shotPNG(1280, 800, color.RGBA{1, 2, 3, 255}), nil
	}
}

func chromeEnv(e *Env, extra map[string]string) {
	env := e.Getenv
	e.Getenv = func(k string) string {
		if k == "COMEBYE_CHROME" {
			return "/fake/chrome"
		}
		if v, ok := extra[k]; ok {
			return v
		}
		return env(k)
	}
}

func TestPublishSendsThumbnails(t *testing.T) {
	f, e, _, errb := newFake(t)
	f.answers["POST /api/pages"] = answer{code: 201, body: `{"page":` + pageJSON + `,"path":"/p/pg_0123456789","url":"https://hub.t.ts.net/p/pg_0123456789","created":true}`}
	f.answers["PUT /api/pages/pg_0123456789/thumbs"] = answer{code: 200, body: `{}`}
	chromeEnv(e, nil)
	fakeShots(t, "")
	fakeRender(t, "<html></html>", errors.New("no layout"))
	e.Stdin = strings.NewReader("<!doctype html><title>x</title><p>hi")
	if code := run(e, "publish", "-"); code != 0 {
		t.Fatalf("publish: %d %s", code, errb)
	}
	r := f.find("PUT", "/api/pages/pg_0123456789/thumbs")
	if r == nil || !strings.Contains(r.body, "light_b64") || !strings.Contains(r.body, "dark_b64") || !strings.Contains(r.body, `"version":`) {
		t.Fatalf("requests = %v", f.reqs)
	}
}

func TestThumbnailsNeverFailAPublish(t *testing.T) {
	_, e, _, _ := newFake(t)
	// No Chrome: nothing.
	fakeShots(t, "")
	if l, d := thumbnails(e, "<p>x"); l != nil || d != nil {
		t.Fatal("thumbnails without Chrome")
	}
	chromeEnv(e, nil)
	// One scheme failing leaves the other.
	fakeShots(t, "dark")
	if l, d := thumbnails(e, "<p>x"); l == nil || d != nil {
		t.Fatalf("light %v dark %v", l != nil, d != nil)
	}
	// Off by the environment.
	chromeEnv(e, map[string]string{"COMEBYE_NO_THUMBNAILS": "1"})
	if l, d := thumbnails(e, "<p>x"); l != nil || d != nil {
		t.Fatal("thumbnails while off")
	}
}
