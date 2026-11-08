// Package stt is the self-hosted speech-to-text engine's data: the pinned
// list of files the browser needs (model weights, tokenizer, the ONNX
// runtime), the command that fetches them once on a host, and the check
// that they are all there.
//
// Nothing here runs speech recognition. The browser does, in a Web Worker
// (web/stt-worker.js), from files the server hands out under
// /stt/<Version>/. They live in the state directory, not in the binary:
// they are 100 to 240 MB, and every one of them is checked against a
// SHA-256 pinned in this file before it is installed.
package stt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Version names the file set. It is the versioned path segment the server
// serves them under (/stt/base.en-v1/) and the directory under the state
// directory. Change any pinned file and bump it: the files are served
// immutable, so a browser keeps what it has under the old name.
const Version = "base.en-v1"

// Backend says which execution path needs a file. The browser picks WebGPU
// when it has an adapter and WASM otherwise; the two use different weights.
const (
	Both   = ""       // needed by either
	WebGPU = "webgpu" // fp32 encoder, 4-bit decoder
	WASM   = "wasm"   // int8 encoder and decoder
)

// File is one pinned upstream file.
type File struct {
	// Path is the slash path under the version directory, which is also
	// the path the browser asks for under /stt/<Version>/.
	Path string
	// URL is where `comebye stt fetch` gets it from. Only the host that
	// fetches talks to it; browsers never do.
	URL string
	// SHA256 (hex) and Size are what the download must match.
	SHA256  string
	Size    int64
	Backend string
}

const (
	hfBase  = "https://huggingface.co/onnx-community/whisper-base.en/resolve/51eefc0af78b103839eda9e7e4f4186acc6517fe/"
	ortBase = "https://cdn.jsdelivr.net/npm/onnxruntime-web@1.31.0-dev.20260914-8d85527a0/dist/"
	model   = "models/whisper-base.en/"
)

func hf(rel, sum string, size int64, backend string) File {
	return File{Path: model + rel, URL: hfBase + rel, SHA256: sum, Size: size, Backend: backend}
}

// Files is the pinned set. The Hugging Face URLs name a commit, not
// "main", so they cannot change under us; the hashes would catch it if
// they did. The ONNX runtime (.mjs and .wasm) must be the build the JS
// bundle in web/vendor/transformers.bundle.js was made with
// (onnxruntime-web 1.31.0-dev.20260914-8d85527a0): see web/vendor/README.md.
func Files() []File { return files }

var files = []File{
	{Path: "ort/ort-wasm-simd-threaded.asyncify.mjs", URL: ortBase + "ort-wasm-simd-threaded.asyncify.mjs",
		SHA256: "0966b6105cd936744498aa60df7a22cbd47af3374dbc64a9ab561c08a71e3611", Size: 53057},
	{Path: "ort/ort-wasm-simd-threaded.asyncify.wasm", URL: ortBase + "ort-wasm-simd-threaded.asyncify.wasm",
		SHA256: "49871f5a4409519797e127440868a6d1923339d9185907f301a5b2a1d90af082", Size: 26861777},
	hf("config.json", "c8a0de5ed8a083565a4319db29d0c210fda35b4d6076c2d711cae53ae00f3cb1", 2197, Both),
	hf("generation_config.json", "3479b1f44a07e41db799e22599222fee5816738036def94a39841cb9cdbb4120", 1556, Both),
	hf("preprocessor_config.json", "a6a76d28c93edb273669eb9e0b0636a2bddbb1272c3261e47b7ca6dfdbac1b8d", 339, Both),
	hf("tokenizer.json", "5eb60cec1e77aeeb6869a2bb5a8e01a84c3fe5d072d75369343021fe6f5310d0", 2405679, Both),
	hf("tokenizer_config.json", "93879c3dccdd4b976f709acd85b44778873f30c275e67026f30ca1e4c975230c", 282662, Both),
	hf("onnx/encoder_model.onnx", "1cc86302d480b061452d348638064383ab41b6f3333ddd0e423532d14edaf535", 82468078, WebGPU),
	hf("onnx/decoder_model_merged_q4.onnx", "7770ac574b71a704191ec52becac42b4a4126af26f54ed653f31c8f16af31f99", 123600371, WebGPU),
	hf("onnx/encoder_model_quantized.onnx", "6e8001198c490bbae018c0044f630c2915efb826bad957006ce36152d0ab2a10", 23201320, WASM),
	hf("onnx/decoder_model_merged_quantized.onnx", "dd4761a3f7add26afda3512abff4706920404c2517e85a9f2ff090b0c0987909", 53692803, WASM),
}

// Licenses records what each part is released under, read when this was
// added (2026-11-08). `comebye stt fetch` prints it.
const Licenses = `Speech to text runs in the browser from these parts:
  Whisper base.en weights      MIT (OpenAI, github.com/openai/whisper; the Hugging Face card
                               openai/whisper-base.en says Apache-2.0: both permit this use)
  ONNX conversion              onnx-community/whisper-base.en: the repository declares no
                               licence of its own; it is a conversion of the weights above
  transformers.js 4.3.2        Apache-2.0
  onnxruntime-web (runtime)    MIT
`

// Dir is the version directory under a state directory.
func Dir(stateDir string) string { return filepath.Join(stateDir, "stt", Version) }

// FileByPath finds a pinned file by its slash path, or nil. It is the
// server's allow-list: only these names are ever served.
func FileByPath(rel string) *File {
	for i := range files {
		if files[i].Path == rel {
			return &files[i]
		}
	}
	return nil
}

// Bytes is the download size for a backend: the runtime and tokenizer, plus
// that backend's weights.
func Bytes(backend string) int64 {
	var n int64
	for _, f := range files {
		if f.Backend == Both || f.Backend == backend {
			n += f.Size
		}
	}
	return n
}

// Total is the size of every pinned file: what `comebye stt fetch` downloads.
func Total() int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

// Status reports whether every pinned file is in dir with the right size.
// It does not hash (that is 290 MB of reading; `comebye stt status
// --verify` and fetch do). Missing lists the paths that are absent or the
// wrong size.
func Status(dir string) (installed bool, missing []string) {
	for _, f := range files {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil || !st.Mode().IsRegular() || st.Size() != f.Size {
			missing = append(missing, f.Path)
		}
	}
	return len(missing) == 0, missing
}

// HashFile returns the hex SHA-256 of a file.
func HashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify hashes every installed file and returns the paths that are absent
// or do not match.
func Verify(dir string) (bad []string) {
	for _, f := range files {
		if !fileOK(filepath.Join(dir, filepath.FromSlash(f.Path)), f) {
			bad = append(bad, f.Path)
		}
	}
	return bad
}

func fileOK(p string, f File) bool {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() != f.Size {
		return false
	}
	sum, err := HashFile(p)
	return err == nil && sum == f.SHA256
}

// ChecksumError is a download whose content is not the pinned file. The
// partial file is deleted and nothing is installed.
type ChecksumError struct {
	Path, Want, Got string
	Size, WantSize  int64
}

func (e *ChecksumError) Error() string {
	if e.Size != e.WantSize {
		return fmt.Sprintf("%s: refused: size %d, expected %d (not installed)", e.Path, e.Size, e.WantSize)
	}
	return fmt.Sprintf("%s: refused: sha256 %s, expected %s (not installed)", e.Path, e.Got, e.Want)
}

// Fetch downloads every file in list that dir does not already hold intact
// into dir, checking each against its pinned size and SHA-256 before it is
// moved into place. A file that fails the check is deleted and Fetch stops
// with a *ChecksumError, leaving the files already installed. progress, if
// set, is told about each file.
func Fetch(ctx context.Context, client *http.Client, dir string, list []File, progress func(msg string)) error {
	if client == nil {
		client = http.DefaultClient
	}
	say := func(format string, a ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, a...))
		}
	}
	for _, f := range list {
		if !safeRel(f.Path) {
			return fmt.Errorf("%s: not a relative path inside the data directory", f.Path)
		}
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		if fileOK(dst, f) {
			say("%s: already installed", f.Path)
			continue
		}
		say("%s: fetching %.1f MB from %s", f.Path, float64(f.Size)/1e6, f.URL)
		if err := fetchOne(ctx, client, dst, f); err != nil {
			return err
		}
		say("%s: ok (sha256 matches)", f.Path)
	}
	return nil
}

func safeRel(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && !strings.HasPrefix(p, "../") && p != ".."
}

func fetchOne(ctx context.Context, client *http.Client, dst string, f File) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", f.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s answered %s", f.Path, f.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	h := sha256.New()
	// One byte past the pinned size is enough to know it is too big.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, f.Size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%s: %w", f.Path, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != f.Size || got != f.SHA256 {
		return &ChecksumError{Path: f.Path, Want: f.SHA256, Got: got, Size: n, WantSize: f.Size}
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Paths returns the pinned paths, sorted (for listings and tests).
func Paths() []string {
	var p []string
	for _, f := range files {
		p = append(p, f.Path)
	}
	sort.Strings(p)
	return p
}
