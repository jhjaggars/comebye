package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// The gallery thumbnails. After `comebye publish` has stored an HTML page it
// renders it in headless Chrome, once light and once dark, shrinks each
// picture and sends both to the hub with the version. It is a courtesy, like
// the layout check: no Chrome, a failed render, a slow page or a hub that
// does not take thumbnails all mean no thumbnail (the gallery shows its
// skeleton) and never an error. It costs a publish at most thumbTimeout.
// COMEBYE_NO_THUMBNAILS=1 turns it off; COMEBYE_CHROME names the browser.

const (
	thumbTimeout = 6 * time.Second
	thumbW       = 400 // the picture sent: 1280x800 shrunk, 16:10 as the gallery's cards
	thumbH       = 250
)

// shotRender renders page in headless Chrome at 1280x800 in the scheme
// ("light" or "dark") and returns the PNG. Tests replace it.
var shotRender = func(ctx context.Context, chrome, page, scheme string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "comebye-shot-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	f, out := filepath.Join(dir, "page.html"), filepath.Join(dir, "shot.png")
	if err := os.WriteFile(f, []byte(page), 0o600); err != nil {
		return nil, err
	}
	pref := "1" // Blink's enum: 0 is dark, 1 is light
	if scheme == "dark" {
		pref = "0"
	}
	cmd := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--no-first-run",
		"--hide-scrollbars", "--user-data-dir="+filepath.Join(dir, "profile"),
		"--blink-settings=preferredColorScheme="+pref,
		"--window-size=1280,800", "--virtual-time-budget=5000", "--screenshot="+out, "file://"+f)
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

// shrink scales a PNG down to thumbW x thumbH by averaging (a box filter) and
// returns a PNG; the picture is cropped to the cards' 16:10 first.
func shrink(b []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	r := src.Bounds()
	w, h := r.Dx(), r.Dy()
	if w*thumbH > h*thumbW { // wider than 16:10: crop the sides
		nw := h * thumbW / thumbH
		r.Min.X += (w - nw) / 2
		r.Max.X = r.Min.X + nw
	} else { // taller: keep the top
		r.Max.Y = r.Min.Y + w*thumbH/thumbW
	}
	w, h = r.Dx(), r.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, thumbW, thumbH))
	for y := 0; y < thumbH; y++ {
		y0, y1 := r.Min.Y+y*h/thumbH, r.Min.Y+(y+1)*h/thumbH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < thumbW; x++ {
			x0, x1 := r.Min.X+x*w/thumbW, r.Min.X+(x+1)*w/thumbW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, n uint32
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, _ := src.At(xx, yy).RGBA()
					sr, sg, sb, n = sr+cr>>8, sg+cg>>8, sb+cb>>8, n+1
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(sr / n), uint8(sg / n), uint8(sb / n), 255})
		}
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, dst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// thumbnails returns the light and dark pictures of page; either may be nil.
func thumbnails(e *Env, page string) (light, dark []byte) {
	if e.Getenv("COMEBYE_NO_THUMBNAILS") != "" {
		return nil, nil
	}
	chrome := findChrome(e)
	if chrome == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), thumbTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for sc, dst := range map[string]*[]byte{"light": &light, "dark": &dark} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shot, err := shotRender(ctx, chrome, page, sc)
			if err != nil {
				return
			}
			if small, err := shrink(shot); err == nil && len(small) <= 90<<10 {
				*dst = small
			}
		}()
	}
	wg.Wait()
	return light, dark
}

// sendThumbnails renders and uploads the thumbnails of a stored version. It
// reports nothing: see above.
func sendThumbnails(e *Env, c *client, id string, version int, page string) {
	light, dark := thumbnails(e, page)
	if light == nil && dark == nil {
		return
	}
	req := map[string]any{"version": version}
	if light != nil {
		req["light_b64"] = base64.StdEncoding.EncodeToString(light)
	}
	if dark != nil {
		req["dark_b64"] = base64.StdEncoding.EncodeToString(dark)
	}
	_, _ = c.raw("PUT", "/api/pages/"+id+"/thumbs", nil, req)
}
