// Package archive keeps agent session files forever: a byte-exact,
// append-only, encrypted copy (this file, seal.go, segment.go, ship.go),
// and an index of sessions and their text that can be rebuilt from that
// copy at any time (index.go, extract.go).
//
// Nothing here reads or writes anything an agent depends on: it only reads
// session files. The one destructive operation is Delete, which exists so
// a session can be removed on request.
package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrExists is returned by Backend.Put when the key is already stored.
// Objects are never overwritten.
var ErrExists = errors.New("archive: object already exists")

// ErrNotExist is what Backend.Get's error wraps when the key is not stored.
// Any other error means the backend could not say: a caller must not read
// that as "not there", because on a network backend it may only be a blip.
var ErrNotExist = fs.ErrNotExist

// Backend is where archive objects live: FSBackend or S3Backend. Keys are
// slash-separated and never contain "." or ".." elements. Put must be atomic
// and must not replace an object; S3Backend says how it manages that on
// Garage, which ignores If-None-Match on PUT.
type Backend interface {
	// Put stores data under key unless the key exists (ErrExists).
	// The object is either completely visible or not at all.
	Put(key string, data []byte) error
	// Get returns the object, or an error wrapping ErrNotExist.
	Get(key string) ([]byte, error)
	// List returns every key that starts with prefix, sorted.
	List(prefix string) ([]string, error)
	Delete(key string) error
}

// FSBackend stores objects as files under Dir.
type FSBackend struct{ Dir string }

// NewFSBackend creates the directory (mode 0700) and sweeps temp files
// left by a crashed Put.
func NewFSBackend(dir string) (*FSBackend, error) {
	if err := os.MkdirAll(filepath.Join(dir, ".tmp"), 0o700); err != nil {
		return nil, err
	}
	b := &FSBackend{Dir: dir}
	ents, _ := os.ReadDir(filepath.Join(dir, ".tmp"))
	for _, e := range ents {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			_ = os.Remove(filepath.Join(dir, ".tmp", e.Name()))
		}
	}
	return b, nil
}

func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\x00") {
		return fmt.Errorf("archive: bad key %q", key)
	}
	for _, el := range strings.Split(key, "/") {
		if el == "" || el == "." || el == ".." || el == ".tmp" {
			return fmt.Errorf("archive: bad key %q", key)
		}
	}
	return nil
}

func (b *FSBackend) path(key string) string { return filepath.Join(b.Dir, filepath.FromSlash(key)) }

// Put writes to a temp file, syncs it, and hard-links it into place: link
// fails if the target exists, so a concurrent or repeated Put never
// replaces an object, and a crash leaves either nothing or a whole file.
func (b *FSBackend) Put(key string, data []byte) error {
	if err := validKey(key); err != nil {
		return err
	}
	final := b.path(key)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(b.Dir, ".tmp"), "put-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrExists
		}
		return err
	}
	if d, err := os.Open(filepath.Dir(final)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func (b *FSBackend) Get(key string) ([]byte, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	return os.ReadFile(b.path(key))
}

func (b *FSBackend) List(prefix string) ([]string, error) {
	root := b.path(strings.TrimSuffix(prefix, "/"))
	var keys []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == ".tmp" && filepath.Dir(p) == b.Dir {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(b.Dir, p)
		if err != nil {
			return err
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (b *FSBackend) Delete(key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	p := b.path(key)
	_ = os.Chmod(p, 0o600)
	err := os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
