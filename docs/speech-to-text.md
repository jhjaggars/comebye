# Voice input (speech to text)

A mic button in the agent prompt box and in each To do answer box. Tap it,
speak, tap again (or wait 30 s): the words are typed into the box **at the
cursor**. They are never sent for you; you read them and press Send (or
Answer).

**The transcribing happens in your browser. No audio is sent to the server or
anywhere else.** (The browser's Web Speech API is not used: Chrome sends that
audio to Google.)

## Turn it on (once per host)

```sh
comebye stt fetch      # about 290 MB into <state dir>/stt/base.en-v1/
comebye stt status     # --verify re-hashes every file
```

That is all: no flag and no restart. Until the files exist the mic buttons are
still there, and the first tap says "Voice input isn't installed on this
host" with the command to run. I chose "not installed until fetched" over a
server flag because the fetch *is* the opt-in (an admin has to run it, and
nothing is served or loaded without it); a flag would be a second switch for
the same thing.

`fetch` downloads from the pinned upstream URLs below and refuses any file
whose size or SHA-256 differs from the one pinned in `internal/stt/stt.go`:
nothing is installed from a mismatching download, and the partial file is
deleted. Run again to repair or resume; files already intact are skipped.
Only the host that fetches talks to Hugging Face and jsDelivr; browsers never
do, and neither does the page at run time.

| What | From | Licence |
|------|------|---------|
| Whisper base.en ONNX weights, tokenizer | `onnx-community/whisper-base.en` on Hugging Face, **by commit** (`51eefc0a…`) | The repository declares none; it converts OpenAI's Whisper base.en, which is MIT (github.com/openai/whisper; the Hugging Face card says Apache-2.0: both allow this) |
| ONNX runtime (`.mjs`, `.wasm`) | onnxruntime-web 1.31.0-dev.20260914-8d85527a0 via jsDelivr | MIT |
| transformers.js 4.3.2 (the JS, in `web/vendor/transformers.bundle.js`) | npm, built by `tools/stt/build.sh` | Apache-2.0 |

`comebye stt fetch` prints this at the start.

## What the user sees

The first tap checks the host, asks for the microphone, and then offers
**"Enable voice input (downloads N MB)"** (about 107 MB on the CPU, 236 MB on
a GPU). Nothing of the model or runtime is requested before that button. After
it: a progress bar and which backend, **GPU** or **CPU (slower)**. Every later
tap just listens.

States of the mic button: idle, **listening** (red, pulsing, `0:07 / 0:30`,
stops itself at 30 s, Whisper's window), **transcribing**, **downloading**, and
**error**, each with its own line above the box. Errors: microphone blocked,
no microphone, microphone in use, no secure context ("open comebye through its
https address, or localhost"), the model failed to load (tap to retry), not
installed, and a text that has nowhere to go any more (shown with a Copy
button, never dropped).

**Keyboard:** `Ctrl+Shift+Space` starts and stops, from the prompt box, an
answer box or a focused mic button. `Esc` while listening cancels and throws
the recording away.

## How it works

- `web/stt.js` (buttons, recording, inserting) records with `MediaRecorder`,
  decodes to 16 kHz mono, and posts the samples to the worker.
- `web/stt-worker.js` runs Whisper base.en through transformers.js in a Web
  Worker, so the page stays responsive. It asks for a WebGPU adapter first,
  retrying up to eight times 250 ms apart (the first request after browser
  start can return null), and falls back to WASM. If WebGPU is there but the
  model will not start on it, the page offers the CPU version (a different,
  smaller download) rather than switching silently.
- WebGPU loads an fp32 encoder and a 4-bit decoder; WASM loads int8 for both.
- `web/vendor/transformers.bundle.js` (550 KB, about 170 KB gzipped) is small
  enough to live in `web/` and so in the binary. The model files and the 27 MB
  `.wasm` do not: they are in the data directory.
- The server serves them at `/stt/base.en-v1/…` (the version is in the path,
  so a changed file set gets a new path): `Content-Type: application/wasm` for
  the runtime, `ETag` (the pinned SHA-256), `Cache-Control: public,
  max-age=31536000, immutable`, Range support. Only the pinned names are served:
  no directory listing, nothing else in the directory. `GET /api/stt` reports
  whether they are installed.

### Caching

**Cache Storage**, written by the worker (`comebye-stt-base.en-v1`), not a
service worker: transformers.js stores each model file there, and the worker
stores the `.wasm` itself. The next visit finds everything locally and makes no
download request (checked by `tools/uitest/stt.mjs`). The immutable headers
also let the browser's HTTP cache help. I did not add a fetch handler to
`sw.js`: it deliberately has none, and a fetch handler there would sit in the
path of every request. Browsers may evict Cache Storage under storage pressure
(iOS especially); then the page simply downloads again, and the Enable step
does not repeat because it was already agreed to.

### Content-Security-Policy

`script-src` gains `'wasm-unsafe-eval'` and nothing else. It allows compiling
WebAssembly, not `eval()` or `new Function()`. The runtime's `.wasm` is handed
to it as bytes (no `blob:` URL), `connect-src 'self'` and `worker-src 'self'`
already cover the rest. comebye sends no `Permissions-Policy`, so nothing blocks
the microphone (a Go test keeps it that way). `getUserMedia` needs a secure
context: the Tailscale `https://` name or `localhost`, not a plain-http
tailnet IP.

## Testing

- `go test ./...`: the pinned list, fetch (checksum mismatch refused, wrong size,
  corrupt file replaced), the `/stt/` route (headers, versioned path, no
  listing), the CLI, the CSP.
- `tools/uitest/run.sh` runs `tools/uitest/stt.mjs`, with Chrome's fake audio
  device playing `tools/uitest/fixtures/speech.wav` as the microphone. The "not
  installed" and secure-context checks always run. The rest (button states,
  transcript inserted and not sent, mic denied, the Enable step, a later visit
  downloading nothing, the To do box, the shortcut) need the real files:
  `comebye stt fetch --state-dir DIR`, then `STT_DATA=DIR tools/uitest/run.sh`
  (`STT_GPU=1` for the GPU). They are skipped, and say so, without it; CI does
  not download 290 MB.

## Rebuilding the JS bundle

`tools/stt/build.sh` (pinned in `tools/stt/package.json` and its lockfile; the
output is deterministic). See `web/vendor/README.md`.
