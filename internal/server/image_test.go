package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A minimal valid PNG header is enough for content sniffing.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func TestImageUpload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	s := New(Config{ReadLines: 100, HostName: "desk", UploadDir: dir}, nil, nil)
	b64 := base64.StdEncoding.EncodeToString(pngBytes)

	// An old upload is pruned by the next one.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "img-old.png")
	if err := os.WriteFile(old, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-imageKeep - time.Hour)
	_ = os.Chtimes(old, past, past)

	for _, path := range []string{"/api/agents/wS:p1/image", "/api/hosts/desk/agents/wS:p1/image"} {
		for _, data := range []string{b64, "data:image/png;base64," + b64} {
			w := do(s, "POST", path, nil, `{"data":"`+data+`"}`)
			var body struct{ Path string }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body)
			}
			if filepath.Dir(body.Path) != dir || !strings.HasSuffix(body.Path, ".png") {
				t.Fatalf("path = %q", body.Path)
			}
			got, err := os.ReadFile(body.Path)
			fi, _ := os.Stat(body.Path)
			if err != nil || string(got) != string(pngBytes) || fi.Mode().Perm() != 0o600 {
				t.Fatalf("stored %v %v", err, fi.Mode())
			}
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old upload not pruned")
	}

	notImage := base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\necho hi\n"))
	if w := do(s, "POST", "/api/agents/wS:p1/image", nil, `{"data":"`+notImage+`"}`); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-image: %d", w.Code)
	}
	if w := do(s, "POST", "/api/agents/wS:p1/image", nil, `{"data":"%%%"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad base64: %d", w.Code)
	}
	off := New(Config{ReadLines: 100, HostName: "desk"}, nil, nil)
	if w := do(off, "POST", "/api/agents/wS:p1/image", nil, `{"data":"`+b64+`"}`); w.Code != http.StatusNotFound {
		t.Fatalf("disabled: %d", w.Code)
	}
}

func TestCapUploads(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, name := range []string{"img-a.png", "img-b.png", "img-c.png"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i-3) * time.Minute)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	// Not an upload: never counted or removed.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	// Room for one more with at most 3 files: the oldest goes.
	if !capUploads(dir, 100, 1<<20, 3) || exists("img-a.png") || !exists("img-b.png") {
		t.Fatal("the file cap should prune the oldest upload")
	}
	// 200 new bytes under a 250-byte cap: b and c both go.
	if !capUploads(dir, 200, 250, 10) || exists("img-b.png") || exists("img-c.png") || !exists("notes.txt") {
		t.Fatal("the byte cap should prune oldest first and leave other files")
	}
	if capUploads(dir, 300, 250, 10) {
		t.Fatal("an image over the whole cap cannot fit")
	}
}
