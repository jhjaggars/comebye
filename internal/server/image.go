package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxImageBytes caps one uploaded image (decoded). The app downscales
// screenshots before sending, so real uploads are far smaller.
const maxImageBytes = 8 << 20

// imageKeep is how long uploaded images are kept before being pruned.
const imageKeep = 7 * 24 * time.Hour

// The upload directory is capped in total, oldest images pruned first, so
// a burst of uploads cannot fill the disk within imageKeep.
const (
	maxUploadBytes = 200 << 20
	maxUploadFiles = 500
)

// imageExt maps the sniffed type of an accepted image to its extension.
var imageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// imageBody is the JSON body of an image upload: base64 data, optionally
// as a data: URL. JSON (not multipart) keeps uploads under the same
// content-type CSRF rule as every other POST and lets the hub proxy them.
type imageBody struct {
	Data string `json:"data"`
}

// handleImage stores an image (a pasted or picked screenshot) in the
// upload directory on this host, the agent's host, and returns its path.
// The app puts that path in the prompt; Claude Code and Pi read images
// from a path. target only routes the upload to the right host.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.UploadDir == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "uploads_disabled", "message": "image uploads are disabled"})
		return
	}
	var body imageBody
	limit := int64(base64.StdEncoding.EncodedLen(maxImageBytes)) + 1024
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(&body); err != nil {
		http.Error(w, "invalid body (images are limited to 8 MB)", http.StatusBadRequest)
		return
	}
	enc := body.Data
	if strings.HasPrefix(enc, "data:") {
		_, enc, _ = strings.Cut(enc, ",")
	}
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(data) == 0 {
		http.Error(w, "data must be base64", http.StatusBadRequest)
		return
	}
	if len(data) > maxImageBytes {
		http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Trust the bytes, not a client-declared type.
	ext, ok := imageExt[http.DetectContentType(data)]
	if !ok {
		http.Error(w, "not a PNG, JPEG, GIF or WebP image", http.StatusUnsupportedMediaType)
		return
	}
	if err := os.MkdirAll(s.cfg.UploadDir, 0o700); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "upload_failed", "message": err.Error()})
		return
	}
	pruneUploads(s.cfg.UploadDir, time.Now().Add(-imageKeep))
	if !capUploads(s.cfg.UploadDir, int64(len(data)), maxUploadBytes, maxUploadFiles) {
		writeJSONStatus(w, http.StatusInsufficientStorage, map[string]string{"code": "uploads_full", "message": "the upload directory is full"})
		return
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := "img-" + time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(rnd[:]) + ext
	path := filepath.Join(s.cfg.UploadDir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "upload_failed", "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"path": path, "bytes": len(data)})
}

// pruneUploads removes uploaded images older than before.
func pruneUploads(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "img-") {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// capUploads makes room for one more image of size bytes: it removes the
// oldest uploads until the directory holds fewer than maxFiles images and
// at most maxBytes including the new one. It reports false when that is
// not possible (files it cannot remove).
func capUploads(dir string, size, maxBytes int64, maxFiles int) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true // nothing there yet; the write reports real errors
	}
	type upload struct {
		name string
		size int64
		mod  time.Time
	}
	var ups []upload
	total := size
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "img-") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			ups = append(ups, upload{e.Name(), fi.Size(), fi.ModTime()})
			total += fi.Size()
		}
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].mod.Before(ups[j].mod) })
	n := len(ups)
	for _, u := range ups {
		if n < maxFiles && total <= maxBytes {
			break
		}
		if os.Remove(filepath.Join(dir, u.name)) == nil {
			n--
			total -= u.size
		}
	}
	return n < maxFiles && total <= maxBytes
}
