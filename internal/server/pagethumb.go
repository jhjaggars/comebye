package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// Thumbnails: the gallery's picture of a page, made by the publisher's own
// `comebye publish` (headless Chrome, one light and one dark) and sent after
// the version is stored. They are images rendered from agent HTML, so:
//   - only a PNG is accepted, checked by decoding its header: at most
//     thumbMaxBytes each and thumbMaxW x thumbMaxH pixels;
//   - sending one is changing the page: the user, a coordinator or the
//     page's publisher, as for publishing a version;
//   - they are served as image/png with nosniff and a sandboxing policy,
//     under the same read rule as the skeleton (the user, a coordinator, or
//     an agent of the page's own host);
//   - they count against the page quota.

const (
	thumbMaxBytes = 96 << 10
	thumbMaxW     = 800
	thumbMaxH     = 600
)

var thumbSchemes = []string{"light", "dark"}

func (st *pageStore) thumbPath(id string, n int, scheme string) string {
	return filepath.Join(st.dir, id, "v"+strconv.Itoa(n)+".thumb-"+scheme+".png")
}

func (st *pageStore) dropThumbs(id string, n int) {
	for _, sc := range thumbSchemes {
		_ = os.Remove(st.thumbPath(id, n, sc))
	}
}

var errThumbBad = errors.New("bad_thumb")

// checkThumb: a PNG within the caps.
func checkThumb(b []byte) error {
	if len(b) == 0 || len(b) > thumbMaxBytes {
		return fmt.Errorf("%w: a thumbnail is a PNG of at most %d KiB", errThumbBad, thumbMaxBytes>>10)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%w: not a PNG", errThumbBad)
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > thumbMaxW || cfg.Height > thumbMaxH {
		return fmt.Errorf("%w: at most %dx%d pixels", errThumbBad, thumbMaxW, thumbMaxH)
	}
	return nil
}

// putThumbs keeps the thumbnails of version n (scheme -> PNG) and records
// them in the index.
func (st *pageStore) putThumbs(id string, n int, imgs map[string][]byte, who pageWho) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, p := range st.data.Pages {
		if p.ID != id {
			continue
		}
		if !who.mayChange(p) {
			return errPageForbidden
		}
		hi := -1
		for j, v := range p.History {
			if v.N == n {
				hi = j
			}
		}
		if hi < 0 {
			return errPageNotFound
		}
		v := p.History[hi]
		var total int64
		for _, q := range st.data.Pages {
			for _, w := range q.History {
				total += w.Size + w.ThumbSize
			}
		}
		total -= v.ThumbSize
		var size int64
		for _, b := range imgs {
			size += int64(len(b))
		}
		if total+size > st.quota {
			return errPageQuota
		}
		if st.dir != "" {
			for sc, b := range imgs {
				if err := writeBodyFile(st.thumbPath(id, n, sc), b); err != nil {
					return err
				}
			}
		}
		v.Thumbs, v.ThumbSize = nil, 0
		for _, sc := range thumbSchemes {
			if b, ok := imgs[sc]; ok {
				v.Thumbs = append(v.Thumbs, sc)
				v.ThumbSize += int64(len(b))
			} else if st.dir != "" {
				if _, err := os.Stat(st.thumbPath(id, n, sc)); err == nil {
					v.Thumbs = append(v.Thumbs, sc)
				}
			}
		}
		prev := st.data
		np := p
		np.History = append([]PageVersion(nil), p.History...)
		np.History[hi] = v
		st.data.Pages = append([]Page(nil), prev.Pages...)
		st.data.Pages[i] = np
		if err := st.saveLocked(); err != nil {
			st.data = prev
			return err
		}
		return nil
	}
	return errPageNotFound
}

type thumbBody struct {
	Version int    `json:"version"`
	Light   string `json:"light_b64"`
	Dark    string `json:"dark_b64"`
}

// handlePageThumbsPut: PUT /api/pages/{id}/thumbs {version, light_b64, dark_b64}.
func (s *Server) handlePageThumbsPut(w http.ResponseWriter, r *http.Request) {
	if s.pagesOff(w, r) {
		return
	}
	bad := func(msg string) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_thumb", "message": msg})
	}
	var b thumbBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*thumbMaxBytes)).Decode(&b); err != nil {
		bad("send {version, light_b64, dark_b64}, each a small PNG")
		return
	}
	imgs := map[string][]byte{}
	for sc, enc := range map[string]string{"light": b.Light, "dark": b.Dark} {
		if enc == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			bad(sc + " is not base64")
			return
		}
		if err := checkThumb(raw); err != nil {
			bad(sc + ": " + err.Error())
			return
		}
		imgs[sc] = raw
	}
	if len(imgs) == 0 || b.Version < 1 {
		bad("send a version and at least one thumbnail")
		return
	}
	who := s.pageCaller(r)
	id := r.PathValue("id")
	if p, ok := s.pages.get(id); ok && !who.sees(p) {
		pageErr(w, errPageNotFound)
		return
	}
	if err := s.pages.putThumbs(id, b.Version, imgs, who); err != nil {
		pageErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"id": id, "version": b.Version})
}

// handlePageThumb: GET /api/pages/{id}/thumb?v=N&scheme=light|dark.
func (s *Server) handlePageThumb(w http.ResponseWriter, r *http.Request) {
	if s.pagesOff(w, r) {
		return
	}
	who := s.pageCaller(r)
	p, ok := s.pages.get(r.PathValue("id"))
	if !ok || !who.sees(p) {
		pageErr(w, errPageNotFound)
		return
	}
	if !who.user && !who.coord && !sameHost(p.Host, who.host) {
		readForbidden(w)
		return
	}
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("v"))
	if n == 0 {
		n = p.Version
	}
	sc := q.Get("scheme")
	have := false
	for _, v := range p.History {
		if v.N == n && slices.Contains(v.Thumbs, sc) {
			have = true
		}
	}
	if !have || s.pages.dir == "" {
		pageErr(w, errPageNotFound)
		return
	}
	b, err := os.ReadFile(s.pages.thumbPath(p.ID, n, sc))
	if err != nil {
		pageErr(w, errPageNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	if q.Get("v") != "" {
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(b)
}
