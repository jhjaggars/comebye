package cli

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	// No Chrome: nothing, and the reason.
	fakeShots(t, "")
	if l, d, why := thumbnails(e, "<p>x"); l != nil || d != nil || !strings.Contains(why, "no Chrome found") {
		t.Fatalf("thumbnails without Chrome: %q", why)
	}
	chromeEnv(e, nil)
	// One scheme failing leaves the other, and says why.
	fakeShots(t, "dark")
	if l, d, why := thumbnails(e, "<p>x"); l == nil || d != nil || why != "boom" {
		t.Fatalf("light %v dark %v %q", l != nil, d != nil, why)
	}
	// Off by the environment: nothing, and nothing to say.
	chromeEnv(e, map[string]string{"COMEBYE_NO_THUMBNAILS": "1"})
	if l, d, why := thumbnails(e, "<p>x"); l != nil || d != nil || why != "" {
		t.Fatalf("thumbnails while off: %q", why)
	}
}

func TestThumbnailWhy(t *testing.T) {
	_, e, _, _ := newFake(t)
	chromeEnv(e, nil)
	old := shotRender
	t.Cleanup(func() { shotRender = old })
	for _, c := range []struct {
		name string
		shot func(context.Context, string, string, string) ([]byte, error)
		want string
	}{
		{"timeout", func(context.Context, string, string, string) ([]byte, error) { return nil, errChromeTimeout }, "Chrome did not finish in 8s"},
		{"chrome error", func(context.Context, string, string, string) ([]byte, error) {
			return nil, errors.New("Chrome exited exit status 1: no display")
		}, "no display"},
		{"not a png", func(context.Context, string, string, string) ([]byte, error) { return []byte("<html>"), nil }, "not a PNG"},
		{"too big", func(context.Context, string, string, string) ([]byte, error) { return noisePNG(1280, 800), nil }, "over the hub's 90 KiB"},
	} {
		shotRender = c.shot
		l, d, why := thumbnails(e, "<p>x")
		if l != nil || d != nil || !strings.Contains(why, c.want) {
			t.Errorf("%s: light %v dark %v why %q, want %q", c.name, l != nil, d != nil, why, c.want)
		}
	}
}

// noisePNG is a picture that does not compress: shrunk it stays far over the hub's cap.
func noisePNG(w, h int) []byte {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	x := uint32(1)
	for i := range im.Pix {
		x = x*1664525 + 1013904223
		im.Pix[i] = uint8(x >> 24)
		if i%4 == 3 {
			im.Pix[i] = 255
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, im)
	return b.Bytes()
}

func publishThumbs(t *testing.T, put answer, env map[string]string) (*fakeComebye, string) {
	t.Helper()
	f, e, _, errb := newFake(t)
	f.answers["POST /api/pages"] = answer{code: 201, body: `{"page":` + pageJSON + `,"path":"/p/pg_0123456789","url":"https://hub.t.ts.net/p/pg_0123456789","created":true}`}
	f.answers["PUT /api/pages/pg_0123456789/thumbs"] = put
	chromeEnv(e, env)
	fakeRender(t, "<html></html>", errors.New("no layout"))
	e.Stdin = strings.NewReader("<!doctype html><title>x</title><p>hi")
	if code := run(e, "publish", "-"); code != 0 {
		t.Fatalf("publish: %d %s", code, errb)
	}
	return f, errb.String()
}

func TestPublishSaysWhyThereIsNoThumbnail(t *testing.T) {
	// Stored: not a word.
	fakeShots(t, "")
	if _, msg := publishThumbs(t, answer{code: 200, body: `{}`}, nil); strings.Contains(msg, "thumbnail") {
		t.Errorf("stored, yet: %s", msg)
	}
	// Off by the environment: not a word either.
	if f, msg := publishThumbs(t, answer{code: 200, body: `{}`}, map[string]string{"COMEBYE_NO_THUMBNAILS": "1"}); strings.Contains(msg, "thumbnail") || f.find("PUT", "/api/pages/pg_0123456789/thumbs") != nil {
		t.Errorf("off, yet: %s", msg)
	}
	// The hub refuses: one line with its answer, and the publish still succeeds.
	_, msg := publishThumbs(t, answer{code: 400, body: `{"code":"bad_thumb","message":"light: not a PNG"}`}, nil)
	if !strings.Contains(msg, "No gallery thumbnail: the hub did not take it") || !strings.Contains(msg, "not a PNG") || strings.Count(msg, "gallery thumbnail") != 1 {
		t.Errorf("hub refusal: %s", msg)
	}
	// Chrome fails at both: nothing is sent; the reason is told.
	fakeShots(t, "light")
	old := shotRender
	shotRender = func(context.Context, string, string, string) ([]byte, error) { return nil, errChromeTimeout }
	t.Cleanup(func() { shotRender = old })
	f, msg := publishThumbs(t, answer{code: 200, body: `{}`}, nil)
	if !strings.Contains(msg, "No gallery thumbnail: Chrome did not finish in 8s.") || f.find("PUT", "/api/pages/pg_0123456789/thumbs") != nil {
		t.Errorf("timeout: %s", msg)
	}
	// One scheme made: sent, and the other's failure told.
	shotRender = old
	fakeShots(t, "dark")
	f, msg = publishThumbs(t, answer{code: 200, body: `{}`}, nil)
	if !strings.Contains(msg, "Only one gallery thumbnail") || !strings.Contains(msg, "boom") || f.find("PUT", "/api/pages/pg_0123456789/thumbs") == nil {
		t.Errorf("partial: %s", msg)
	}
}

func TestPublishWithoutChromeSaysSo(t *testing.T) {
	f, e, _, errb := newFake(t)
	f.answers["POST /api/pages"] = answer{code: 201, body: `{"page":` + pageJSON + `,"path":"/p/pg_0123456789","url":"https://hub.t.ts.net/p/pg_0123456789","created":true}`}
	fakeRender(t, "", errors.New("none"))
	e.Stdin = strings.NewReader("<!doctype html><title>x</title><p>hi")
	if code := run(e, "publish", "-"); code != 0 {
		t.Fatalf("publish: %d", code)
	}
	if !strings.Contains(errb.String(), "No gallery thumbnail: no Chrome found (put it on PATH or set COMEBYE_CHROME).") {
		t.Errorf("stderr: %s", errb)
	}
}

func TestFindChromeKnownPaths(t *testing.T) {
	_, e, _, _ := newFake(t)
	if got := findChrome(e); got != "" {
		t.Fatalf("found %q with nothing installed", got)
	}
	fake := filepath.Join(t.TempDir(), "Google Chrome")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := chromePaths
	chromePaths = []string{filepath.Join(t.TempDir(), "absent"), fake}
	t.Cleanup(func() { chromePaths = old })
	if got := findChrome(e); got != fake {
		t.Errorf("findChrome = %q, want %q", got, fake)
	}
	// COMEBYE_CHROME still wins.
	chromeEnv(e, nil)
	if got := findChrome(e); got != "/fake/chrome" {
		t.Errorf("findChrome = %q", got)
	}
}

// A Chrome that hangs (a page waiting on a host that never answers) with a helper process that holds its output:
// the run ends at the deadline, says it timed out, and leaves no process behind.
func TestRunChromeKillsAHungChromeAndItsHelpers(t *testing.T) {
	dir := t.TempDir()
	pidf := filepath.Join(dir, "helper.pid")
	script := filepath.Join(dir, "chrome")
	body := "#!/bin/sh\nsleep 300 &\necho $! > " + pidf + "\nsleep 300\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()
	err := runChrome(ctx, script, []string{"--x"}, &out)
	if !errors.Is(err, errChromeTimeout) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	b, _ := os.ReadFile(pidf)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		t.Fatal("helper never started")
	}
	for i := 0; ; i++ {
		if syscall.Kill(pid, 0) != nil { // gone
			break
		}
		if i > 50 {
			t.Fatalf("helper %d still running", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunChromeReportsChromesComplaint(t *testing.T) {
	script := filepath.Join(t.TempDir(), "chrome")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'noise' >&2\necho 'Missing X server or $DISPLAY' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runChrome(context.Background(), script, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Missing X server") || strings.Contains(err.Error(), "noise") {
		t.Fatalf("err = %v", err)
	}
}

// What a hung render needs: no network but the CDN, a profile of its own.
func TestChromeBaseFlags(t *testing.T) {
	args := strings.Join(chromeBase("/tmp/p"), "\n")
	for _, w := range []string{"--user-data-dir=/tmp/p", "--headless", `h=="cdnjs.cloudflare.com"?"DIRECT":"PROXY 127.0.0.1:9"`, "--disable-background-networking"} {
		if !strings.Contains(args, w) {
			t.Errorf("flags lack %q", w)
		}
	}
}

// With a real Chrome (skipped without one): a page that waits on hosts that never answer is still
// screenshotted well inside the time, in both schemes, and the picture fits the hub's cap.
func TestRealChromeThumbnailOfAPageWithDeadScripts(t *testing.T) {
	e := &Env{Getenv: os.Getenv}
	if os.Getenv("COMEBYE_CHROME") == "" && testing.Short() {
		t.Skip("short")
	}
	chrome := findChrome(e)
	if chrome == "" {
		t.Skip("no Chrome here")
	}
	page := `<!doctype html><meta charset=utf-8><title>x</title>
<script src="http://10.255.255.1/x.js"></script><script src="http://192.0.2.1/y.js"></script>
<link rel=stylesheet href="https://fonts.googleapis.com/css2?family=Inter">
<style>body{background:#fff;color:#000}@media(prefers-color-scheme:dark){body{background:#111;color:#eee}}</style><body style="font:20px sans-serif"><h1>Hello</h1><p>Body text of the page.</p></body>`
	start := time.Now()
	light, dark, why := thumbnails(e, page)
	if light == nil || dark == nil || why != "" {
		t.Fatalf("light %v dark %v why %q", light != nil, dark != nil, why)
	}
	if d := time.Since(start); d > thumbTimeout {
		t.Errorf("took %v", d)
	}
	for _, b := range [][]byte{light, dark} {
		if cfg, err := png.DecodeConfig(bytes.NewReader(b)); err != nil || cfg.Width != thumbW || len(b) > thumbMax {
			t.Errorf("picture %+v %v, %d bytes", cfg, err, len(b))
		}
	}
	if bytes.Equal(light, dark) {
		t.Error("light and dark are the same picture")
	}
}
