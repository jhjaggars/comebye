// Speech to text, off the main thread. A module Worker: stt.js starts it on
// the first tap of a mic button and talks to it with postMessage.
//
// Whisper base.en through transformers.js (web/vendor/transformers.bundle.js,
// built from the pinned npm packages: see web/vendor/README.md). Every file
// comes from this server's /stt/<version>/ (put there by `comebye stt fetch`);
// nothing is fetched from Hugging Face or a CDN, and no audio leaves the
// machine: the samples go in, the text comes out, here.
//
// Messages in:
//   {type:"probe"}                              which backend can this browser run?
//   {type:"load", base, backend, bytes}         download (or read from the cache) and start the model
//   {type:"transcribe", id, audio:Float32Array} 16 kHz mono samples
// Messages out:
//   {type:"probed", backend, adapter}
//   {type:"progress", loaded, total}            bytes of the whole download, so far
//   {type:"ready", backend}
//   {type:"result", id, text, ms}
//   {type:"error", stage, id?, message}
"use strict";

import { pipeline, env } from "./vendor/transformers.bundle.js";

const send = (m) => self.postMessage(m);

// A WebGPU adapter. The first requestAdapter() after the browser starts can
// answer null where a later one succeeds (seen on Chrome 155 with an NVIDIA
// GPU), so ask up to eight times, 250 ms apart.
async function gpuAdapter() {
  if (!self.navigator || !navigator.gpu) return null;
  for (let i = 0; i < 8; i++) {
    try {
      const a = await navigator.gpu.requestAdapter({ powerPreference: "high-performance" });
      if (a) return a;
    } catch {}
    await new Promise((r) => setTimeout(r, 250));
  }
  return null;
}

async function probe() {
  const a = await gpuAdapter();
  let adapter = "";
  if (a) {
    const i = a.info || {};
    adapter = [i.vendor, i.architecture].filter(Boolean).join(" ");
  }
  send({ type: "probed", backend: a ? "webgpu" : "wasm", adapter });
}

// Which weights each backend loads (they match the files `comebye stt fetch`
// installs): WebGPU wants an fp32 encoder and a 4-bit decoder; the CPU
// path wants int8 for both.
const DTYPE = {
  webgpu: { encoder_model: "fp32", decoder_model_merged: "q4" },
  wasm: { encoder_model: "q8", decoder_model_merged: "q8" },
};

let asr = null;

// The ONNX runtime's WebAssembly, read through Cache Storage with its own
// progress (transformers.js reports the model files but not this one).
// Handed to the runtime as bytes, so no blob: URL is needed and the CSP
// stays as it is.
async function runtimeBinary(url, cache, onBytes) {
  let hit = cache && (await cache.match(url));
  if (hit) {
    const b = await hit.arrayBuffer();
    onBytes(b.byteLength);
    return b;
  }
  const res = await fetch(url);
  if (!res.ok) throw new Error(`${url}: ${res.status}`);
  const chunks = [];
  let n = 0;
  const reader = res.body.getReader();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    n += value.length;
    onBytes(n);
  }
  const buf = new Uint8Array(n);
  let at = 0;
  for (const c of chunks) {
    buf.set(c, at);
    at += c.length;
  }
  if (cache) {
    try {
      await cache.put(url, new Response(buf, { headers: { "Content-Type": "application/wasm" } }));
    } catch {} // storage full or refused: it is only a cache
  }
  return buf.buffer;
}

async function load({ base, backend, bytes, version }) {
  const cacheName = "comebye-stt-" + version;
  env.allowRemoteModels = false; // nothing from huggingface.co
  env.allowLocalModels = true;
  env.localModelPath = base + "models/";
  env.useBrowserCache = true; // Cache Storage, so a later visit skips the download
  env.cacheKey = cacheName;
  env.useWasmCache = false;
  const onnx = env.backends.onnx;
  onnx.wasm.numThreads = 1; // the app is not cross-origin isolated
  onnx.wasm.wasmPaths = {
    mjs: base + "ort/ort-wasm-simd-threaded.asyncify.mjs",
    wasm: base + "ort/ort-wasm-simd-threaded.asyncify.wasm",
  };
  onnx.webgpu = Object.assign(onnx.webgpu || {}, { powerPreference: "high-performance" });

  let cache = null;
  try { cache = await caches.open(cacheName); } catch {}
  let rt = 0;
  const files = {};
  const report = () => {
    let loaded = rt;
    for (const v of Object.values(files)) loaded += v;
    send({ type: "progress", loaded: Math.min(loaded, bytes), total: bytes });
  };
  onnx.wasm.wasmBinary = await runtimeBinary(onnx.wasm.wasmPaths.wasm, cache, (n) => { rt = n; report(); });

  if (backend === "webgpu" && !(await gpuAdapter())) throw new Error("no WebGPU adapter");
  asr = await pipeline("automatic-speech-recognition", "whisper-base.en", {
    device: backend,
    dtype: DTYPE[backend],
    progress_callback: (p) => {
      if (p.status === "progress" && p.file) {
        files[p.file] = p.loaded;
        report();
      }
    },
  });
  send({ type: "ready", backend });
}

// Whisper answers silence with a bracketed tag ("[BLANK_AUDIO]", "(silence)").
function clean(text) {
  const t = String(text || "").trim();
  if (/^[\[(][^\])]*[\])]$/.test(t)) return "";
  return t;
}

async function transcribe({ id, audio }) {
  const t0 = performance.now();
  const r = await asr(audio);
  send({ type: "result", id, text: clean(r.text), ms: Math.round(performance.now() - t0) });
}

self.onmessage = async (ev) => {
  const m = ev.data || {};
  try {
    if (m.type === "probe") await probe();
    else if (m.type === "load") await load(m);
    else if (m.type === "transcribe") await transcribe(m);
  } catch (e) {
    send({ type: "error", stage: m.type, id: m.id, message: String((e && e.message) || e) });
  }
};
